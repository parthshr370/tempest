package config

import (
	"errors"
	"slices"
	"testing"
)

func TestContextFilesDefaultsFillPartialObject(t *testing.T) {
	const userMaxBytes, smallMaxBytes = 100000, 1000
	w := newLayerWorkspace(t)
	w.write(w.user, `{"context_files":{"max_bytes":100000}}`)
	w.flags.Overrides = StringsInput{Set: true, Values: []string{"context_files.nested=false"}}
	resolved := w.mustResolve()
	got := resolved.ContextFiles()
	want := ContextFileSettings{Fallback: DefaultContextFallback, MaxBytes: userMaxBytes, NestedMaxBytes: DefaultContextNestedMaxBytes, NestedDisabled: true}
	if !slices.Equal(got.Fallback, want.Fallback) || got.MaxBytes != want.MaxBytes || got.NestedMaxBytes != want.NestedMaxBytes || got.NestedDisabled != want.NestedDisabled {
		t.Fatalf("context files = %+v, want %+v", got, want)
	}
	for _, setting := range resolved.Settings() {
		if setting.Key == "context_files.nested_max_bytes" && setting.Layer != LayerDefault {
			t.Fatalf("nested_max_bytes source = %+v, want the built-in default", setting)
		}
	}

	// A total cap below the nested default bounds each nested file too.
	w = newLayerWorkspace(t)
	w.write(w.user, `{"context_files":{"max_bytes":1000}}`)
	if got := w.mustResolve().ContextFiles(); got.NestedMaxBytes != smallMaxBytes {
		t.Fatalf("nested_max_bytes = %d, want %d", got.NestedMaxBytes, smallMaxBytes)
	}
}

func TestContextFilesBuiltinDefaults(t *testing.T) {
	got := newLayerWorkspace(t).mustResolve().ContextFiles()
	if !slices.Equal(got.Fallback, []string{"CLAUDE.md"}) || got.MaxBytes != DefaultContextMaxBytes || got.NestedMaxBytes != DefaultContextNestedMaxBytes || got.NestedDisabled {
		t.Fatalf("context files = %+v", got)
	}
}

func TestContextFilesRejectsBadValues(t *testing.T) {
	for _, test := range []struct {
		settings string
		field    string
	}{
		{`{"context_files":{"fallback":["docs/CLAUDE.md"]}}`, "context_files.fallback"},
		{`{"context_files":{"max_bytes":0}}`, "context_files.max_bytes"},
		{`{"context_files":{"nested_max_bytes":0}}`, "context_files.nested_max_bytes"},
	} {
		w := newLayerWorkspace(t)
		w.write(w.user, test.settings)
		_, err := w.resolve()
		var configErr *Error
		if !errors.As(err, &configErr) || configErr.Field != test.field {
			t.Fatalf("%s: err = %v, want field %s", test.settings, err, test.field)
		}
	}
}
