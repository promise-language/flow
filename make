#!/usr/bin/env bash
# Bootstrap trampoline. Compiles every dev tool into bin/ via the meta-builder.
#
# This file is committed shell text — nothing builds it. It runs the
# meta-builder via 'go run', which needs only the Go toolchain (no pre-built
# binary), and that meta-builder compiles every other tool into bin/.
#
# The inner cd resolves $0 via its own directory, so ./make works from any cwd,
# and pins go run's working directory to <repo>/tools/build — which is how the
# meta-builder learns the absolute repo root.
set -euo pipefail
repo="$(cd "$(dirname "$0")" && pwd)"

go run -C "$repo/tools/build" ./cmd/make "$@"

# Optional local post-build hook — not committed, not required, and absent for
# every contributor. It exists so a working environment can provision itself
# after a build (syncing sibling tooling, refreshing generated config) without
# this repository carrying any knowledge of what that environment is.
#
# Deliberately one-directional: this repo calls out, and nothing calls back in.
# A hook that re-entered this script would loop.
#
# Interactive only — a terminal on stderr — so builds under CI, in a pipeline,
# or from another tool behave exactly as they did before this existed.
#
# A hook failure NEVER fails the build. The build above already succeeded;
# reporting failure here would attribute someone else's problem to it.
if [ -t 2 ] && [ -x "$repo/make.local" ]; then
	"$repo/make.local" "$@" || {
		printf '\n./make: local hook failed (the build itself succeeded)\n' >&2
		printf '        re-run it alone with: %s\n\n' "$repo/make.local" >&2
	}
fi
