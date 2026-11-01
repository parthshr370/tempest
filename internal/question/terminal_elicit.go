//go:build darwin || linux

package question

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"os"
	"strings"

	"go.harness.dev/harness/internal/termtext"
)

// TerminalElicitor shows forwarded elicitations on the controlling terminal.
// It returns nil unless both input and prompt output are terminals.
func TerminalElicitor(input *os.File, output io.Writer) Elicitor {
	promptOutput, ok := output.(*os.File)
	if !ok || !isTerminal(input) || !isTerminal(promptOutput) {
		return nil
	}
	return func(ctx context.Context, request Elicit) (ElicitResult, error) {
		if err := request.Validate(); err != nil {
			return ElicitResult{}, err
		}
		select {
		case terminalSlot <- struct{}{}:
			defer func() { <-terminalSlot }()
		case <-ctx.Done():
			return ElicitResult{}, ctx.Err()
		}
		if request.Mode == "url" {
			return terminalURL(ctx, input, output, request)
		}
		return terminalForm(ctx, input, output, request)
	}
}

// terminalURL shows the full URL and its host and waits for consent; the
// terminal never opens or fetches the URL itself.
func terminalURL(ctx context.Context, input *os.File, output io.Writer, request Elicit) (ElicitResult, error) {
	target, _ := url.Parse(request.URL)
	if _, err := fmt.Fprintf(output, "\n%s\nOpen this URL in a browser (host %s):\n  %s\nPress Enter once it is open, or type 'decline': ", request.terminalDisplay(), termtext.Line(target.Host), termtext.Line(request.URL)); err != nil {
		return ElicitResult{}, err
	}
	line, err := readTerminalLine(ctx, input)
	if err != nil {
		return ElicitResult{}, err
	}
	if action := refusal(line); action != "" {
		return ElicitResult{Action: action}, nil
	}
	return ElicitResult{Action: "accept"}, nil
}

func terminalForm(ctx context.Context, input *os.File, output io.Writer, request Elicit) (ElicitResult, error) {
	form, err := parseForm(request.Schema)
	if err != nil {
		return ElicitResult{}, err
	}
	if _, err := fmt.Fprintf(output, "\n%s\n(type 'decline' or 'cancel' at any prompt to refuse)\n", request.terminalDisplay()); err != nil {
		return ElicitResult{}, err
	}
	content := map[string]json.RawMessage{}
	for _, field := range form.fields {
		if _, err := fmt.Fprint(output, field.prompt()); err != nil {
			return ElicitResult{}, err
		}
		for {
			line, err := readTerminalLine(ctx, input)
			if err != nil {
				return ElicitResult{}, err
			}
			if action := refusal(line); action != "" {
				return ElicitResult{Action: action}, nil
			}
			value, err := field.answer(line)
			if err == nil && value != nil {
				content[field.Name] = value
			}
			if err == nil {
				break
			}
			if _, err := fmt.Fprintf(output, "%s; try again: ", termtext.Line(err.Error())); err != nil {
				return ElicitResult{}, err
			}
		}
	}
	encoded, err := json.Marshal(content)
	if err != nil {
		return ElicitResult{}, err
	}
	return ElicitResult{Action: "accept", Content: encoded}, nil
}

func refusal(line string) string {
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "decline":
		return "decline"
	case "cancel":
		return "cancel"
	}
	return ""
}
