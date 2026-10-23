//go:build darwin || linux

package jobs

import (
	"errors"
	"os"
	"syscall"
	"unsafe"
)

const ptySupported = true

// The size every terminal starts with absent a resize: 24 rows of 80 columns.
const (
	ptyRows    = 24
	ptyColumns = 80
)

// windowSize is struct winsize from <sys/ttycom.h> and <asm/termios.h>.
type windowSize struct {
	rows, columns, xPixels, yPixels uint16
}

// openPTY allocates a pseudo-terminal: master is the job worker's end for
// output and input, tty the terminal the process gets as stdin, stdout and
// stderr. Neither becomes the harness's controlling terminal.
func openPTY() (master, tty *os.File, err error) {
	master, err = os.OpenFile("/dev/ptmx", os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		return nil, nil, err
	}
	name, err := unlockPTY(master)
	if err == nil {
		tty, err = os.OpenFile(name, os.O_RDWR|syscall.O_NOCTTY, 0)
	}
	if err == nil {
		// Darwin rejects TIOCSWINSZ on the master, so size the terminal side.
		size := windowSize{rows: ptyRows, columns: ptyColumns}
		err = ioctl(tty, syscall.TIOCSWINSZ, unsafe.Pointer(&size))
	}
	if err != nil {
		err = errors.Join(err, master.Close())
		if tty != nil {
			err = errors.Join(err, tty.Close())
		}
		return nil, nil, err
	}
	return master, tty, nil
}

// ioctl issues request on file's descriptor with a pointer argument (nil
// for requests that take none).
func ioctl(file *os.File, request uintptr, argument unsafe.Pointer) error {
	conn, err := file.SyscallConn()
	if err != nil {
		return err
	}
	var errno syscall.Errno
	if err = conn.Control(func(fd uintptr) {
		_, _, errno = syscall.Syscall(syscall.SYS_IOCTL, fd, request, uintptr(argument))
	}); err != nil {
		return err
	}
	if errno != 0 {
		return os.NewSyscallError("ioctl", errno)
	}
	return nil
}
