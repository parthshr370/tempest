// Package build holds build/version metadata for the tempest binary.
package build

import "runtime/debug"

const (
	defaultVersion = "devel"
	defaultCommit  = "unknown"
	defaultDate    = "unknown"
)

// Version is the harness build version. Release builds override it through
// -ldflags "-X go.harness.dev/harness/internal/build.Version=...".
var Version = defaultVersion

// Commit is the source revision used for this build. Release builds override it
// through -ldflags "-X go.harness.dev/harness/internal/build.Commit=...".
var Commit = defaultCommit

// Date is the UTC build timestamp. Release builds override it through
// -ldflags "-X go.harness.dev/harness/internal/build.Date=...".
var Date = defaultDate

// Name is the binary/product name printed by usage and version output.
const Name = "tempest"

// Metadata is the complete, immutable build identity printed by the CLI.
type Metadata struct {
	Version string
	Commit  string
	Date    string
}

// Current returns linker-injected metadata, filling unset development values
// from Go's embedded build information when it is available.
func Current() Metadata {
	info, _ := debug.ReadBuildInfo()
	return metadataFrom(Version, Commit, Date, info)
}

// metadataFrom preserves linker values and fills only development placeholders from module build information.
func metadataFrom(version, commit, date string, info *debug.BuildInfo) Metadata {
	result := Metadata{Version: valueOr(version, defaultVersion), Commit: valueOr(commit, defaultCommit), Date: valueOr(date, defaultDate)}
	if info == nil {
		return result
	}
	if result.Version == defaultVersion && info.Main.Version != "" && info.Main.Version != "(devel)" {
		result.Version = info.Main.Version
	}
	for _, setting := range info.Settings {
		switch setting.Key {
		case "vcs.revision":
			if result.Commit == defaultCommit && setting.Value != "" {
				result.Commit = setting.Value
			}
		case "vcs.time":
			if result.Date == defaultDate && setting.Value != "" {
				result.Date = setting.Value
			}
		}
	}
	return result
}

func valueOr(value, fallback string) string {
	if value == "" {
		return fallback
	}
	return value
}
