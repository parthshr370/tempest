package rpc

import (
	"bufio"
	"context"
	"errors"
	"io"

	"github.com/coder/websocket"
)

// NewStream wraps a line-delimited stream. Close must unblock Read and Write.
func NewStream(stream io.ReadWriteCloser) *Conn {
	return NewStreamWithLimit(stream, MaxFrame)
}

// NewStreamWithLimit applies maxFrame to inbound and outbound JSON frames.
func NewStreamWithLimit(stream io.ReadWriteCloser, maxFrame int) *Conn {
	if maxFrame <= 0 {
		maxFrame = MaxFrame
	}
	c := newConn(&lineTransport{stream: stream, reader: bufio.NewReaderSize(stream, 32<<10), maxFrame: maxFrame})
	c.maxFrame = maxFrame
	return c
}

type lineTransport struct {
	stream         io.ReadWriteCloser
	reader         *bufio.Reader
	maxFrame       int
	stopOnOversize bool
}

func (t *lineTransport) close() error { return t.stream.Close() }
func (t *lineTransport) write(_ context.Context, b []byte) error {
	b = append(b, '\n')
	for len(b) > 0 {
		n, err := t.stream.Write(b)
		if err != nil {
			return err
		}
		if n == 0 {
			return io.ErrShortWrite
		}
		b = b[n:]
	}
	return nil
}
func (t *lineTransport) read(_ context.Context) ([]byte, error) {
	var frame []byte
	oversized := false
	for {
		fragment, err := t.reader.ReadSlice('\n')
		content := fragment
		if len(content) > 0 && content[len(content)-1] == '\n' {
			content = content[:len(content)-1]
		}
		if !oversized {
			if len(frame)+len(content) > t.maxFrame {
				oversized = true
				frame = nil
				if t.stopOnOversize {
					return nil, errOversize
				}
			} else {
				frame = append(frame, content...)
			}
		}
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		if oversized {
			return nil, errOversize
		}
		if err != nil && !(errors.Is(err, io.EOF) && len(frame) > 0) {
			return nil, err
		}
		return frame, nil
	}
}

// NewWebSocket wraps a text-message WebSocket without its default closing read cap.
func NewWebSocket(ws *websocket.Conn) *Conn {
	ws.SetReadLimit(-1)
	return newConn(&wsTransport{ws: ws})
}

type wsTransport struct{ ws *websocket.Conn }

func (t *wsTransport) close() error { return t.ws.CloseNow() }
func (t *wsTransport) write(ctx context.Context, b []byte) error {
	return t.ws.Write(ctx, websocket.MessageText, b)
}
func (t *wsTransport) read(ctx context.Context) ([]byte, error) {
	kind, r, err := t.ws.Reader(ctx)
	if err != nil {
		return nil, err
	}
	b, err := io.ReadAll(io.LimitReader(r, MaxFrame+1))
	if err != nil {
		return nil, err
	}
	if len(b) > MaxFrame {
		_, err = io.Copy(io.Discard, r)
		if err != nil {
			return nil, err
		}
		return nil, errOversize
	}
	if kind != websocket.MessageText {
		return []byte("null"), nil
	}
	return b, nil
}
