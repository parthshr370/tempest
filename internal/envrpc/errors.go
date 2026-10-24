package envrpc

import (
	"errors"
	"fmt"

	"go.harness.dev/harness/internal/rpc"
	"go.harness.dev/harness/tempest/environment"
)

const (
	identityErrorCode    = -32020
	unsupportedErrorCode = -32021
	unknownErrorCode     = -32022
)

func errorName(err *rpc.Error) string {
	switch data := err.Data.(type) {
	case map[string]any:
		name, _ := data["code"].(string)
		return name
	case map[string]string:
		return data["code"]
	default:
		return ""
	}
}

func wireError(err error) *rpc.Error {
	switch {
	case errors.Is(err, environment.ErrIdentity):
		return rpc.NewError(identityErrorCode, "environment_identity_mismatch", err.Error())
	case errors.Is(err, environment.ErrUnsupported):
		return rpc.NewError(unsupportedErrorCode, "environment_unsupported", err.Error())
	case errors.Is(err, environment.ErrUnknown):
		return rpc.NewError(unknownErrorCode, "environment_unknown", err.Error())
	default:
		return rpc.NewError(-32000, "environment_error", err.Error())
	}
}

func classifyError(err error) error {
	var response *rpc.Error
	if !errors.As(err, &response) {
		return err
	}
	switch response.Code {
	case identityErrorCode:
		return fmt.Errorf("%w: %s", environment.ErrIdentity, response.Message)
	case unsupportedErrorCode:
		return fmt.Errorf("%w: %s", environment.ErrUnsupported, response.Message)
	case unknownErrorCode:
		return fmt.Errorf("%w: %s", environment.ErrUnknown, response.Message)
	default:
		return err
	}
}
