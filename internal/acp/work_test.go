package acp

import (
	"errors"
	"testing"

	"go.harness.dev/harness/tempest"
	"go.harness.dev/harness/tempest/durable"
)

func TestWorkErrorsKeepExactExtensionCodes(t *testing.T) {
	cases := []struct {
		err  error
		code int
		name string
	}{
		{tempest.ErrQueueFull, -32014, "queue_full"}, {tempest.ErrQueueConflict, -32015, "queue_conflict"},
		{tempest.ErrQueueNotFound, -32016, "queue_not_found"}, {tempest.ErrQueueTooLate, -32017, "queue_too_late"},
		{tempest.ErrJobNotFound, -32018, "job_not_found"}, {tempest.ErrJobLimit, -32019, "job_limit"},
		{tempest.ErrJobNotCleanupable, -32020, "job_not_cleanupable"}, {tempest.ErrRecoveryRequired, -32021, "recovery_required"},
		{tempest.ErrInvalidQueueInput, -32602, "invalid_params"}, {tempest.ErrInvalidJobInput, -32602, "invalid_params"},
		{durable.ErrConflict, -32024, "durable_conflict"},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			wire := failure(errors.Join(errors.New("context"), test.err))
			if wire.Code != test.code {
				t.Fatalf("code=%d want %d", wire.Code, test.code)
			}
		})
	}
}
