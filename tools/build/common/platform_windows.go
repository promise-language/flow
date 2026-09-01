//go:build windows

package common

import (
	"syscall"
	"unsafe"
)

// Windows has no statfs. GetDiskFreeSpaceExW answers the same question, and its
// first out-parameter — the bytes available to the calling user, with quota and
// reserve already deducted — is the same quantity as Bavail on the unix side.
// Both platforms therefore report space this project's build can write into,
// rather than space that merely exists.
//
// Windows is covered rather than stubbed because make.cmd is committed: a
// required gate that refuses on a platform the build tooling claims to support
// is a default that cannot run in the tree shipping it.
var getDiskFreeSpaceExW = syscall.NewLazyDLL("kernel32.dll").NewProc("GetDiskFreeSpaceExW")

// freeBytes reports the bytes available to the calling user on the volume
// holding path.
func freeBytes(path string) (int64, error) {
	dir, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return 0, err
	}
	// Find before Addr: LazyProc panics when the entry point is missing, and a
	// gate that panicked would leave nothing parseable on stdout — which reads
	// as a gate that died rather than as a machine that could not be measured.
	if err := getDiskFreeSpaceExW.Find(); err != nil {
		return 0, err
	}
	// SyscallN rather than LazyProc.Call: it is the one that keeps the pointers
	// alive across the call, so the out-parameters cannot be moved out from
	// under the kernel while it is writing to them.
	var availableToCaller, totalBytes, totalFree uint64
	r, _, errno := syscall.SyscallN(getDiskFreeSpaceExW.Addr(),
		uintptr(unsafe.Pointer(dir)),
		uintptr(unsafe.Pointer(&availableToCaller)),
		uintptr(unsafe.Pointer(&totalBytes)),
		uintptr(unsafe.Pointer(&totalFree)),
	)
	if r == 0 {
		return 0, errno
	}
	return int64(availableToCaller), nil
}
