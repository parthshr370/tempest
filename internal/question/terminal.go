//go:build darwin || linux

package question

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"syscall"
	"unsafe"
)

const terminalPollMicros = 100000
const terminalAnswerLimit = 64 << 10

// Terminal returns nil unless both input and prompt output are terminals.
func Terminal(input *os.File, output io.Writer) Handler {
	promptOutput, ok := output.(*os.File)
	if !ok || !isTerminal(input) || !isTerminal(promptOutput) {
		return nil
	}
	promptSlot := make(chan struct{}, 1)
	return func(ctx context.Context, request Request) ([]Answer, error) {
		select {
		case promptSlot <- struct{}{}:
			defer func() { <-promptSlot }()
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		answers := make([]Answer, 0, len(request.Questions))
		for _, prompt := range request.Questions {
			if _, err := fmt.Fprintln(output, "\n"+prompt.Question); err != nil {
				return nil, err
			}
			for i, option := range prompt.Options {
				if _, err := fmt.Fprintf(output, "  %d. %s", i+1, option.Label); err != nil {
					return nil, err
				}
				if option.Description != "" {
					if _, err := fmt.Fprint(output, " - "+option.Description); err != nil {
						return nil, err
					}
				}
				if _, err := fmt.Fprintln(output); err != nil {
					return nil, err
				}
			}
			hint := "Choose a number"
			if prompt.Multi {
				hint = "Choose numbers separated by commas (empty selects none)"
			}
			if prompt.AllowText {
				hint += " or type text after 'text:'"
			}
			if len(prompt.Options) == 0 && prompt.AllowText {
				hint = "Enter your answer"
			}
			hint += "; 'cancel' declines: "
			for {
				if _, err := fmt.Fprint(output, hint); err != nil {
					return nil, err
				}
				line, err := readTerminalLine(ctx, input)
				if err != nil {
					return nil, err
				}
				if line == "cancel" {
					return nil, errors.New("user cancelled ask; proceed with your best judgment")
				}
				answer := Answer{ID: prompt.ID, Selected: []string{}}
				isValidSelection := true
				if strings.HasPrefix(line, "text:") && prompt.AllowText {
					answer.Text = strings.TrimSpace(strings.TrimPrefix(line, "text:"))
				} else if len(prompt.Options) == 0 && prompt.AllowText {
					answer.Text = line
				} else if line != "" {
					for _, part := range strings.Split(line, ",") {
						index, err := strconv.Atoi(strings.TrimSpace(part))
						if err != nil || index < 1 || index > len(prompt.Options) {
							isValidSelection = false
							break
						}
						answer.Selected = append(answer.Selected, prompt.Options[index-1].Label)
					}
				}
				if !isValidSelection {
					if _, err := fmt.Fprintln(output, "Enter an offered option number."); err != nil {
						return nil, err
					}
					continue
				}
				if err := ValidateAnswers([]Question{prompt}, []Answer{answer}); err != nil {
					if _, writeErr := fmt.Fprintln(output, err); writeErr != nil {
						return nil, writeErr
					}
					continue
				}
				answers = append(answers, answer)
				break
			}
		}
		return answers, nil
	}
}

func isTerminal(file *os.File) bool {
	if file == nil {
		return false
	}
	var size [4]uint16
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, file.Fd(), syscall.TIOCGWINSZ, uintptr(unsafe.Pointer(&size)))
	return errno == 0
}

func readTerminalLine(ctx context.Context, input *os.File) (string, error) {
	descriptor := int(input.Fd())
	var line strings.Builder
	var ready syscall.FdSet
	wordBits := int(unsafe.Sizeof(ready.Bits[0])) * 8
	if descriptor < 0 || descriptor >= len(ready.Bits)*wordBits {
		return "", errors.New("terminal descriptor exceeds select capacity")
	}
	var character [1]byte
	for {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		ready = syscall.FdSet{}
		ready.Bits[descriptor/wordBits] |= 1 << uint(descriptor%wordBits)
		timeout := syscall.Timeval{Usec: terminalPollMicros}
		isReady, err := terminalReady(descriptor, &ready, &timeout)
		if err == syscall.EINTR {
			continue
		}
		if err != nil {
			return "", err
		}
		if !isReady {
			continue
		}
		count, err := syscall.Read(descriptor, character[:])
		if err == syscall.EINTR {
			continue
		}
		if err != nil {
			return "", err
		}
		if count == 0 {
			return "", io.EOF
		}
		if character[0] == '\n' {
			return strings.TrimSpace(line.String()), nil
		}
		if line.Len() >= terminalAnswerLimit {
			return "", errors.New("ask answer exceeds 64 KiB")
		}
		line.WriteByte(character[0])
	}
}
