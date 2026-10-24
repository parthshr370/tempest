package adapter

import (
	"context"
	"fmt"
	"net/url"
	"strings"

	openaiauth "go.harness.dev/harness/internal/auth/openai"
)

// RequestCredential holds only request-local access, not a renewable token set.
type RequestCredential = openaiauth.Credential

// OpenAIChatGPTCredentials opts into the local Sign in with ChatGPT store.
// The profile is fixed when this source is created, not reselected on refresh.
func OpenAIChatGPTCredentials(profile string) func(context.Context) (RequestCredential, error) {
	authManager, err := openaiauth.Default()
	if err == nil {
		profile, err = authManager.Profile(profile)
	}
	return func(ctx context.Context) (RequestCredential, error) {
		if err != nil {
			return RequestCredential{}, err
		}
		return authManager.Access(ctx, profile)
	}
}

func validateChatGPTCredential(credential RequestCredential) error {
	if credential.AccessToken == "" || strings.ContainsAny(credential.AccessToken, "\r\n") {
		return fmt.Errorf("OpenAI sign-in credential is missing or invalid; run tempest login openai")
	}
	if credential.BaseURL == openaiauth.Resource {
		return nil
	}
	endpoint, err := url.Parse(credential.BaseURL)
	if err != nil || endpoint.Scheme != "http" || endpoint.Hostname() != "127.0.0.1" || endpoint.Port() == "" || endpoint.Path != "/v1" || endpoint.User != nil || endpoint.RawQuery != "" || endpoint.Fragment != "" {
		return fmt.Errorf("OpenAI sign-in credential is not bound to the public Responses endpoint")
	}
	return nil
}
