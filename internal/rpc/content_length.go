package rpc

import (
	"bufio"
	"context"
	"errors"
	"io"
	"mime"
	"strconv"
	"strings"
)

// MaxContentLength is the largest body a Content-Length stream accepts.
const MaxContentLength = 8 << 20

const (
	maxHeaderSection = 8 << 10
	maxHeaderLines   = 16
)

// framingError is a corrupt Content-Length frame. The stream can't resynchronize.
type framingError struct{ detail string }

func (e *framingError) Error() string { return "invalid frame: " + e.detail }

// NewContentLengthStream wraps a stream framed by the LSP base protocol:
// ASCII headers, a blank line, then exactly Content-Length bytes of JSON.
// maxBody caps inbound and outbound bodies; values outside 1..MaxContentLength
// select MaxContentLength. Strict-mode faults use the lsp_ prefix.
func NewContentLengthStream(stream io.ReadWriteCloser, maxBody int) *Conn {
	if maxBody <= 0 || maxBody > MaxContentLength {
		maxBody = MaxContentLength
	}
	c := newConn(&contentLengthTransport{stream: stream, reader: bufio.NewReaderSize(stream, 64<<10), maxBody: maxBody})
	c.maxFrame = maxBody
	c.protocol = "lsp"
	return c
}

type contentLengthTransport struct {
	stream  io.ReadWriteCloser
	reader  *bufio.Reader
	maxBody int
	// broken holds the first framing failure; every later read repeats it.
	broken error
}

func (t *contentLengthTransport) close() error { return t.stream.Close() }

// write runs only on the connection's single writer goroutine, so the header
// and body can't interleave with another frame.
func (t *contentLengthTransport) write(_ context.Context, body []byte) error {
	var header [40]byte
	h := append(header[:0], "Content-Length: "...)
	h = strconv.AppendInt(h, int64(len(body)), 10)
	h = append(h, "\r\n\r\n"...)
	if err := writeAll(t.stream, h); err != nil {
		return err
	}
	return writeAll(t.stream, body)
}

func writeAll(w io.Writer, b []byte) error {
	for len(b) > 0 {
		n, err := w.Write(b)
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

func (t *contentLengthTransport) read(context.Context) ([]byte, error) {
	if t.broken != nil {
		return nil, t.broken
	}
	body, err := t.readFrame()
	var framing *framingError
	if errors.As(err, &framing) || errors.Is(err, errOversize) {
		t.broken = err
		if errors.Is(err, errOversize) {
			t.broken = &framingError{detail: "stream unusable after an oversized frame"}
		}
	}
	return body, err
}

func (t *contentLengthTransport) readFrame() ([]byte, error) {
	length := -1
	sawType := false
	headerBytes := 0
	lines := 0
	for {
		line, err := t.reader.ReadSlice('\n')
		headerBytes += len(line)
		if headerBytes > maxHeaderSection {
			return nil, &framingError{detail: "header section exceeds 8 KiB"}
		}
		if err != nil {
			if errors.Is(err, bufio.ErrBufferFull) {
				return nil, &framingError{detail: "header section exceeds 8 KiB"}
			}
			if errors.Is(err, io.EOF) {
				if headerBytes == 0 {
					return nil, io.EOF
				}
				return nil, &framingError{detail: "truncated header"}
			}
			return nil, err
		}
		if len(line) < 2 || line[len(line)-2] != '\r' {
			return nil, &framingError{detail: "header line must end with CRLF"}
		}
		line = line[:len(line)-2]
		if len(line) == 0 {
			break
		}
		lines++
		if lines > maxHeaderLines {
			return nil, &framingError{detail: "more than 16 headers"}
		}
		for _, b := range line {
			if (b < 0x20 && b != '\t') || b > 0x7e {
				return nil, &framingError{detail: "header contains non-ASCII or control bytes"}
			}
		}
		name, value, ok := strings.Cut(string(line), ":")
		if !ok || name == "" || strings.ContainsAny(name, " \t") {
			return nil, &framingError{detail: "malformed header"}
		}
		value = strings.Trim(value, " \t")
		switch {
		case strings.EqualFold(name, "Content-Length"):
			if length >= 0 {
				return nil, &framingError{detail: "duplicate Content-Length"}
			}
			if value == "" || len(value) > 10 || strings.Trim(value, "0123456789") != "" {
				return nil, &framingError{detail: "invalid Content-Length"}
			}
			n, parseErr := strconv.ParseInt(value, 10, 64)
			if parseErr != nil || n < 1 {
				return nil, &framingError{detail: "invalid Content-Length"}
			}
			if n > int64(t.maxBody) {
				return nil, errOversize
			}
			length = int(n)
		case strings.EqualFold(name, "Content-Type"):
			if sawType {
				return nil, &framingError{detail: "duplicate Content-Type"}
			}
			sawType = true
			if !acceptedContentType(value) {
				return nil, &framingError{detail: "unsupported Content-Type"}
			}
		}
	}
	if length < 0 {
		return nil, &framingError{detail: "missing Content-Length"}
	}
	// The declared length was validated above, so this allocation is bounded.
	body := make([]byte, length)
	if _, err := io.ReadFull(t.reader, body); err != nil {
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			return nil, &framingError{detail: "truncated body"}
		}
		return nil, err
	}
	return body, nil
}

func acceptedContentType(value string) bool {
	media, params, err := mime.ParseMediaType(value)
	if err != nil || media != "application/vscode-jsonrpc" {
		return false
	}
	charset, ok := params["charset"]
	if !ok {
		return true
	}
	charset = strings.ToLower(charset)
	return charset == "utf-8" || charset == "utf8"
}
