package question

import (
	"syscall"
	"unsafe"
)

func terminalReady(descriptor int, ready *syscall.FdSet, timeout *syscall.Timeval) (bool, error) {
	err := syscall.Select(descriptor+1, ready, nil, nil, timeout)
	wordBits := int(unsafe.Sizeof(ready.Bits[0])) * 8
	return ready.Bits[descriptor/wordBits]&(1<<uint(descriptor%wordBits)) != 0, err
}
