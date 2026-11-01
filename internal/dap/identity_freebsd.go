//go:build freebsd && (amd64 || arm64 || riscv64)

package dap

import "encoding/binary"

// Offsets in FreeBSD's LP64 struct kinfo_proc (sys/user.h).
const (
	kinfoProcSize       = 1088 // KINFO_PROC_SIZE
	kinfoStructSizeOff  = 0    // ki_structsize, int32
	kinfoPIDOffset      = 72   // ki_pid, int32
	kinfoStartSecOffset = 336  // ki_start.tv_sec, int64
	kinfoStartUsOffset  = 344  // ki_start.tv_usec, int64
)

// processStartTime reads the process's start time from kern.proc.pid.
func processStartTime(pid int) (processStart, bool) {
	info, ok := kinfoProc(pid, kinfoProcSize)
	if !ok || binary.NativeEndian.Uint32(info[kinfoStructSizeOff:]) != kinfoProcSize || int32(binary.NativeEndian.Uint32(info[kinfoPIDOffset:])) != int32(pid) {
		return processStart{}, false
	}
	seconds := int64(binary.NativeEndian.Uint64(info[kinfoStartSecOffset:]))
	microseconds := int64(binary.NativeEndian.Uint64(info[kinfoStartUsOffset:]))
	return processStart{seconds, microseconds}, true
}
