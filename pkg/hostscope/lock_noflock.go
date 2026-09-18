//go:build !(darwin || dragonfly || freebsd || illumos || linux || netbsd || openbsd)

package hostscope

import (
	"context"
	"fmt"
	"os"
	"runtime"
)

// lockExclusive has no implementation here. Windows, plan9, the wasm ports,
// solaris and aix either have no advisory lock in the standard library or spell
// it through a call `syscall` does not export, and reaching them would cost a
// dependency paid for by every machine that builds this.
//
// IT REFUSES RATHER THAN BEING ABSENT, and that is the whole point of the file:
// with no definition at all this package would not compile on those platforms,
// so nothing that imports it could be built there either — which would cost
// them the SDK, not just host-scoped gates.
//
// IT REFUSES RATHER THAN RETURNING nil, which is the tempting reading and the
// expensive one. A nil here is "the exclusion was taken" said by something that
// took nothing: every heavy gate on the machine would run beside every other,
// and docs/gates-and-commands.md § Two scopes forbids exactly that — a party
// that cannot take the exclusion does not run the measurement, it refuses and
// names what it could not take. A refusal costs those platforms the gates a
// project declared host-scoped and nothing else.
func lockExclusive(_ context.Context, f *os.File) (contended bool, err error) {
	f.Close()
	return false, fmt.Errorf("hostscope: the host-scope exclusion cannot be held on %s/%s — no advisory file lock is reachable from the standard library there",
		runtime.GOOS, runtime.GOARCH)
}

// unlock is unreachable: nothing on these platforms ever holds the exclusion,
// because lockExclusive refuses before a caller can. It exists so the package
// compiles, and it returns the same refusal rather than nil so that a future
// caller that found a way around the acquire path is not told it released
// something.
func unlock(_ *os.File) error {
	return fmt.Errorf("hostscope: the host-scope exclusion cannot be held on %s/%s, so nothing here holds one to release",
		runtime.GOOS, runtime.GOARCH)
}
