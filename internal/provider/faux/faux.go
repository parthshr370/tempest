// Package faux provides a scripted in-memory provider for hermetic loop tests.
// Package faux provides a scripted mock provider that implements
// [base.Provider]. It is the hermetic test engine: queue [ResponseStep]s
// feeding start/deltas/done or errors, with per-call seeded chunking for
// deterministic output. Abort is [context.Context.Done].
package faux

import (
	"bytes"
	"context"
	"encoding/json"
	"math/rand"
	"strings"
	"sync"
	"time"

	"go.harness.dev/harness/internal/engine/estimate"
	"go.harness.dev/harness/internal/engine/stream"
	"go.harness.dev/harness/internal/engine/types"
	"go.harness.dev/harness/internal/provider/base"
)

const (
	defaultAPI          = "faux"
	defaultProvider     = "faux"
	defaultModelID      = "faux-1"
	defaultModelName    = "Faux Model"
	defaultBaseURL      = "http://localhost:0"
	defaultMinTokenSize = 3
	defaultMaxTokenSize = 5
	defaultSeed         = int64(1)
)

var (
	packageToolIDMu      sync.Mutex
	packageToolIDCounter int
)

// ModelDef describes one faux model.
type ModelDef struct {
	ID            string
	Name          string
	Reasoning     bool
	Input         []string
	Cost          types.ModelCost
	ContextWindow int
	MaxTokens     int
}

// Options configures a Faux base.
type Options struct {
	API             string
	Provider        string
	Models          []ModelDef
	TokensPerSecond float64
	MinTokenSize    int
	MaxTokenSize    int
	Seed            int64
}

// State is the mutable faux-provider state snapshot passed to response factories.
type State struct {
	CallCount int
}

// ResponseStep is a queued faux response, resolved when Stream consumes it.
type ResponseStep func(c types.Context, opts *types.StreamOptions, state State, model types.Model) (types.AssistantMessage, error)

// Faux is the scripted provider state. The queue, call count, prompt cache, and
// generated missing tool-call IDs are mutex-protected so Stream is race-clean.
type Faux struct {
	api             string
	provider        string
	models          []types.Model
	tokensPerSecond float64
	minTokenSize    int
	maxTokenSize    int
	seed            int64

	mu            sync.Mutex
	pending       []ResponseStep
	callCount     int
	promptCache   map[string]string
	toolIDCounter int
}

// New creates a faux provider core.
func New(opts Options) *Faux {
	api := opts.API
	if api == "" {
		// Use a stable default for deterministic tests and reproducible provider
		// registry keys.
		api = defaultAPI
	}
	providerID := opts.Provider
	if providerID == "" {
		providerID = defaultProvider
	}

	minOpt := opts.MinTokenSize
	if minOpt == 0 {
		minOpt = defaultMinTokenSize
	}
	maxOpt := opts.MaxTokenSize
	if maxOpt == 0 {
		maxOpt = defaultMaxTokenSize
	}
	minTokenSize := maxInt(1, minInt(minOpt, maxOpt))
	maxTokenSize := maxInt(minTokenSize, maxOpt)

	seed := opts.Seed
	if seed == 0 {
		seed = defaultSeed
	}

	defs := opts.Models
	if len(defs) == 0 {
		defs = []ModelDef{{
			ID:            defaultModelID,
			Name:          defaultModelName,
			Input:         []string{"text", "image"},
			ContextWindow: 128000,
			MaxTokens:     16384,
		}}
	}
	models := make([]types.Model, 0, len(defs))
	for _, def := range defs {
		name := def.Name
		if name == "" {
			name = def.ID
		}
		input := def.Input
		if input == nil {
			input = []string{"text", "image"}
		}
		contextWindow := def.ContextWindow
		if contextWindow == 0 {
			contextWindow = 128000
		}
		maxTokens := def.MaxTokens
		if maxTokens == 0 {
			maxTokens = 16384
		}
		models = append(models, types.Model{
			ID:            def.ID,
			Name:          name,
			API:           api,
			Provider:      providerID,
			BaseURL:       defaultBaseURL,
			Reasoning:     def.Reasoning,
			Input:         append([]string(nil), input...),
			Cost:          def.Cost,
			ContextWindow: contextWindow,
			MaxTokens:     maxTokens,
		})
	}

	return &Faux{
		api:             api,
		provider:        providerID,
		models:          models,
		tokensPerSecond: opts.TokensPerSecond,
		minTokenSize:    minTokenSize,
		maxTokenSize:    maxTokenSize,
		seed:            seed,
		promptCache:     map[string]string{},
	}
}

// SetResponses replaces the pending response queue.
func (f *Faux) SetResponses(steps ...ResponseStep) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pending = append([]ResponseStep(nil), steps...)
}

// AppendResponses appends to the pending response queue.
func (f *Faux) AppendResponses(steps ...ResponseStep) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pending = append(f.pending, steps...)
}

// PendingCount returns the number of queued responses.
func (f *Faux) PendingCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.pending)
}

// CallCount returns the number of Stream calls consumed so far.
func (f *Faux) CallCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.callCount
}

// Model returns the first faux model.
func (f *Faux) Model() types.Model { return f.models[0] }

// ModelByID returns the matching faux model.
func (f *Faux) ModelByID(id string) (types.Model, bool) {
	for _, model := range f.models {
		if model.ID == id {
			return model, true
		}
	}
	return types.Model{}, false
}

// Provider returns the provider seam wrapper for this Faux instance.
func (f *Faux) Provider() base.Provider {
	return base.Provider{
		ID:           f.provider,
		Models:       append([]types.Model(nil), f.models...),
		Stream:       f.Stream,
		StreamSimple: f.StreamSimple,
	}
}

// Stream consumes the next queued response and replays it as assistant events.
func (f *Faux) Stream(ctx context.Context, model types.Model, c types.Context, opts *types.StreamOptions) *stream.AssistantStream {
	f.mu.Lock()
	var step ResponseStep
	if len(f.pending) > 0 {
		step = f.pending[0]
		f.pending = f.pending[1:]
	}
	f.callCount++
	callIndex := f.callCount
	f.mu.Unlock()

	s := stream.NewAssistantStream(f.api, f.provider, model.ID)
	rng := rand.New(rand.NewSource(f.seed + int64(callIndex)))
	go f.produce(ctx, s, step, c, opts, State{CallCount: callIndex}, model, rng)
	return s
}

// StreamSimple delegates to Stream.
func (f *Faux) StreamSimple(ctx context.Context, model types.Model, c types.Context, opts *types.SimpleStreamOptions) *stream.AssistantStream {
	if opts == nil {
		return f.Stream(ctx, model, c, nil)
	}
	streamOpts := opts.StreamOptions
	return f.Stream(ctx, model, c, &streamOpts)
}

// produce runs response factories away from Stream's caller and converts panics into in-band errors.
func (f *Faux) produce(ctx context.Context, s *stream.AssistantStream, step ResponseStep, c types.Context, opts *types.StreamOptions, state State, model types.Model, rng *rand.Rand) {
	defer func() {
		if recovered := recover(); recovered != nil {
			message := createErrorMessage(errorText(recovered), f.api, f.provider, model.ID)
			s.Push(types.StreamEvent{Type: types.EvError, Reason: types.StopError, Err: &message})
		}
	}()

	if step == nil {
		message := createErrorMessage("No more faux responses queued", f.api, f.provider, model.ID)
		message = f.withUsageEstimate(message, c, opts, model)
		s.Push(types.StreamEvent{Type: types.EvError, Reason: types.StopError, Err: &message})
		return
	}

	resolved, err := step(c, opts, state, model)
	if err != nil {
		message := createErrorMessage(err.Error(), f.api, f.provider, model.ID)
		s.Push(types.StreamEvent{Type: types.EvError, Reason: types.StopError, Err: &message})
		return
	}

	message := f.cloneMessage(resolved, model.ID)
	message = f.withUsageEstimate(message, c, opts, model)
	f.streamWithDeltas(ctx, s, message, rng)
}

// Text returns a faux text content block.
func Text(text string) types.ContentBlock { return types.NewText(text) }

// Thinking returns a faux thinking content block.
func Thinking(thinking string) types.ContentBlock {
	return types.ContentBlock{Type: types.BlockThinking, Thinking: thinking}
}

// ToolCall returns a faux tool-call block. Empty args become `{}` to match the
// required object argument shape. Empty IDs use a deterministic guarded package
// counter because this package-level builder has no Faux receiver.
func ToolCall(name string, args json.RawMessage, id string) types.ContentBlock {
	if id == "" {
		id = nextPackageToolID()
	}
	if len(args) == 0 {
		args = json.RawMessage(`{}`)
	}
	return types.NewToolCall(id, name, copyRaw(args))
}

// Respond returns a static successful assistant response step.
func Respond(content ...types.ContentBlock) ResponseStep {
	blocks := deepCopyContent(content)
	return func(types.Context, *types.StreamOptions, State, types.Model) (types.AssistantMessage, error) {
		return types.AssistantMessage{
			Content:    deepCopyContent(blocks),
			API:        defaultAPI,
			Provider:   defaultProvider,
			Model:      defaultModelID,
			Usage:      types.Usage{},
			StopReason: types.StopStop,
			Timestamp:  time.Now().UnixMilli(),
		}, nil
	}
}

// RespondMessage wraps an explicit assistant message as a response step.
func RespondMessage(msg types.AssistantMessage) ResponseStep {
	return func(types.Context, *types.StreamOptions, State, types.Model) (types.AssistantMessage, error) {
		return msg, nil
	}
}

// streamWithDeltas replays a completed response through the provider event lifecycle while tracking cancellation state.
func (f *Faux) streamWithDeltas(ctx context.Context, s *stream.AssistantStream, message types.AssistantMessage, rng *rand.Rand) {
	// AssistantStream folds events into its own builder, so Partial is
	// intentionally left nil. Context cancellation uses ctx, randomness uses a
	// per-call seeded RNG, and timestamps use Unix milliseconds.
	partial := message
	partial.Content = []types.ContentBlock{}
	if ctx.Err() != nil {
		aborted := createAbortedMessage(partial)
		s.Push(types.StreamEvent{Type: types.EvError, Reason: types.StopAborted, Err: &aborted})
		return
	}

	s.Push(types.StreamEvent{Type: types.EvStart})
	for index, block := range message.Content {
		if ctx.Err() != nil {
			aborted := createAbortedMessage(partial)
			s.Push(types.StreamEvent{Type: types.EvError, Reason: types.StopAborted, Err: &aborted})
			return
		}

		switch block.Type {
		case types.BlockThinking:
			partial.Content = append(partial.Content, types.ContentBlock{Type: types.BlockThinking})
			s.Push(types.StreamEvent{Type: types.EvThinkingStart, ContentIndex: index})
			for _, chunk := range f.splitStringByTokenSize(block.Thinking, rng) {
				f.scheduleChunk(ctx, chunk)
				if ctx.Err() != nil {
					aborted := createAbortedMessage(partial)
					s.Push(types.StreamEvent{Type: types.EvError, Reason: types.StopAborted, Err: &aborted})
					return
				}
				partial.Content[index].Thinking += chunk
				s.Push(types.StreamEvent{Type: types.EvThinkingDelta, ContentIndex: index, Delta: chunk})
			}
			s.Push(types.StreamEvent{Type: types.EvThinkingEnd, ContentIndex: index, Content: block.Thinking})

		case types.BlockText:
			partial.Content = append(partial.Content, types.ContentBlock{Type: types.BlockText})
			s.Push(types.StreamEvent{Type: types.EvTextStart, ContentIndex: index})
			for _, chunk := range f.splitStringByTokenSize(block.Text, rng) {
				f.scheduleChunk(ctx, chunk)
				if ctx.Err() != nil {
					aborted := createAbortedMessage(partial)
					s.Push(types.StreamEvent{Type: types.EvError, Reason: types.StopAborted, Err: &aborted})
					return
				}
				partial.Content[index].Text += chunk
				s.Push(types.StreamEvent{Type: types.EvTextDelta, ContentIndex: index, Delta: chunk})
			}
			s.Push(types.StreamEvent{Type: types.EvTextEnd, ContentIndex: index, Content: block.Text})

		case types.BlockToolCall:
			partial.Content = append(partial.Content, types.ContentBlock{Type: types.BlockToolCall, ID: block.ID, Name: block.Name, Arguments: json.RawMessage(`{}`)})
			start := types.ContentBlock{Type: types.BlockToolCall, ID: block.ID, Name: block.Name}
			s.Push(types.StreamEvent{Type: types.EvToolCallStart, ContentIndex: index, ToolCall: &start})
			for _, chunk := range f.splitStringByTokenSize(rawArgumentsString(block.Arguments), rng) {
				f.scheduleChunk(ctx, chunk)
				if ctx.Err() != nil {
					aborted := createAbortedMessage(partial)
					s.Push(types.StreamEvent{Type: types.EvError, Reason: types.StopAborted, Err: &aborted})
					return
				}
				s.Push(types.StreamEvent{Type: types.EvToolCallDelta, ContentIndex: index, Delta: chunk})
			}
			partial.Content[index].Arguments = copyRaw(block.Arguments)
			full := block
			full.Arguments = copyRaw(block.Arguments)
			s.Push(types.StreamEvent{Type: types.EvToolCallEnd, ContentIndex: index, ToolCall: &full})
		}
	}

	if message.StopReason == types.StopError || message.StopReason == types.StopAborted {
		s.Push(types.StreamEvent{Type: types.EvError, Reason: message.StopReason, Err: &message})
		return
	}
	s.Push(types.StreamEvent{Type: types.EvDone, Reason: message.StopReason, Message: &message})
}

// splitStringByTokenSize keeps faux chunk cadence reproducible from each call's seeded RNG.
func (f *Faux) splitStringByTokenSize(text string, rng *rand.Rand) []string {
	chunks := []string{}
	for index := 0; index < len(text); {
		tokenSize := f.minTokenSize + rng.Intn(f.maxTokenSize-f.minTokenSize+1)
		charSize := maxInt(1, tokenSize*4)
		end := index + charSize
		if end > len(text) {
			end = len(text)
		}
		chunks = append(chunks, text[index:end])
		index = end
	}
	if len(chunks) == 0 {
		return []string{""}
	}
	return chunks
}

// scheduleChunk lets latency tests pace output while cancellation can still break the wait.
func (f *Faux) scheduleChunk(ctx context.Context, chunk string) {
	if f.tokensPerSecond <= 0 {
		return
	}
	delay := time.Duration((float64(estimate.EstimateTextTokens(chunk)) / f.tokensPerSecond) * float64(time.Second))
	select {
	case <-time.After(delay):
	case <-ctx.Done():
	}
}

// withUsageEstimate fabricates cache-aware usage so tests can exercise accounting without a real provider.
func (f *Faux) withUsageEstimate(message types.AssistantMessage, c types.Context, opts *types.StreamOptions, model types.Model) types.AssistantMessage {
	promptText := serializeContext(c)
	promptTokens := estimate.EstimateTextTokens(promptText)
	outputTokens := estimate.EstimateTextTokens(assistantContentToText(message.Content))
	input := promptTokens
	cacheRead := 0
	cacheWrite := 0

	if opts != nil && opts.SessionID != "" && opts.CacheRetention != "none" {
		f.mu.Lock()
		previousPrompt := f.promptCache[opts.SessionID]
		if previousPrompt != "" {
			cachedChars := commonPrefixLength(previousPrompt, promptText)
			cacheRead = estimate.EstimateTextTokens(previousPrompt[:cachedChars])
			cacheWrite = estimate.EstimateTextTokens(promptText[cachedChars:])
			input = maxInt(0, promptTokens-cacheRead)
		} else {
			cacheWrite = promptTokens
		}
		f.promptCache[opts.SessionID] = promptText
		f.mu.Unlock()
	}

	message.Usage = types.Usage{
		Input:       input,
		Output:      outputTokens,
		CacheRead:   cacheRead,
		CacheWrite:  cacheWrite,
		TotalTokens: input + outputTokens + cacheRead + cacheWrite,
		Cost:        types.Cost{},
	}
	types.CalculateCost(model.Cost, &message.Usage)
	return message
}

// serializeContext captures every prompt input that can change a faux cache estimate.
func serializeContext(c types.Context) string {
	parts := []string{}
	if c.SystemPrompt != "" {
		parts = append(parts, "system:"+c.SystemPrompt)
	}
	for _, message := range c.Messages {
		if message == nil {
			continue
		}
		parts = append(parts, message.Role()+":"+messageToText(message))
	}
	if len(c.Tools) > 0 {
		raw, err := jsonStringify(c.Tools)
		if err != nil {
			raw = []byte("[unserializable]")
		}
		parts = append(parts, "tools:"+string(raw))
	}
	return strings.Join(parts, "\n\n")
}

// jsonStringify marshals like JS JSON.stringify: compact and without Go's default
// HTML escaping of <, >, & so serialization length remains predictable.
func jsonStringify(v any) ([]byte, error) {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimRight(b.Bytes(), "\n"), nil // Encoder appends a newline; strip it
}

// messageToText flattens known message variants for cache and usage accounting; unknown variants don't affect faux estimates.
func messageToText(message types.Message) string {
	switch m := message.(type) {
	case types.UserMessage:
		return contentToText(m.Content)
	case *types.UserMessage:
		if m == nil {
			return ""
		}
		return contentToText(m.Content)
	case types.AssistantMessage:
		return assistantContentToText(m.Content)
	case *types.AssistantMessage:
		if m == nil {
			return ""
		}
		return assistantContentToText(m.Content)
	case types.ToolResultMessage:
		return toolResultToText(m)
	case *types.ToolResultMessage:
		if m == nil {
			return ""
		}
		return toolResultToText(*m)
	default:
		return ""
	}
}

func contentToText(content types.UserContent) string {
	if !content.IsBlocks() {
		return content.Text
	}
	parts := make([]string, 0, len(content.Blocks))
	for _, block := range content.Blocks {
		parts = append(parts, contentBlockToText(block))
	}
	return strings.Join(parts, "\n")
}

// toolResultToText includes the tool name so equal output from different tools doesn't share cache text.
func toolResultToText(message types.ToolResultMessage) string {
	parts := []string{message.ToolName}
	for _, block := range message.Content {
		parts = append(parts, contentBlockToText(block))
	}
	return strings.Join(parts, "\n")
}

// contentBlockToText replaces image bytes with a stable marker, so estimates distinguish images without serializing payloads.
func contentBlockToText(block types.ContentBlock) string {
	if block.Type == types.BlockText {
		return block.Text
	}
	return "[image:" + block.MimeType + ":" + intString(len(block.Data)) + "]"
}

// assistantContentToText includes thinking and tool calls so faux output usage doesn't treat them as free.
func assistantContentToText(content []types.ContentBlock) string {
	parts := make([]string, 0, len(content))
	for _, block := range content {
		switch block.Type {
		case types.BlockText:
			parts = append(parts, block.Text)
		case types.BlockThinking:
			parts = append(parts, block.Thinking)
		case types.BlockToolCall:
			parts = append(parts, block.Name+":"+rawArgumentsString(block.Arguments))
		}
	}
	return strings.Join(parts, "\n")
}

// commonPrefixLength finds the byte index needed to reuse only an unchanged prompt prefix.
func commonPrefixLength(a, b string) int {
	length := minInt(len(a), len(b))
	index := 0
	for index < length && a[index] == b[index] {
		index++
	}
	return index
}

// cloneMessage keeps response templates reusable while stamping the active provider and filling tool-call defaults.
func (f *Faux) cloneMessage(message types.AssistantMessage, modelID string) types.AssistantMessage {
	cloned := message
	cloned.Content = deepCopyContent(message.Content)
	cloned.Diagnostics = deepCopyDiagnostics(message.Diagnostics)
	cloned.API = f.api
	cloned.Provider = f.provider
	cloned.Model = modelID
	if cloned.Timestamp == 0 {
		cloned.Timestamp = time.Now().UnixMilli()
	}
	for index := range cloned.Content {
		if cloned.Content[index].Type == types.BlockToolCall {
			if cloned.Content[index].ID == "" {
				cloned.Content[index].ID = f.nextToolID()
			}
			if len(cloned.Content[index].Arguments) == 0 {
				cloned.Content[index].Arguments = json.RawMessage(`{}`)
			}
		}
	}
	return cloned
}

// createErrorMessage keeps faux failures on the same assistant-message event shape that real provider streams use.
func createErrorMessage(message, api, providerID, modelID string) types.AssistantMessage {
	return types.AssistantMessage{
		Content:      []types.ContentBlock{},
		API:          api,
		Provider:     providerID,
		Model:        modelID,
		Usage:        types.Usage{},
		StopReason:   types.StopError,
		ErrorMessage: message,
		Timestamp:    time.Now().UnixMilli(),
	}
}

// createAbortedMessage preserves the partial message, letting cancellation report what consumers already received.
func createAbortedMessage(partial types.AssistantMessage) types.AssistantMessage {
	partial.StopReason = types.StopAborted
	partial.ErrorMessage = "Request was aborted"
	partial.Timestamp = time.Now().UnixMilli()
	return partial
}

// nextToolID keeps generated tool-call IDs unique across concurrent streams.
func (f *Faux) nextToolID() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.toolIDCounter++
	return "tool:" + intString(f.toolIDCounter)
}

// nextPackageToolID gives package-level builders stable IDs even though they don't have a Faux counter.
func nextPackageToolID() string {
	packageToolIDMu.Lock()
	defer packageToolIDMu.Unlock()
	packageToolIDCounter++
	return "tool:" + intString(packageToolIDCounter)
}

// deepCopyContent keeps response-step templates isolated when Stream fills tool IDs or normalizes arguments.
func deepCopyContent(content []types.ContentBlock) []types.ContentBlock {
	if content == nil {
		return nil
	}
	copyContent := make([]types.ContentBlock, len(content))
	copy(copyContent, content)
	for index := range copyContent {
		copyContent[index].Arguments = copyRaw(copyContent[index].Arguments)
	}
	return copyContent
}

// deepCopyDiagnostics breaks diagnostic aliasing before a queued response can be replayed.
func deepCopyDiagnostics(diagnostics []types.AssistantMessageDiagnostic) []types.AssistantMessageDiagnostic {
	if diagnostics == nil {
		return nil
	}
	var copied []types.AssistantMessageDiagnostic
	if raw, err := json.Marshal(diagnostics); err == nil && json.Unmarshal(raw, &copied) == nil {
		return copied
	}
	return append([]types.AssistantMessageDiagnostic(nil), diagnostics...)
}

func copyRaw(raw json.RawMessage) json.RawMessage {
	if raw == nil {
		return nil
	}
	return append(json.RawMessage(nil), raw...)
}

func rawArgumentsString(args json.RawMessage) string {
	if len(args) == 0 {
		return "{}"
	}
	return string(args)
}

// errorText reduces recovered panic values to a usable in-band stream error.
func errorText(value any) string {
	switch v := value.(type) {
	case error:
		return v.Error()
	case string:
		return v
	default:
		raw, err := json.Marshal(v)
		if err == nil && len(raw) > 0 {
			return string(raw)
		}
		return "panic"
	}
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func intString(n int) string {
	if n == 0 {
		return "0"
	}
	digits := [20]byte{}
	index := len(digits)
	for n > 0 {
		index--
		digits[index] = byte('0' + n%10)
		n /= 10
	}
	return string(digits[index:])
}
