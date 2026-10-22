package mcpwire

import (
	"bufio"
	"bytes"
	"errors"
	"io"
	"math"
	"strconv"
	"time"
	"unicode/utf8"
)

const maxEventID = 4 << 10
const defaultSSERetry = 3 * time.Second

type sseEvent struct {
	data     []byte
	id       string
	hasID    bool
	retry    time.Duration
	hasRetry bool
}

type sseReader struct{ reader *bufio.Reader }

func newSSEReader(reader io.Reader) *sseReader {
	return &sseReader{bufio.NewReaderSize(reader, 32<<10)}
}

func (s *sseReader) next() (sseEvent, error) {
	event := sseEvent{}
	for {
		var line []byte
		for {
			fragment, err := s.reader.ReadSlice('\n')
			if len(line)+len(fragment) > MaxResponse+maxEventID {
				return event, errors.New("mcp_output_limit: SSE line exceeds limit; remote outcome may be unknown")
			}
			line = append(line, fragment...)
			if errors.Is(err, bufio.ErrBufferFull) {
				continue
			}
			if err != nil {
				return event, err
			}
			break
		}
		line = bytes.TrimSuffix(line, []byte{'\n'})
		line = bytes.TrimSuffix(line, []byte{'\r'})
		if !utf8.Valid(line) {
			return event, errors.New("mcp_protocol: SSE is not UTF-8")
		}
		if len(line) == 0 {
			if len(event.data) > 0 {
				event.data = event.data[:len(event.data)-1]
			}
			return event, nil
		}
		if line[0] == ':' {
			continue
		}
		field, value, _ := bytes.Cut(line, []byte{':'})
		value = bytes.TrimPrefix(value, []byte{' '})
		switch string(field) {
		case "data":
			if len(event.data)+len(value) > MaxResponse {
				return event, errors.New("mcp_output_limit: SSE event exceeds 16 MiB; remote outcome may be unknown")
			}
			event.data = append(event.data, value...)
			event.data = append(event.data, '\n')
		case "id":
			if bytes.IndexByte(value, 0) >= 0 {
				continue
			}
			if len(value) > maxEventID {
				return event, errors.New("mcp_protocol: SSE id exceeds 4 KiB")
			}
			event.id = string(value)
			event.hasID = true
		case "retry":
			digits := len(value) > 0
			for _, r := range value {
				if r < '0' || r > '9' {
					digits = false
					break
				}
			}
			if digits {
				milliseconds, err := strconv.ParseUint(string(value), 10, 64)
				if err != nil || milliseconds > uint64(math.MaxInt64/int64(time.Millisecond)) {
					event.retry = time.Duration(math.MaxInt64)
				} else {
					event.retry = time.Duration(milliseconds) * time.Millisecond
				}
				event.hasRetry = true
			}
		}
	}
}
