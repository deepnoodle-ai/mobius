//go:build linux

package main

import "syscall"

// protectWorkerProcess marks the worker non-dumpable, which makes its
// /proc/<pid> files (environ, mem) readable only by root.
func protectWorkerProcess() {
	_, _, _ = syscall.RawSyscall(syscall.SYS_PRCTL, syscall.PR_SET_DUMPABLE, 0, 0)
}
