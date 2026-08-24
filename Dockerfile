# Build-time note for whoever configures the builder, because the two largest
# remaining costs cannot be fixed from inside this file.
#
# If the build is being OOM killed, start with `--compressed-caching=false`.
# Kaniko holds layer contents in memory to compress them, and this image's
# plugin layer is ~324 MB of `.so` files, so the default costs roughly that
# again in RSS at the worst moment. A killed build retries and the attempts
# share one log stream, which reads convincingly like one very slow build
# rather than three dead ones.
#
# Nothing here is cached between builds unless the builder is told to cache.
# Under Kaniko that means `--cache=true --cache-repo=<registry>/rolltop-cache`;
# without it `apk add build-base`, `npm ci` and `go mod download` are paid in
# full every time, and the layer ordering below — dependencies above sources,
# ARGs below them — buys nothing. `--snapshot-mode=redo` is worth setting too:
# Kaniko snapshots the whole filesystem after each RUN, and on this image that
# is a large share of the build with the default mode.
#
# Kaniko also builds stages one after another. The `frontend` and `build`
# stages below share nothing and would otherwise overlap, so a builder that
# runs stages concurrently (BuildKit, `docker buildx`) removes the shorter of
# the two from the total on its own.

FROM node:20-alpine AS frontend

WORKDIR /src
COPY package.json package-lock.json ./
RUN npm ci
COPY tsconfig.json vite.config.ts vite.plugins.config.ts ./
COPY scripts ./scripts
COPY frontend ./frontend
COPY plugins ./plugins
# `ROLLTOP_BUILD_SOURCEMAPS=0` because nothing serves a sourcemap and the image
# build pays for one three times over: generating it, snapshotting the layer,
# and pushing it. They outweigh the bundles they describe — the attachment
# preview bundle's map alone is 10 MB — and the plugin asset route hands out
# whole bundle directories, so shipping them also published the sources.
#
# Assembling the deployable plugin tree here, in the same layer as the build
# that produced it, keeps a snapshot the final stage would otherwise pay for.
RUN ROLLTOP_BUILD_SOURCEMAPS=0 npm run build \
	&& node scripts/assemble-plugin-dist.mjs /out/plugins

FROM golang:1.25-alpine AS build
RUN apk add --no-cache build-base

WORKDIR /src

# Only the trees the Go build reads. The previous `COPY . .` pulled in
# `frontend/`, `android/`, `docs/` and the rest, so editing a stylesheet
# invalidated every layer below and recompiled the whole backend. `plugins/`
# still carries its own frontend sources, so a plugin's UI change does rebuild
# the backend; splitting that too needs a directory layout change, not a
# Dockerfile one.
COPY go.mod go.sum ./
COPY scripts/build-go.sh ./scripts/
COPY backend ./backend
COPY cmd ./cmd
COPY internal ./internal
COPY plugins ./plugins

# Below the source copies on purpose: these change on every commit, and above
# them they would invalidate the source layers each time.
ARG ROLLTOP_VERSION=latest
ARG ROLLTOP_BUILD_DATE=
ARG ROLLTOP_COMMIT=

# Download, both builds and the cache cleanup are one step because Kaniko keeps
# a layer per RUN in memory: split up, this stage retained ~760 MB of module
# cache and ~610 MB of build cache for the rest of the build. See the script
# for what that trades away.
RUN sh scripts/build-go.sh /out

FROM alpine:3.22

RUN apk add --no-cache ca-certificates tzdata poppler-utils antiword \
	&& addgroup -S -g 10001 rolltop \
	&& adduser -S -D -H -u 10001 -G rolltop -s /sbin/nologin rolltop \
	&& mkdir -p /data \
	&& chown -R rolltop:rolltop /data

WORKDIR /app
COPY --from=build /out/rolltop /usr/local/bin/rolltop
COPY --from=frontend /src/frontend/dist /app/frontend/dist
# The assembled tree, not `/src/plugins`: manifests, the bundle directories the
# asset route serves, and the migrations the manifest loader applies at startup.
# The `.tsx`, `.go` and `.scss` sources it leaves behind were never read.
COPY --from=frontend /out/plugins /app/plugins
COPY --from=build /out/plugins /app/plugins

USER rolltop
EXPOSE 8080
VOLUME ["/data"]

ENV ROLLTOP_ADDR=:8080
ENV ROLLTOP_DATA_DIR=/data

ENTRYPOINT ["/usr/local/bin/rolltop"]
