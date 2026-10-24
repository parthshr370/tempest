//go:build !darwin && !linux

package question

import (
	"io"
	"os"
)

// Terminal disables prompting where cancellable terminal input isn't supported.
func Terminal(_ *os.File, _ io.Writer) Handler { return nil }
