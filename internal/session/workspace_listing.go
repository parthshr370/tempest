package session

import (
	"context"
	"slices"

	ptypes "go.harness.dev/harness/internal/engine/types"
)

// withWorkspaceListing places the session's listing at the head of the first
// user message of every provider request. The listing is repository data, so
// it rides with user input rather than in the system prompt, and it is applied
// per request rather than stored in the message, so history, events and the
// journal carry only what the user sent. The same journaled bytes go into
// every request, which keeps the cached prefix stable across turns, resume
// and forks. An empty listing installs no transform.
func withWorkspaceListing(listing string) func(context.Context, []ptypes.Message) []ptypes.Message {
	if listing == "" {
		return nil
	}
	block := ptypes.NewText(listing)
	return func(_ context.Context, messages []ptypes.Message) []ptypes.Message {
		for index, message := range messages {
			var user ptypes.UserMessage
			switch typed := message.(type) {
			case ptypes.UserMessage:
				user = typed
			case *ptypes.UserMessage:
				if typed == nil {
					continue
				}
				user = *typed
			default:
				continue
			}
			blocks := make([]ptypes.ContentBlock, 0, len(user.Content.Blocks)+2)
			blocks = append(blocks, block)
			if user.Content.IsBlocks() {
				blocks = append(blocks, user.Content.Blocks...)
			} else if user.Content.Text != "" {
				blocks = append(blocks, ptypes.NewText(user.Content.Text))
			}
			user.Content = ptypes.BlockContent(blocks...)
			placed := slices.Clone(messages)
			placed[index] = user
			return placed
		}
		return messages
	}
}
