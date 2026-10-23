package jobs

import (
	"bytes"
	"errors"
	"os"
	"syscall"
	"unsafe"
)

// unlockPTY is grantpt, unlockpt and ptsname for a darwin master.
func unlockPTY(master *os.File) (string, error) {
	if err := ioctl(master, syscall.TIOCPTYGRANT, nil); err != nil {
		return "", err
	}
	if err := ioctl(master, syscall.TIOCPTYUNLK, nil); err != nil {
		return "", err
	}
	// TIOCPTYGNAME fills a 128-byte buffer with the NUL-terminated name.
	var name [128]byte
	if err := ioctl(master, syscall.TIOCPTYGNAME, unsafe.Pointer(&name[0])); err != nil {
		return "", err
	}
	end := bytes.IndexByte(name[:], 0)
	if end <= 0 {
		return "", errors.New("pty: TIOCPTYGNAME returned no terminal name")
	}
	return string(name[:end]), nil
}
