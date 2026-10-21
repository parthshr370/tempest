package base

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"go.harness.dev/harness/internal/engine/types"
	"go.harness.dev/harness/internal/engine/util"
	"go.harness.dev/harness/internal/retry"
)

// ResponseError retains HTTP status and retry hints instead of classifying body text alone.
func ResponseError(resp *http.Response, body []byte) *retry.Error {
	return retry.Classify(retry.Failure{Status: resp.StatusCode, Headers: util.HeadersToRecord(resp.Header), Message: fmt.Sprintf("provider HTTP %d: %s", resp.StatusCode, body)}, time.Now())
}

// ApplyError preserves structured converter errors and classifies transport/provider failures.
func ApplyError(output *types.AssistantMessage, err error) {
	if err == nil {
		return
	}
	output.ErrorMessage = err.Error()
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
