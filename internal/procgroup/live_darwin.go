package procgroup

import (
	"encoding/binary"
	"syscall"
	"unsafe"
)

// The kern.proc.pgrp.<pgid> sysctl and the fields of Darwin's 64-bit struct
// kinfo_proc (whose first member is struct extern_proc) that tell a live
// process from a zombie or an exiting one.
const (
	ctlKern         = 1      // CTL_KERN
	kernProc        = 14     // KERN_PROC
	kernProcPgrp    = 2      // KERN_PROC_PGRP
	kinfoProcSize   = 648    // sizeof(struct kinfo_proc)
	kinfoFlagOffset = 32     // kp_proc.p_flag, int32
	kinfoStatOffset = 36     // kp_proc.p_stat, char
	statZombie      = 5      // SZOMB
	flagExiting     = 0x2000 // P_WEXIT
)

// hasLiveMember reports whether group pgid has a member that is neither a
// zombie nor exiting, the members Darwin's killpg skips. When the kernel's
// answer can't be read, it reports true so the caller keeps the EPERM.
func hasLiveMember(pgid int) bool {
	mib := [4]int32{ctlKern, kernProc, kernProcPgrp, int32(pgid)}
	var length uintptr
	if sysctl(mib[:], nil, &length) != nil {
		return true
	}
	// The group can't gain members once none is live, so this headroom only
	// matters for a group that still has one.
	length += 4 * kinfoProcSize
	info := make([]byte, length)
	if err := sysctl(mib[:], info, &length); err != nil || length%kinfoProcSize != 0 {
		return true
	}
	for record := info[:length]; len(record) > 0; record = record[kinfoProcSize:] {
		flag := binary.NativeEndian.Uint32(record[kinfoFlagOffset:])
		if record[kinfoStatOffset] != statZombie && flag&flagExiting == 0 {
			return true
		}
	}
	return false
}

func sysctl(mib []int32, out []byte, length *uintptr) error {
	var buffer unsafe.Pointer
	if len(out) > 0 {
		buffer = unsafe.Pointer(&out[0])
	}
	_, _, errno := syscall.Syscall6(syscall.SYS___SYSCTL, uintptr(unsafe.Pointer(&mib[0])), uintptr(len(mib)), uintptr(buffer), uintptr(unsafe.Pointer(length)), 0, 0)
	if errno != 0 {
		return errno
	}
	return nil
}
