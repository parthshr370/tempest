package question

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestQuestionForms(t *testing.T) {
	for _, multi := range []bool{false, true} {
		prompt := Question{ID: "choice", Question: "Choose", Options: []Option{{Label: "one", Description: "First option"}, {Label: "two"}}, Multi: multi, AllowText: true}
		if err := Validate([]Question{prompt}); err != nil {
			t.Fatal(err)
		}
		encoded, err := json.Marshal(Form(prompt))
		if err != nil {
			t.Fatal(err)
		}
		if multi && !bytes.Contains(encoded, []byte(`"enum":["one","two"]`)) ||
			!multi && !bytes.Contains(encoded, []byte(`"oneOf":[{"title":"one - First option","const":"one"},{"title":"two","const":"two"}]`)) ||
			bytes.Contains(encoded, []byte("enumNames")) || bytes.Contains(encoded, []byte("uniqueItems")) {
			t.Fatalf("schema=%s", encoded)
		}
		response := `{"action":"accept","content":{"selected":"one","text":"context"}}`
		if multi {
			response = `{"action":"accept","content":{"selected":["one","two"],"text":"context"}}`
		}
		answer, err := DecodeForm(prompt, json.RawMessage(response))
		if err != nil {
			t.Fatal(err)
		}
		if answer.Text != "context" {
			t.Fatalf("answer=%+v", answer)
		}
		for _, invalid := range []string{`{"action":"cancel"}`, `{"action":"decline"}`, `{"action":"accept","content":{"selected":"unknown"}}`} {
			if _, err := DecodeForm(prompt, json.RawMessage(invalid)); err == nil {
				t.Fatalf("accepted %s", invalid)
			}
		}
	}
}

func TestQuestionTerminalPTY(t *testing.T) {
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skip("PTY test needs script")
	}
	script, err := exec.LookPath("script")
	if err != nil {
		t.Skip("script is not installed")
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	arguments := []string{"-q", "/dev/null", executable, "-test.run=^TestQuestionTerminalHelper$", "--", "question-pty"}
	if runtime.GOOS == "linux" {
		arguments = []string{"-q", "-c", executable + " -test.run=^TestQuestionTerminalHelper$ -- question-pty", "/dev/null"}
	}
	command := exec.CommandContext(ctx, script, arguments...)
	input, err := command.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	defer input.Close()
	var output bytes.Buffer
	command.Stdout, command.Stderr = &output, &output
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	if _, err := input.Write([]byte("1,2\nuse defaults\n")); err != nil {
		t.Fatal(err)
	}
	if err := command.Wait(); err != nil {
		t.Fatalf("PTY: %v\n%s", err, output.String())
	}
	if !strings.Contains(output.String(), `"selected":["one","two"]`) || !strings.Contains(output.String(), `"text":"use defaults"`) || !strings.Contains(output.String(), "First choice") {
		t.Fatalf("PTY output=%s", output.String())
	}
}

func TestQuestionTerminalHelper(t *testing.T) {
	if len(os.Args) == 0 || os.Args[len(os.Args)-1] != "question-pty" {
		return
	}
	handler := Terminal(os.Stdin, os.Stdout)
	if handler == nil {
		t.Fatal("PTY stdin was not recognized")
	}
	file, err := os.CreateTemp(t.TempDir(), "redirected-prompt")
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	if Terminal(os.Stdin, file) != nil {
		t.Fatal("redirected prompt output treated as terminal")
	}
	answers, err := handler(t.Context(), Request{Questions: []Question{{ID: "many", Question: "Pick choices", Options: []Option{{Label: "one", Description: "First choice"}, {Label: "two"}}, Multi: true}, {ID: "text", Question: "Explain", Options: []Option{}, AllowText: true}}})
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(answers)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stdout.Write(encoded); err != nil {
		t.Fatal(err)
	}
}

func TestQuestionHeadlessTerminal(t *testing.T) {
	file, err := os.CreateTemp(t.TempDir(), "stdin")
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	if Terminal(file, &bytes.Buffer{}) != nil {
		t.Fatal("regular file treated as terminal")
	}
}

func TestQuestionTextOnlyFormRequired(t *testing.T) {
	for _, multi := range []bool{false, true} {
		prompt := Question{ID: "text", Question: "Explain", AllowText: true, Multi: multi}
		for _, schema := range []FormSchema{Form(prompt), LegacyForm(prompt)} {
			if len(schema.Required) != 1 || schema.Required[0] != "text" {
				t.Fatalf("text-only schema=%+v", schema)
			}
		}
		if _, err := DecodeForm(prompt, json.RawMessage(`{"action":"accept","content":{}}`)); err == nil {
			t.Fatal("accepted empty text-only form")
		}
	}
}

func TestQuestionLegacySingleChoiceSchema(t *testing.T) {
	form := LegacyForm(Question{ID: "q", Question: "Choose", Options: []Option{{Label: "one", Description: "First"}}})
	selected := form.Properties["selected"]
	if len(selected.OneOf) != 0 || len(selected.Enum) != 1 || selected.Enum[0] != "one" || selected.EnumNames[0] != "one - First" {
		t.Fatalf("legacy schema=%+v", selected)
	}
}
