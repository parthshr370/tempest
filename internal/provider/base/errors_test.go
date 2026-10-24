package base

import (
	"errors"
	"fmt"
	"net/url"
	"strings"
	"testing"

	"go.harness.dev/harness/internal/engine/types"
)

func TestApplyErrorStripsURLCredentials(t *testing.T) {
	err := fmt.Errorf("request failed: %w", &url.Error{Op: "Post", URL: "https://user:password@example.test/api?custom=private-value#secret", Err: errors.New("connection refused")})
	var out types.AssistantMessage
	ApplyError(&out, err)
	if strings.Contains(out.ErrorMessage, "private-value") || strings.Contains(out.ErrorMessage, "password") || strings.Contains(out.ErrorMessage, "#secret") || !strings.Contains(out.ErrorMessage, "https://example.test/api") || out.ErrorCode != "network_failure" {
		t.Fatalf("unsafe/classification-losing error: %+v", out)
	}
}
