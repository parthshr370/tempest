package jobs

import (
	"os"
	"strconv"
	"syscall"
	"unsafe"
)

// unlockPTY is unlockpt and ptsname for a Linux devpts master; devpts
// already grants the terminal to its opener.
func unlockPTY(master *os.File) (string, error) {
	var locked int32
	if err := ioctl(master, syscall.TIOCSPTLCK, unsafe.Pointer(&locked)); err != nil {
		return "", err
	}
	var number uint32
	if err := ioctl(master, syscall.TIOCGPTN, unsafe.Pointer(&number)); err != nil {
		return "", err
	}
	return "/dev/pts/" + strconv.FormatUint(uint64(number), 10), nil
}
