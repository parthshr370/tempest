// Package dap implements the Debug Adapter Protocol without adapter dependencies.
package dap

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"sync"
)

const (
	maxMessage     = 16 << 20
	maxHeaderBytes = 8 << 10
)

type message struct {
	Seq        int             `json:"seq"`
	Type       string          `json:"type"`
	RequestSeq int             `json:"request_seq,omitempty"`
	Command    string          `json:"command,omitempty"`
	Event      string          `json:"event,omitempty"`
	Success    bool            `json:"success"`
	Message    string          `json:"message,omitempty"`
	Arguments  json.RawMessage `json:"arguments,omitempty"`
	Body       json.RawMessage `json:"body,omitempty"`
}

func readMessage(r *bufio.Reader) (message, error) {
	length := -1
	total := 0
	for {
		header, err := r.ReadSlice('\n')
		line := string(header)
		if err != nil {
			return message{}, err
		}
		total += len(line)
		if total > maxHeaderBytes {
			return message{}, errors.New("DAP headers exceed 8 KiB")
		}
		if line == "\r\n" {
			break
		}
		name, value, ok := strings.Cut(strings.TrimSpace(line), ":")
		if !ok {
			return message{}, errors.New("invalid DAP header")
		}
		if strings.EqualFold(name, "Content-Length") {
			if length != -1 {
				return message{}, errors.New("duplicate DAP Content-Length")
			}
			length, err = strconv.Atoi(strings.TrimSpace(value))
			if err != nil || length < 1 || length > maxMessage {
				return message{}, errors.New("invalid DAP Content-Length")
			}
		}
	}
	if length < 1 {
		return message{}, errors.New("missing DAP Content-Length")
	}
	data := make([]byte, length)
	if _, err := io.ReadFull(r, data); err != nil {
		return message{}, err
	}
	var m message
	err := json.Unmarshal(data, &m)
	return m, err
}

func writeMessage(w io.Writer, m message) error {
	data, err := json.Marshal(m)
	if err != nil {
		return err
	}
	header := fmt.Sprintf("Content-Length: %d\r\n\r\n", len(data))
	if _, err = io.WriteString(w, header); err != nil {
		return err
	}
	n, err := w.Write(data)
	if err == nil && n != len(data) {
		err = io.ErrShortWrite
	}
	return err
}

type client struct {
	mu              sync.Mutex
	writeMu         sync.Mutex
	writer          io.Writer
	seq             int
	pending         map[int]chan message
	done            chan struct{}
	err             error
	initialized     chan struct{}
	initializedOnce sync.Once
	onEvent         func(message)
}

func newClient(r io.Reader, w io.Writer, onEvent func(message)) *client {
	c := &client{writer: w, pending: map[int]chan message{}, done: make(chan struct{}), initialized: make(chan struct{}), onEvent: onEvent}
	go c.read(r)
	return c
}
func (c *client) read(r io.Reader) {
	reader := bufio.NewReaderSize(r, maxHeaderBytes)
	var readErr error
	defer func() {
		c.mu.Lock()
		c.err = readErr
		close(c.done)
		c.mu.Unlock()
	}()
	for {
		m, err := readMessage(reader)
		if err != nil {
			readErr = err
			return
		}
		switch m.Type {
		case "response":
			c.mu.Lock()
			ch := c.pending[m.RequestSeq]
			delete(c.pending, m.RequestSeq)
			c.mu.Unlock()
			if ch != nil {
				ch <- m
			}
		case "event":
			if m.Event == "initialized" {
				c.initializedOnce.Do(func() { close(c.initialized) })
			}
			if c.onEvent != nil {
				c.onEvent(m)
			}
		case "request":
			c.writeMu.Lock()
			c.mu.Lock()
			c.seq++
			seq := c.seq
			c.mu.Unlock()
			err = writeMessage(c.writer, message{Seq: seq, Type: "response", RequestSeq: m.Seq, Command: m.Command, Message: "reverse requests are not supported; use an internal console"})
			c.writeMu.Unlock()
			if err != nil {
				readErr = err
				return
			}
		}
	}
}
func (c *client) start(ctx context.Context, command string, args json.RawMessage) (func() (json.RawMessage, error), error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	c.mu.Lock()
	select {
	case <-c.done:
		err := c.err
		c.mu.Unlock()
		return nil, err
	default:
	}
	c.seq++
	seq := c.seq
	ch := make(chan message, 1)
	c.pending[seq] = ch
	c.mu.Unlock()
	written := make(chan error, 1)
	go func() {
		c.writeMu.Lock()
		err := writeMessage(c.writer, message{Seq: seq, Type: "request", Command: command, Arguments: args})
		c.writeMu.Unlock()
		written <- err
	}()
	var err error
	select {
	case err = <-written:
	case <-ctx.Done():
		err = ctx.Err()
	case <-c.done:
		c.mu.Lock()
		err = c.err
		c.mu.Unlock()
	}
	if err != nil {
		c.mu.Lock()
		delete(c.pending, seq)
		c.mu.Unlock()
		return nil, err
	}
	return func() (json.RawMessage, error) {
		defer func() { c.mu.Lock(); delete(c.pending, seq); c.mu.Unlock() }()
		select {
		case m := <-ch:
			if !m.Success {
				return nil, fmt.Errorf("DAP %s: %s", command, m.Message)
			}
			return m.Body, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-c.done:
			c.mu.Lock()
			err := c.err
			c.mu.Unlock()
			return nil, err
		}
	}, nil
}
func (c *client) request(ctx context.Context, command string, args json.RawMessage) (json.RawMessage, error) {
	wait, err := c.start(ctx, command, args)
	if err != nil {
		return nil, err
	}
	return wait()
}
