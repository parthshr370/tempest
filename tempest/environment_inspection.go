package tempest

import (
	"context"
	"encoding/json"

	"go.harness.dev/harness/internal/envlocal"
	"go.harness.dev/harness/internal/permission"
	"go.harness.dev/harness/tempest/environment"
)

func bindEnvironmentInspection(local *envlocal.Local) {
	local.Bind("inspect", func(ctx context.Context, call environment.Call) (json.RawMessage, error) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		var request permission.InspectionRequest
		if err := json.Unmarshal(call.Arguments, &request); err != nil {
			return nil, err
		}
		return json.Marshal(permission.InspectWorkspace(request.Tool, request.Arguments, local.LocalRoot()))
	})
}
