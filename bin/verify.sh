#!/usr/bin/env bash
# verify.sh — flow (OSS SDK) module gate: build + test.
#
# Run standalone when working on flow alone; also invoked by the superproject's
# bin/verify.sh so the tracker's combined gate covers this module. flow is an
# independent module — its changes are gated by THIS script, never by the
# tracker's verify (see docs/task-resolution.md §4.3a).
set -uo pipefail
cd "$(dirname "$0")/.." # module root, regardless of caller cwd

failed=0

echo "flow: building..."
if ! go build ./...; then failed=1; fi

echo "flow: vetting..."
if ! go vet ./...; then failed=1; fi

echo "flow: testing..."
if ! go test ./...; then failed=1; fi

if [ "$failed" -eq 0 ]; then
    echo "✅ OK to commit"
else
    echo "❌ Verify FAILED: not safe to commit"
    exit 1
fi
