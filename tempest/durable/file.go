package durable

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"syscall"
)

// ErrLocked means another writer owns the stable sidecar lock.
var ErrLocked = errors.New("durable store locked")

// OpenFile opens an append-only v4 file store. The stable lockfile survives
// journal replacement; readers never execute application code during recovery.
func OpenFile(ctx context.Context, path string) (*Log, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return nil, err
	}
	lock, err := os.OpenFile(path+".lock", os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err = syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		lock.Close()
		return nil, fmt.Errorf("%w: %v", ErrLocked, err)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		lock.Close()
		return nil, err
	}
	fail := func(err error) (*Log, error) { f.Close(); lock.Close(); return nil, err }
	log := NewLog(func(e Envelope) error {
		raw, err := EncodeEnvelope(path, e)
		if err != nil {
			return err
		}
		raw = append(raw, '\n')
		if len(raw) > MaxBatchBytes+4096 {
			return fmt.Errorf("envelope too large")
		}
		n, err := f.Write(raw)
		if err != nil {
			return err
		}
		if n != len(raw) {
			return io.ErrShortWrite
		}
		return f.Sync()
	}, func() error { return errors.Join(f.Close(), lock.Close()) })
	r := bufio.NewReaderSize(f, MaxBatchBytes+4096)
	var offset int64
	for {
		raw, readErr := r.ReadSlice('\n')
		if len(raw) == 0 && readErr == io.EOF {
			break
		}
		if errors.Is(readErr, bufio.ErrBufferFull) || len(raw) > MaxBatchBytes+4096 {
			return fail(fmt.Errorf("%w: oversized envelope", ErrCorrupt))
		}
		if readErr == io.EOF {
			if err = f.Truncate(offset); err != nil {
				return fail(err)
			}
			break
		}
		if readErr != nil {
			return fail(readErr)
		}
		e, decodeErr := DecodeEnvelope(path, bytes.TrimSuffix(raw, []byte{'\n'}))
		if err = decodeErr; err != nil {
			return fail(fmt.Errorf("%w: %v", ErrCorrupt, err))
		}
		if err = log.Restore(e); err != nil {
			return fail(err)
		}
		offset += int64(len(raw))
	}
	if _, err = f.Seek(0, io.SeekEnd); err != nil {
		return fail(err)
	}
	if err = f.Sync(); err != nil {
		return fail(err)
	}
	dir, err := os.Open(filepath.Dir(path))
	if err != nil {
		return fail(err)
	}
	err = dir.Sync()
	closeErr := dir.Close()
	if err = errors.Join(err, closeErr); err != nil {
		return fail(err)
	}
	return log, nil
}
