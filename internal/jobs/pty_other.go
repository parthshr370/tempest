//go:build !darwin && !linux

package jobs

import (
	"errors"
	"os"
)

// ptySupported gates bash pty admission: only darwin and Linux pseudo-terminals are implemented.
const ptySupported = false

func openPTY() (master, tty *os.File, err error) {
	return nil, nil, errors.New("pty needs darwin or linux")
}
