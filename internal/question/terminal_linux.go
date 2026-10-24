package question

import "syscall"

func terminalReady(descriptor int, ready *syscall.FdSet, timeout *syscall.Timeval) (bool, error) {
	count, err := syscall.Select(descriptor+1, ready, nil, nil, timeout)
	return count > 0, err
}
