package vertex

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"strings"
	"time"

	"go.harness.dev/harness/internal/engine/stream"
	"go.harness.dev/harness/internal/engine/types"
	"go.harness.dev/harness/internal/provider/anthropic"
	"go.harness.dev/harness/internal/provider/base"
	"go.harness.dev/harness/internal/provider/google"
)

// StreamSimple selects the publisher from the model ID and shares the native wire converters.
func StreamSimple(parent context.Context, model types.Model, c types.Context, opts *types.SimpleStreamOptions) *stream.AssistantStream {
	s := stream.NewAssistantStream(model.API, model.Provider, model.ID)
	options := types.SimpleStreamOptions{}
	if opts != nil {
		options = *opts
	}
	options.Headers = map[string]string{}
	if opts != nil {
		for k, v := range opts.Headers {
			options.Headers[k] = v
		}
	}
	go func() {
		ctx, cancel := context.WithCancel(parent)
		defer cancel()
		watchdog := base.WatchStream(cancel, base.StreamTimeoutsFromEnv())
		watching := true
		defer func() {
			if watching {
				watchdog.Stop()
			}
		}()
		fail := func(err error) {
			out := &types.AssistantMessage{API: model.API, Provider: model.Provider, Model: model.ID, Content: []types.ContentBlock{}, StopReason: types.StopError, Timestamp: time.Now().UnixMilli(), Usage: types.Usage{Cost: types.Cost{Unknown: model.Cost.Unknown}}}
			err = watchdog.Err(parent.Err(), err)
			if parent.Err() != nil {
				out.StopReason = types.StopAborted
			}
			base.ApplyError(out, err)
			s.Push(types.StreamEvent{Type: types.EvError, Reason: out.StopReason, Err: out})
		}
		if options.TimeoutMs > 0 {
			var stop context.CancelFunc
			ctx, stop = context.WithTimeout(ctx, time.Duration(options.TimeoutMs)*time.Millisecond)
			defer stop()
		}
		endpoint := strings.TrimRight(model.BaseURL, "/")
		endpointURL, _ := url.Parse(endpoint)
		endpointPath := ""
		if endpointURL != nil {
			endpointPath = endpointURL.Path
		}
		claude := strings.HasSuffix(endpointPath, ":streamRawPredict") || strings.HasSuffix(endpointPath, ":rawPredict")
		if !strings.HasSuffix(endpointPath, ":streamGenerateContent") && !strings.HasSuffix(endpointPath, ":generateContent") {
			claude = claude || strings.HasPrefix(model.ID, "claude-") || strings.Contains(model.ID, "/anthropic/")
		}
		expressKey := ""
		if !claude {
			expressKey = options.APIKey
			if expressKey == "" && !options.DisableEnvAuth && !options.NoAuth {
				expressKey = os.Getenv("GOOGLE_CLOUD_API_KEY")
			}
		}
		hasAuth := false
		for _, headers := range []map[string]string{model.Headers, options.Headers} {
			for k, v := range headers {
				if strings.EqualFold(k, "authorization") && v != "" {
					hasAuth = true
				}
			}
		}
		if hasAuth {
			expressKey = ""
		}
		project := os.Getenv("GOOGLE_CLOUD_PROJECT")
		if project == "" {
			project = os.Getenv("GCLOUD_PROJECT")
		}
		if !options.NoAuth && !hasAuth && expressKey == "" {
			if options.DisableEnvAuth {
				fail(fmt.Errorf("no credentials for Vertex"))
				return
			}
			token, accountProject, err := accessToken(ctx)
			if err != nil {
				fail(err)
				return
			}
			options.Headers["Authorization"] = "Bearer " + token
			if project == "" {
				project = accountProject
			}
		}
		options.DisableEnvAuth = true
		options.APIKey = ""
		if expressKey != "" && !options.NoAuth {
			options.APIKey = expressKey
		}
		if !strings.Contains(endpoint, ":streamGenerateContent") && !strings.Contains(endpoint, ":streamRawPredict") && !strings.Contains(endpoint, ":generateContent") && !strings.Contains(endpoint, ":rawPredict") {
			publisher := "google"
			method := ":streamGenerateContent?alt=sse"
			if claude {
				publisher = "anthropic"
				method = ":streamRawPredict"
			}
			if options.Transport == "http" {
				method = ":generateContent"
				if claude {
					method = ":rawPredict"
				}
			}
			if expressKey != "" && !options.NoAuth {
				if endpoint == "" {
					endpoint = "https://aiplatform.googleapis.com"
				}
				endpoint += "/v1/publishers/google/models/" + url.PathEscape(model.ID) + method
			} else {
				location := os.Getenv("GOOGLE_CLOUD_LOCATION")
				if location == "" {
					location = "global"
				}
				if project == "" && !strings.HasPrefix(model.ID, "projects/") {
					fail(fmt.Errorf("GOOGLE_CLOUD_PROJECT or a service-account project_id is required for Vertex"))
					return
				}
				if endpoint == "" {
					endpoint = "https://aiplatform.googleapis.com"
					if location != "global" {
						endpoint = "https://" + location + "-aiplatform.googleapis.com"
					}
				}
				resource := "projects/" + project + "/locations/" + location + "/publishers/" + publisher + "/models/" + model.ID
				if strings.HasPrefix(model.ID, "projects/") {
					resource = model.ID
				} else if strings.HasPrefix(model.ID, "publishers/") {
					resource = "projects/" + project + "/locations/" + location + "/" + model.ID
				}
				segments := strings.Split(resource, "/")
				for i := range segments {
					segments[i] = url.PathEscape(segments[i])
				}
				endpoint += "/v1/" + strings.Join(segments, "/") + method
			}
		}
		model.BaseURL = endpoint
		onPayload := options.OnPayload
		options.OnPayload = func(payload any, m types.Model) (any, error) {
			p, ok := payload.(map[string]any)
			if !ok {
				return nil, fmt.Errorf("invalid Vertex request body")
			}
			delete(p, "model")
			if claude {
				p["anthropic_version"] = "vertex-2023-10-16"
				if options.Transport == "http" {
					delete(p, "stream")
				}
			}
			if onPayload != nil {
				return onPayload(p, m)
			}
			return p, nil
		}
		var upstream *stream.AssistantStream
		if claude {
			upstream = anthropic.StreamSimple(ctx, model, c, &options)
		} else {
			upstream = google.StreamSimple(ctx, model, c, &options)
		}
		// The native adapter owns the request watchdog once credentials are ready.
		watchdog.Stop()
		watching = false
		for event := range upstream.Events() {
			s.Push(event)
		}
	}()
	return s
}
