//go:build aix || darwin || dragonfly || freebsd || linux

package common

import "syscall"

// freeBytes reports the bytes available to an unprivileged process on the
// filesystem holding path.
//
// Bavail, not Bfree: the blocks the superuser is holding back are not space
// this project's build can write into, and counting them would overstate
// fitness on exactly the machine that is about to run out.
//
// The build tag lists the platforms whose standard `syscall` package spells
// this exact call and these exact fields, and it is spelled out rather than
// written as `!windows`. "The BSDs" is not one answer: NetBSD has no Statfs in
// `syscall` at all and OpenBSD's Statfs_t names the same two fields F_bavail
// and F_bsize, while Solaris, illumos, plan9 and the wasm ports have none of
// it. A negation would stop this whole package compiling on all of them —
// taking the other five gates, verify and the guard with it. What they get
// instead is platform_nostatfs.go.
func freeBytes(path string) (int64, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, err
	}
	return int64(st.Bavail) * int64(st.Bsize), nil
}
