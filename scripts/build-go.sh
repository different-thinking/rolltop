#!/bin/sh
# Builds the server binary and every plugin backend, then throws the Go caches
# away in the same layer.
#
# The layer part is the point. Kaniko snapshots the filesystem after each RUN
# and holds the resulting layers in memory, so a stage split into
# download / build / build-plugins retains all three: ~760 MB of module cache,
# ~610 MB of build cache, and the outputs, against a builder capped at 4 GB for
# the whole build. Doing it in one step and ending with `go clean` leaves a
# layer holding /out and the sources instead of the caches that produced them.
#
# The cost is that `go mod download` no longer has a layer of its own to be
# cached across builds. That is free today because the builder runs without
# `--cache`, and it is deliberately reversible: split this back into separate
# RUN steps if caching is ever switched on and memory is no longer the binding
# constraint.
set -eu

out="${1:?usage: build-go.sh <output-dir>}"

# Resolved before anything is built, because the cleanup at the bottom must not
# invoke `go` again. Where the toolchain itself lives under GOMODCACHE — which
# is how it is installed outside this image — `go clean -modcache` deletes the
# toolchain it is running from and the next `go` call downloads it back, which
# is 214 MB put right back into the layer this is trying to empty.
gocache="$(go env GOCACHE)"
gomodcache="$(go env GOMODCACHE)"

go mod download

CGO_ENABLED=1 GOOS=linux go build -trimpath \
	-ldflags="-s -w \
		-X rolltop/backend/buildinfo.Version=${ROLLTOP_VERSION:-latest} \
		-X rolltop/backend/buildinfo.BuildDate=${ROLLTOP_BUILD_DATE:-} \
		-X rolltop/backend/buildinfo.Commit=${ROLLTOP_COMMIT:-}" \
	-o "${out}/rolltop" ./cmd/rolltop

# How many plugin backends link at once is bounded by the builder's *memory*,
# not its cores. A Go link holds its output in memory, and `language_search`
# links a 124 MB embedded model set, so a few of those together is gigabytes.
# Overshooting a capped builder does not make the build slow, it makes the
# kernel kill it.
jobs="$(nproc 2>/dev/null || echo 2)"
if [ "$jobs" -gt 4 ]; then
	jobs=4
fi
limit=""
if [ -r /sys/fs/cgroup/memory.max ]; then
	limit="$(cat /sys/fs/cgroup/memory.max)"
elif [ -r /sys/fs/cgroup/memory/memory.limit_in_bytes ]; then
	limit="$(cat /sys/fs/cgroup/memory/memory.limit_in_bytes)"
fi
case "$limit" in
"" | max | *[!0-9]*) limit="" ;;
esac
if [ -n "$limit" ]; then
	bymem=$((limit / 2147483648))
	if [ "$bymem" -lt 1 ]; then
		bymem=1
	fi
	if [ "$jobs" -gt "$bymem" ]; then
		jobs="$bymem"
	fi
fi
echo "building plugin backends, $jobs at a time"

build_one() {
	plugin="$(basename "$(dirname "$1")")"
	mkdir -p "${out}/plugins/${plugin}/backend"
	CGO_ENABLED=1 GOOS=linux go build -buildmode=plugin -trimpath -ldflags="-s -w" \
		-o "${out}/plugins/${plugin}/backend/${plugin}.so" "./$1"
}

# Derived from the directories in the build context, not a hand-maintained
# list, so a new plugin backend cannot be silently left out of the image.
#
# The first is built alone deliberately: `-buildmode=plugin` compiles packages
# differently from the binary above, so its cache is cold, and starting all of
# them together would have every process compiling the same shared packages at
# once instead of reusing the first one's work.
status=0
pids=""
running=0
first=1
for backend in plugins/*/backend; do
	if [ "$first" = 1 ]; then
		first=0
		build_one "$backend"
		continue
	fi
	build_one "$backend" &
	pids="$pids $!"
	running=$((running + 1))
	if [ "$running" -ge "$jobs" ]; then
		for pid in $pids; do wait "$pid" || status=1; done
		pids=""
		running=0
	fi
done
for pid in $pids; do wait "$pid" || status=1; done
[ "$status" = 0 ] || exit "$status"

want="$(ls -d plugins/*/backend | wc -l)"
got="$(ls "${out}"/plugins/*/backend/*.so 2>/dev/null | wc -l)"
if [ "$want" != "$got" ]; then
	echo "expected $want plugin backends, built $got" >&2
	exit 1
fi

# Same layer as the build that filled them, or the snapshot keeps them. Guarded
# because an empty value here would be `rm -rf /`.
for cache in "$gocache" "$gomodcache"; do
	case "$cache" in
	"" | "/") echo "refusing to remove cache path '$cache'" >&2; exit 1 ;;
	esac
	chmod -R u+w "$cache" 2>/dev/null || true
	rm -rf "$cache"
done
