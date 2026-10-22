package journal

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

var (
	// ErrRunNotFound reports that bounded inspection found no matching run.
	ErrRunNotFound = fmt.Errorf("run not found: %w", fs.ErrNotExist)
	// ErrLookupLimit reports that journal inspection exceeded its fixed bounds.
	ErrLookupLimit = errors.New("run lookup limit")
	// ErrRunBusy reports an unfinished run owned by a different writer.
	ErrRunBusy = errors.New("task owner elsewhere")
	// ErrRunAmbiguous reports duplicate run identity in different sessions.
	ErrRunAmbiguous = errors.New("ambiguous run id")
)

// LookupRun inspects only top-level workspace journals without opening a writer or repairing tails.
func LookupRun(ctx context.Context, dir, cwd, runID string) (RunDetail, error) {
	if strings.TrimSpace(runID) == "" {
		return RunDetail{}, ErrRunNotFound
	}
	lookupCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	d, err := os.Open(filepath.Join(dir, encodeCwd(cwd)))
	if os.IsNotExist(err) {
		return RunDetail{}, ErrRunNotFound
	}
	if err != nil {
		return RunDetail{}, err
	}
	defer d.Close()
	var found *RunDetail
	var busy bool
	var bytesRead int64
	files := 0
	for {
		if err := lookupCtx.Err(); err != nil {
			if ctx.Err() != nil {
				return RunDetail{}, ctx.Err()
			}
			return RunDetail{}, ErrLookupLimit
		}
		batch, err := d.ReadDir(128)
		if err != nil && err != io.EOF {
			return RunDetail{}, err
		}
		for _, entry := range batch {
			if entry.IsDir() || entry.Type()&os.ModeSymlink != 0 || !strings.HasSuffix(entry.Name(), ".jsonl") {
				continue
			}
			files++
			if files > 10000 {
				return RunDetail{}, ErrLookupLimit
			}
			path := filepath.Join(dir, encodeCwd(cwd), entry.Name())
			f, err := os.Open(path)
			if err != nil {
				return RunDetail{}, err
			}
			info, err := f.Stat()
			if err != nil {
				_ = f.Close()
				return RunDetail{}, err
			}
			size := info.Size()
			if size < 0 || size > 256<<20-bytesRead {
				_ = f.Close()
				return RunDetail{}, ErrLookupLimit
			}
			contents, err := io.ReadAll(io.LimitReader(f, size))
			bytesRead += int64(len(contents))
			if err != nil {
				_ = f.Close()
				return RunDetail{}, err
			}
			loaded, err := loadContents(lookupCtx, path, contents, false)
			if err != nil {
				_ = f.Close()
				if lookupCtx.Err() != nil && ctx.Err() == nil {
					return RunDetail{}, ErrLookupLimit
				}
				return RunDetail{}, err
			}
			if loaded.Header.Cwd == cwd {
				for _, r := range loaded.Runs {
					if r.RunID != runID {
						continue
					}
					if found != nil {
						_ = f.Close()
						return RunDetail{}, ErrRunAmbiguous
					}
					copy := r
					found = &copy
					if !r.Finished {
						err := syscall.Flock(int(f.Fd()), syscall.LOCK_SH|syscall.LOCK_NB)
						if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
							busy = true
						} else if err != nil {
							_ = f.Close()
							return RunDetail{}, err
						}
					}
				}
			}
			_ = f.Close()
		}
		if err == io.EOF {
			break
		}
	}
	if found == nil {
		return RunDetail{}, ErrRunNotFound
	}
	if busy {
		return RunDetail{}, ErrRunBusy
	}
	return *found, nil
}
