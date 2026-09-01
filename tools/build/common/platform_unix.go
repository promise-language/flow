//go:build !windows

package common

import "syscall"

// freeBytes reports the bytes available to an unprivileged process on the
// filesystem holding path.
//
// Bavail, not Bfree: the blocks the superuser is holding back are not space
// this project's build can write into, and counting them would overstate
// fitness on exactly the machine that is about to run out.
//
// syscall.Statfs exists on Linux, macOS and the BSDs. The platforms where it
// does not (plan9, js/wasip1, solaris) cannot host tooling that shells out to
// `go build`, so there is no third file.
func freeBytes(path string) (int64, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, err
	}
	return int64(st.Bavail) * int64(st.Bsize), nil
}
