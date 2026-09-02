//go:build !windows && !aix && !darwin && !dragonfly && !freebsd && !linux

package common

import (
	"fmt"
	"runtime"
)

// freeBytes has no implementation here. NetBSD, OpenBSD, Solaris, illumos,
// plan9 and the wasm ports reach a filesystem's free space through calls their
// standard `syscall` package either does not export or spells differently, and
// taking a dependency on the build tooling to reach them would be paid for by
// every host that builds it.
//
// It refuses rather than being absent, and that is the whole point of the file:
// with no definition at all this package would not compile on those platforms,
// so nothing else in it — the other five gates, verify, the guard — could be
// built there either. A refusal costs them `fit` and nothing more.
//
// It refuses rather than reporting a number, because there is no honest one. A
// zero would report a full disk on every such machine, and a large constant
// would report fitness that was never measured.
func freeBytes(path string) (int64, error) {
	return 0, fmt.Errorf("free space at %s is not measurable from the standard library on %s/%s",
		path, runtime.GOOS, runtime.GOARCH)
}
