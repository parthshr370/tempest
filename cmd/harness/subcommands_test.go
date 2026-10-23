package main

import (
	"bytes"
	"go.harness.dev/harness/internal/build"
	"go.harness.dev/harness/internal/catalog"
	"strconv"
	"strings"
	"testing"
)

func TestDoctorRedactsCredentialsAndWarnsWithoutThem(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("ANTHROPIC_API_KEY", "visible-secret")
	var withCredential bytes.Buffer
	if code := runDoctor(nil, &withCredential, &bytes.Buffer{}); code != 0 {
		t.Fatalf("doctor exit = %d", code)
	}
	if output := withCredential.String(); strings.Contains(output, "visible-secret") || !strings.Contains(output, "REDACTED") || !strings.Contains(output, "PASS default provider credential") {
		t.Fatalf("doctor output did not redact/presence-check credential: %s", output)
	}

	t.Setenv("ANTHROPIC_API_KEY", "")
	t.Setenv("OPENAI_API_KEY", "")
	var withoutCredential bytes.Buffer
	if code := runDoctor([]string{"-model", "openai:gpt-x@https://ex/"}, &withoutCredential, &bytes.Buffer{}); code != 0 {
		t.Fatalf("doctor exit = %d", code)
	}
	output := withoutCredential.String()
	if !strings.Contains(output, "provider=openai") || !strings.Contains(output, "WARN default provider credential: openai absent") {
		t.Fatalf("doctor output = %s", output)
	}
}

func TestDoctorFailsOnHardConfigurationError(t *testing.T) {
	var stderr bytes.Buffer
	if code := runDoctor([]string{"-model", "unknown:model"}, &bytes.Buffer{}, &stderr); code == 0 {
		t.Fatal("doctor succeeded for unsupported provider")
	}
}

func TestDoctorListsCatalogModelsOnlyWithModelsFlag(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	var brief bytes.Buffer
	if code := runDoctor(nil, &brief, &bytes.Buffer{}); code != 0 {
		t.Fatalf("doctor exit = %d", code)
	}
	if first := catalog.Models()[0]; strings.Contains(brief.String(), first.Provider+"/"+first.ID+": window=") {
		t.Fatalf("default doctor output dumps the catalog: %s", &brief)
	}
	var out bytes.Buffer
	if code := runDoctor([]string{"-models"}, &out, &bytes.Buffer{}); code != 0 {
		t.Fatalf("doctor exit = %d", code)
	}
	output := out.String()
	for _, p := range catalog.Providers() {
		if !strings.Contains(output, p.ID+": api="+p.API) {
			t.Fatalf("doctor output missing provider %s: %s", p.ID, output)
		}
		if strings.Contains(output, "present=") && !strings.Contains(output, "present=yes") && !strings.Contains(output, "present=no") {
			t.Fatalf("provider key presence not yes/no: %s", output)
		}
	}
	for _, m := range catalog.Models() {
		if !strings.Contains(output, m.Provider+"/"+m.ID) {
			t.Fatalf("doctor output missing model %s/%s: %s", m.Provider, m.ID, output)
		}
	}
}

func TestVersionOutput(t *testing.T) {
	var output bytes.Buffer
	if code := runVersion(&output); code != 0 {
		t.Fatalf("version exit = %d", code)
	}
	metadata := build.Current()
	want := build.Name + " version=" + metadata.Version + " commit=" + metadata.Commit + " date=" + metadata.Date + "\n"
	if output.String() != want {
		t.Fatalf("version output = %q, want %q", output.String(), want)
	}
}

func TestVersionFlagDispatchIgnoresInvalidConfig(t *testing.T) {
	t.Setenv("HARNESS_MAX_TOKENS", "not-an-int")
	var output, stderr bytes.Buffer
	if code := run([]string{"-version"}, &output, &stderr); code != 0 {
		t.Fatalf("-version exit = %d, stderr = %q", code, stderr.String())
	}
	if got := output.String(); !strings.Contains(got, build.Name+" version=") || !strings.Contains(got, " commit=") || !strings.Contains(got, " date=") {
		t.Fatalf("-version output = %q", got)
	}
}

// A stray word used to be dropped silently: `tempest run -p hi` stopped
// parsing at "run", never read -p, and printed only the prompt usage line.
func TestStrayPositionalArgumentsAreRejected(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	for _, test := range []struct {
		args  []string
		stray string
	}{
		{args: []string{"run", "-p", "hi"}, stray: "run"},
		{args: []string{"-p", "hi", "extra"}, stray: "extra"},
		{args: []string{"doctor", "extra"}, stray: "extra"},
		{args: []string{"serve", "extra"}, stray: "extra"},
		{args: []string{"mcp", "extra"}, stray: "extra"},
		{args: []string{"version", "extra"}, stray: "extra"},
	} {
		t.Run(strings.Join(test.args, " "), func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			if code := run(test.args, &stdout, &stderr); code != exitUsage {
				t.Fatalf("exit = %d, want %d; stderr = %q", code, exitUsage, stderr.String())
			}
			if !strings.Contains(stderr.String(), strconv.Quote(test.stray)) || stdout.Len() != 0 {
				t.Fatalf("stderr = %q, stdout = %q; want the stray argument named", stderr.String(), stdout.String())
			}
		})
	}
}

// serve and mcp take -allow-tool/-deny-tool too, so a typo there must not
// silently grant or block nothing.
func TestServerCommandsWarnAboutUnknownToolNames(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	for _, command := range []string{"serve", "mcp"} {
		t.Run(command, func(t *testing.T) {
			var stderr bytes.Buffer
			// A zero idle timeout fails after the policy is built and before
			// the command starts serving stdin.
			run([]string{command, "-allow-tool", "bassh", "-deny-tool", "wirte", "-idle-timeout", "0"}, &bytes.Buffer{}, &stderr)
			if text := stderr.String(); !strings.Contains(text, `"bassh"`) || !strings.Contains(text, `"wirte"`) {
				t.Fatalf("stderr = %q, want warnings naming both unknown tools", text)
			}
		})
	}
}

// The README documents OPENAI_BASE_URL and ANTHROPIC_BASE_URL; the CLI used
// to resolve only the HARNESS_ variants and the flags.
func TestVendorBaseURLVariablesRouteCLIModels(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("HARNESS_OPENAI_BASE_URL", "")
	t.Setenv("HARNESS_ANTHROPIC_BASE_URL", "")
	t.Setenv("OPENAI_BASE_URL", "https://vendor-openai.example/v1")
	t.Setenv("ANTHROPIC_BASE_URL", "https://vendor-anthropic.example")
	var output bytes.Buffer
	if code := runDoctor([]string{"-model", "openai:gpt-x", "-plan-model", "anthropic:claude-x"}, &output, &bytes.Buffer{}); code != 0 {
		t.Fatalf("doctor exit = %d", code)
	}
	text := output.String()
	if !strings.Contains(text, "default: model=gpt-x provider=openai api=openai-completions base_url=https://vendor-openai.example/v1 ") ||
		!strings.Contains(text, "plan: model=claude-x provider=anthropic api=anthropic-messages base_url=https://vendor-anthropic.example ") {
		t.Fatalf("doctor output = %s", text)
	}

	t.Setenv("HARNESS_OPENAI_BASE_URL", "https://harness-openai.example/v1")
	output.Reset()
	if code := runDoctor([]string{"-model", "openai:gpt-x"}, &output, &bytes.Buffer{}); code != 0 {
		t.Fatalf("doctor exit = %d", code)
	}
	if text := output.String(); !strings.Contains(text, "base_url=https://harness-openai.example/v1 ") {
		t.Fatalf("HARNESS_OPENAI_BASE_URL did not win over OPENAI_BASE_URL: %s", text)
	}
}
