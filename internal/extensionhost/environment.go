package extensionhost

import (
	"context"
	"errors"
	"io"
	"os"
	"unicode/utf8"
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
