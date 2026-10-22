package worktree

import (
	"bytes"
	"context"
	"fmt"
	"io"
)

type presenceWriter struct{ present bool }

func (writer *presenceWriter) Write(output []byte) (int, error) {
	writer.present = writer.present || len(output) > 0
	return len(output), nil
}

// gitlinkWriter only inspects each index entry's mode; repository size doesn't grow memory.
type gitlinkWriter struct {
	prefix [7]byte
	count  int
	found  bool
}

func (writer *gitlinkWriter) Write(output []byte) (int, error) {
	length := len(output)
	for len(output) > 0 && !writer.found {
		if writer.count < len(writer.prefix) {
			copied := copy(writer.prefix[writer.count:], output)
			writer.count += copied
			output = output[copied:]
			if writer.count == len(writer.prefix) && string(writer.prefix[:]) == "160000 " {
				writer.found = true
			}
		} else {
			newline := bytes.IndexByte(output, '\n')
			if newline < 0 {
				break
			}
			writer.count = 0
			output = output[newline+1:]
		}
	}
	return length, nil
}
func streamGit(ctx context.Context, cwd string, writer io.Writer, args ...string) error {
	command := command(ctx, cwd, nil, args...)
	command.Stdout = writer
	var diagnostic bytes.Buffer
	command.Stderr = &boundedWriter{w: &diagnostic, left: 4096}
	if err := command.Run(); err != nil {
		return fmt.Errorf("git %s: %w: %s", args[0], err, diagnostic.String())
	}
	return nil
}
func hasGitlinks(ctx context.Context, repo string) (bool, error) {
	writer := &gitlinkWriter{}
	err := streamGit(ctx, repo, writer, "ls-files", "--stage")
	return writer.found, err
}
func hasStatus(ctx context.Context, cwd string, ignored bool) (bool, error) {
	writer := &presenceWriter{}
	args := []string{"status", "--porcelain", "--untracked-files=all"}
	if ignored {
		args = append(args, "--ignored")
	}
	err := streamGit(ctx, cwd, writer, args...)
	return writer.present, err
}
