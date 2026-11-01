// Package procgroup signals the process groups that tempest starts with
// Setpgid, so a child's descendants share its fate.
package procgroup

import (
	"errors"
	"os"
	"syscall"
)

// Signal sends sig to every process in group pgid. It returns
// os.ErrProcessDone when the group has no live member left to signal: the
// group is gone (ESRCH), or Darwin refused with EPERM because every member is
// a zombie or already exiting. Darwin reports EPERM for such a group until each
// zombie's parent reaps it, and that parent is often not us. EPERM while a
// live member remains is a real permission error and is returned as is.
// Signal 0 probes the group, but only Darwin's EPERM inspection recognizes a
// group of zombie or exiting members as done. Elsewhere the native result
// stands: on Linux, kill(-pgid, 0) succeeds while unreaped zombies remain, so
// nil there does not prove a live member.
func Signal(pgid int, sig syscall.Signal) error {
	// kill(0) names the caller's own group and kill(-1) every process it may
	// signal; neither is a child's group.
	if pgid <= 1 {
		return syscall.EINVAL
	}
	err := syscall.Kill(-pgid, sig)
	if errors.Is(err, syscall.ESRCH) || errors.Is(err, syscall.EPERM) && !hasLiveMember(pgid) {
		return os.ErrProcessDone
	}
	return err
}
