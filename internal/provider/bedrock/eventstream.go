package bedrock

import (
	"encoding/binary"
	"fmt"
	"hash/crc32"
	"io"
)

// readFrame validates both CRCs before exposing any event payload.
func readFrame(reader io.Reader) (map[string]string, []byte, error) {
	var prelude [12]byte
	if _, err := io.ReadFull(reader, prelude[:]); err != nil {
		return nil, nil, err
	}
	total := int(binary.BigEndian.Uint32(prelude[:4]))
	headerLen := int(binary.BigEndian.Uint32(prelude[4:8]))
	if total < 16 || headerLen > total-16 {
		return nil, nil, fmt.Errorf("invalid AWS event-stream frame lengths")
	}
	if crc32.ChecksumIEEE(prelude[:8]) != binary.BigEndian.Uint32(prelude[8:]) {
		return nil, nil, fmt.Errorf("AWS event-stream prelude CRC mismatch")
	}
	// Read only bytes the peer actually sends. An untrusted length must not
	// trigger a multi-gigabyte allocation before the body arrives.
	rest, err := io.ReadAll(io.LimitReader(reader, int64(total-12)))
	if err != nil {
		return nil, nil, err
	}
	if len(rest) != total-12 {
		return nil, nil, io.ErrUnexpectedEOF
	}
	crc := crc32.NewIEEE()
	crc.Write(prelude[:])
	crc.Write(rest[:len(rest)-4])
	if crc.Sum32() != binary.BigEndian.Uint32(rest[len(rest)-4:]) {
		return nil, nil, fmt.Errorf("AWS event-stream message CRC mismatch")
	}
	headers := map[string]string{}
	raw := rest[:headerLen]
	for len(raw) > 0 {
		n := int(raw[0])
		raw = raw[1:]
		if n == 0 || len(raw) < n+1 {
			return nil, nil, fmt.Errorf("truncated AWS event header")
		}
		name := string(raw[:n])
		kind := raw[n]
		raw = raw[n+1:]
		size := 0
		switch kind {
		case 0, 1:
			size = 0
		case 2:
			size = 1
		case 3:
			size = 2
		case 4:
			size = 4
		case 5, 8:
			size = 8
		case 9:
			size = 16
		case 6, 7:
			if len(raw) < 2 {
				return nil, nil, io.ErrUnexpectedEOF
			}
			size = int(binary.BigEndian.Uint16(raw[:2]))
			raw = raw[2:]
		default:
			return nil, nil, fmt.Errorf("invalid AWS event header type %d", kind)
		}
		if len(raw) < size {
			return nil, nil, io.ErrUnexpectedEOF
		}
		if kind == 7 {
			headers[name] = string(raw[:size])
		}
		raw = raw[size:]
	}
	return headers, rest[headerLen : len(rest)-4], nil
}
