package dap

import "encoding/binary"

// Offsets in Darwin's 64-bit struct kinfo_proc, whose first member is
// struct extern_proc.
const (
	kinfoProcSize       = 648 // sizeof(struct kinfo_proc)
	kinfoStartSecOffset = 0   // kp_proc.p_starttime.tv_sec, int64
	kinfoStartUsOffset  = 8   // kp_proc.p_starttime.tv_usec, int32
	kinfoPIDOffset      = 40  // kp_proc.p_pid, int32
)

// processStartTime reads the process's start time from kern.proc.pid.
func processStartTime(pid int) (processStart, bool) {
	info, ok := kinfoProc(pid, kinfoProcSize)
	if !ok || int32(binary.NativeEndian.Uint32(info[kinfoPIDOffset:])) != int32(pid) {
		return processStart{}, false
	}
	seconds := int64(binary.NativeEndian.Uint64(info[kinfoStartSecOffset:]))
	microseconds := int64(int32(binary.NativeEndian.Uint32(info[kinfoStartUsOffset:])))
	return processStart{seconds, microseconds}, true
}
