package extensionhost

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"io"
	"os"
	"unicode/utf8"

	"go.harness.dev/harness/tempest/environment"
	"go.harness.dev/harness/tempest/extension"
)

const maxEnvironmentRead = 64 << 10

type localReadEnvironment struct{ cwd, identity string }

func (environment localReadEnvironment) Identity() string { return environment.identity }
func (environment localReadEnvironment) ReadFile(ctx context.Context, path string, offset int64, limit int) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if offset < 0 || limit < 1 || limit > maxEnvironmentRead {
		return "", errors.New("environment read range exceeds bounds")
	}
	root, err := os.OpenRoot(environment.cwd)
	if err != nil {
		return "", err
	}
	defer root.Close()
	file, err := root.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	contents := make([]byte, limit)
	size, err := file.ReadAt(contents, offset)
	if err != nil && !errors.Is(err, io.EOF) {
		return "", err
	}
	if !utf8.Valid(contents[:size]) {
		return "", errors.New("environment text read requires UTF-8 boundaries")
	}
	return string(contents[:size]), nil
}

type leasedReadEnvironment struct {
	lease    environment.Lease
	identity string
}

func (reader leasedReadEnvironment) Identity() string { return reader.identity }

func (reader leasedReadEnvironment) ReadFile(ctx context.Context, path string, offset int64, limit int) (string, error) {
	arguments, err := json.Marshal(environment.ReadRequest{Path: path, Offset: offset, Limit: limit})
	if err != nil {
		return "", err
	}
	result, err := reader.lease.Call(ctx, environment.Call{Environment: reader.lease.Reference(), Key: reader.identity + ":section-read:" + rand.Text(), Operation: "read_bytes", Arguments: arguments})
	if err != nil {
		return "", err
	}
	var content environment.ReadResult
	if err := json.Unmarshal(result, &content); err != nil {
		return "", err
	}
	return content.Text, nil
}

func (runtime *Runtime) readEnvironment() (extension.EnvironmentLease, error) {
	if runtime.Environment == nil {
		return localReadEnvironment{cwd: runtime.Cwd, identity: runtime.EnvironmentID}, nil
	}
	if err := environment.Require(runtime.Environment, environment.Files); err != nil {
		return nil, err
	}
	return leasedReadEnvironment{lease: runtime.Environment, identity: runtime.EnvironmentID}, nil
}
