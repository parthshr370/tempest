package extension

import "encoding/json"

type initializeRequest struct {
	ProtocolVersion string `json:"protocolVersion"`
	Host            struct {
		Name string `json:"name"`
	} `json:"host"`
	MaxFrameSize int      `json:"maxFrameSize"`
	Capabilities []string `json:"capabilities"`
}
type initializedNotification struct {
	Capabilities map[string]bool `json:"capabilities"`
}
type handlerRequest struct {
	Name         string             `json:"name"`
	Context      InvocationIdentity `json:"context"`
	Arguments    json.RawMessage    `json:"arguments,omitempty"`
	Input        *HookInput         `json:"input,omitempty"`
	Call         *Call              `json:"call,omitempty"`
	Task         *Task              `json:"task,omitempty"`
	Abort        bool               `json:"abort,omitempty"`
	Continuation string             `json:"continuation,omitempty"`
}
type memoResponse struct {
	Value    json.RawMessage `json:"value"`
	Present  bool            `json:"present"`
	Inserted bool            `json:"inserted"`
}
