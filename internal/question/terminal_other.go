//go:build !darwin && !linux

package question

import (
	"io"
	"os"
)

// Terminal disables prompting where cancellable terminal input isn't supported.
func Terminal(_ *os.File, _ io.Writer) Handler { return nil }

// TerminalElicitor disables forwarded elicitations where cancellable terminal input isn't supported.
func TerminalElicitor(_ *os.File, _ io.Writer) Elicitor { return nil }
