package extensionhost

import "go.harness.dev/harness/tempest/extension"

// Child shares executable generations, not the parent's store or callback grants.
// BuildAgentStack supplies the child's own tools and inherited permission gate.
func (runtime *Runtime) Child(store Store, sessionID, cwd string) *Runtime {
	return &Runtime{Registry: runtime.Registry, Selected: append([]extension.Selection(nil), runtime.Selected...), Store: store, SessionID: sessionID, Cwd: cwd, EnvironmentID: "local:" + cwd}
}
