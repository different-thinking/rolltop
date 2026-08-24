# Build-time note for whoever configures the builder, because the two largest
# remaining costs cannot be fixed from inside this file.
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
COPY go.mod go.sum ./
RUN go mod download

# Only the trees the Go build reads. The previous `COPY . .` pulled in
# `frontend/`, `android/`, `docs/` and the rest, so editing a stylesheet
# invalidated every layer below and recompiled the whole backend. `plugins/`
# still carries its own frontend sources, so a plugin's UI change does rebuild
# the backend; splitting that too needs a directory layout change, not a
# Dockerfile one.
COPY backend ./backend
COPY cmd ./cmd
COPY internal ./internal
COPY plugins ./plugins

# Below the source copies on purpose: these change on every commit, and above
# them they would invalidate `go mod download` and the source layers each time.
ARG ROLLTOP_VERSION=latest
ARG ROLLTOP_BUILD_DATE=
ARG ROLLTOP_COMMIT=
RUN CGO_ENABLED=1 GOOS=linux go build -trimpath -ldflags="-s -w -X rolltop/backend/buildinfo.Version=${ROLLTOP_VERSION} -X rolltop/backend/buildinfo.BuildDate=${ROLLTOP_BUILD_DATE} -X rolltop/backend/buildinfo.Commit=${ROLLTOP_COMMIT}" -o /out/rolltop ./cmd/rolltop

# Derived from the directories in the build context, not a hand-maintained
# list, so a new plugin backend cannot be silently left out of the image.
#
# Built a few at a time rather than one after another: each `.so` links the
# whole shared dependency graph, and linking is the part that does not use the
# machine. The first one is built alone deliberately — `-buildmode=plugin`
# compiles packages differently from the main binary above, so its cache is
# cold, and starting all of them together would have every process compiling
# the same shared packages at once instead of reusing the first one's work.
RUN set -eu; \
	jobs="$(nproc 2>/dev/null || echo 2)"; \
	if [ "$jobs" -gt 4 ]; then jobs=4; fi; \
	build_one() { \
		plugin="$(basename "$(dirname "$1")")"; \
		mkdir -p "/out/plugins/${plugin}/backend"; \
		CGO_ENABLED=1 GOOS=linux go build -buildmode=plugin -trimpath -ldflags="-s -w" -o "/out/plugins/${plugin}/backend/${plugin}.so" "./$1"; \
	}; \
	status=0; pids=""; running=0; first=1; \
	for backend in plugins/*/backend; do \
		if [ "$first" = 1 ]; then \
			first=0; \
			build_one "$backend"; \
			continue; \
		fi; \
		build_one "$backend" & \
		pids="$pids $!"; \
		running=$((running + 1)); \
		if [ "$running" -ge "$jobs" ]; then \
			for pid in $pids; do wait "$pid" || status=1; done; \
			pids=""; running=0; \
		fi; \
	done; \
	for pid in $pids; do wait "$pid" || status=1; done; \
	[ "$status" = 0 ] || exit "$status"; \
	want="$(ls -d plugins/*/backend | wc -l)"; \
	got="$(ls /out/plugins/*/backend/*.so | wc -l)"; \
	if [ "$want" != "$got" ]; then \
		echo "expected $want plugin backends, built $got" >&2; \
		exit 1; \
	fi

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
