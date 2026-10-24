package base

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"go.harness.dev/harness/internal/engine/types"
	"go.harness.dev/harness/internal/engine/util"
	"go.harness.dev/harness/internal/obs"
	"go.harness.dev/harness/internal/retry"
)

// ResponseError retains HTTP status and retry hints instead of classifying body text alone.
func ResponseError(resp *http.Response, body []byte) *retry.Error {
	var payload struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	_ = json.Unmarshal(body, &payload)
	return retry.Classify(retry.Failure{Status: resp.StatusCode, ProviderCode: payload.Error.Code, Headers: util.HeadersToRecord(resp.Header), Message: fmt.Sprintf("provider HTTP %d: %s", resp.StatusCode, body)}, time.Now())
}

// ApplyError preserves structured converter errors and classifies transport/provider failures.
func ApplyError(output *types.AssistantMessage, err error) {
	if err == nil {
		return
	}
	message := err.Error()
	var urlErr *url.Error
	if errors.As(err, &urlErr) {
		if endpoint, parseErr := url.Parse(urlErr.URL); parseErr == nil {
			endpoint.RawQuery, endpoint.Fragment, endpoint.User = "", "", nil
			message = strings.ReplaceAll(message, urlErr.URL, endpoint.String())
		} else {
			message = strings.ReplaceAll(message, urlErr.URL, "[REDACTED URL]")
		}
	}
	output.ErrorMessage = obs.Redact(message)
	var structured types.StructuredError
	if errors.As(err, &structured) {
		output.ErrorCode = structured.ErrorCode()
		output.ErrorDetails = structured.ErrorDetails()
		return
	}
	var classified *retry.Error
	if !errors.As(err, &classified) {
		var contextErr error
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			contextErr = err
		}
		classified = retry.Classify(retry.Failure{Message: err.Error(), StopReason: output.StopReason, ContextErr: contextErr}, time.Now())
	}
	output.ErrorCode = string(classified.Code)
}
