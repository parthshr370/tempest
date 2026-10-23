package jobs

import (
	"errors"
	"os"
	"os/exec"
	"syscall"
)

// processStreams are a bash job's descriptors: the worker's ends, which it
// reads output from and writes input to, and the child's ends, which the
// parent closes once the child holds its copies.
type processStreams struct {
	output []*os.File // stdout and stderr pipes, or the one pty master
	input  *os.File   // stdin pipe write end, the same pty master, or nil
	child  []*os.File
}

// openStreams gives cmd its stdin, stdout and stderr. Pipes keep the job in
// its own process group, and its stdin is /dev/null unless stdin asks for a
// pipe, as for any background process: a command that reads input sees end
// of file instead of waiting for input nobody sends. A pty makes the child a
// session leader with the terminal as its controlling tty, so its group id
// is still its pid, and line-discipline signals such as Ctrl-C reach its
// foreground group.
func openStreams(cmd *exec.Cmd, pty, stdin bool) (processStreams, error) {
	if pty {
		master, tty, err := openPTY()
		if err != nil {
			return processStreams{}, err
		}
		cmd.Stdin, cmd.Stdout, cmd.Stderr = tty, tty, tty
		cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setctty: true, Ctty: 0}
		cmd.Env = append(cmd.Environ(), ptyEnvironment...)
		return processStreams{output: []*os.File{master}, input: master, child: []*os.File{tty}}, nil
	}
	var streams processStreams
	if stdin {
		read, write, err := os.Pipe()
		if err != nil {
			return processStreams{}, err
		}
		cmd.Stdin, streams.input, streams.child = read, write, []*os.File{read}
	}
	var writers [2]*os.File // stdout, then stderr
	for index := range writers {
		read, write, err := os.Pipe()
		if err != nil {
			return processStreams{}, errors.Join(err, streams.closeAll())
		}
		streams.output = append(streams.output, read)
		streams.child = append(streams.child, write)
		writers[index] = write
	}
	cmd.Stdout, cmd.Stderr = writers[0], writers[1]
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	return streams, nil
}

func (streams processStreams) closeChild() error {
	var err error
	for _, file := range streams.child {
		err = errors.Join(err, file.Close())
	}
	return err
}

// closeAll releases every descriptor when the child never started.
func (streams processStreams) closeAll() error {
	err := streams.closeChild()
	for _, file := range streams.output {
		err = errors.Join(err, file.Close())
	}
	if streams.input != nil && (len(streams.output) == 0 || streams.input != streams.output[0]) {
		err = errors.Join(err, streams.input.Close())
	}
	return err
}
