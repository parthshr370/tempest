//go:build darwin || (freebsd && (amd64 || arm64 || riscv64))

package dap

import (
	"syscall"
	"unsafe"
)

// The kern.proc.pid.<pid> sysctl name, shared by Darwin and FreeBSD.
const (
	ctlKern     = 1  // CTL_KERN
	kernProc    = 14 // KERN_PROC
	kernProcPID = 1  // KERN_PROC_PID
)

// kinfoProc reads the struct kinfo_proc of pid. It reports false when the
// process doesn't exist or the kernel's record isn't the size this build
// decodes.
func kinfoProc(pid, size int) ([]byte, bool) {
	mib := [4]int32{ctlKern, kernProc, kernProcPID, int32(pid)}
	info := make([]byte, size)
	length := uintptr(size)
	_, _, errno := syscall.Syscall6(syscall.SYS___SYSCTL, uintptr(unsafe.Pointer(&mib[0])), uintptr(len(mib)), uintptr(unsafe.Pointer(&info[0])), uintptr(unsafe.Pointer(&length)), 0, 0)
	if errno != 0 || length != uintptr(size) {
		return nil, false
	}
	return info, true
}
