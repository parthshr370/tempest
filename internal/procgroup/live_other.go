//go:build !darwin

package procgroup

// hasLiveMember reports true: outside Darwin, signalling a group of zombies
// succeeds or fails with ESRCH, so an EPERM always comes from a live member.
func hasLiveMember(int) bool { return true }
