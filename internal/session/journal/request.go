package journal

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"

	"go.harness.dev/harness/tempest/durable"
)

// LockRequest serializes CLI retries before they choose or create a session.
// Keep the lock until the session closes; its writer can't be shared by processes.
func LockRequest(ctx context.Context, dir, cwd, requestID string) (*os.File, error) {
	if requestID == "" || len(requestID) > 128 || strings.ContainsRune(requestID, 0) {
		return nil, fmt.Errorf("invalid request id")
	}
	root := filepath.Join(dir, encodeCwd(cwd), ".requests")
	if err := os.MkdirAll(root, 0700); err != nil {
		return nil, err
	}
	digest := sha256.Sum256([]byte(cwd + "\x00" + requestID))
	file, err := os.OpenFile(filepath.Join(root, hex.EncodeToString(digest[:])+".lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	var retry *time.Ticker
	defer func() {
		if retry != nil {
			retry.Stop()
		}
	}()
	for {
		if err = ctx.Err(); err != nil {
			break
		}
		err = syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return file, nil
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) && !errors.Is(err, syscall.EAGAIN) {
			break
		}
		if retry == nil {
			retry = time.NewTicker(10 * time.Millisecond)
		}
		select {
		case <-ctx.Done():
		case <-retry.C:
		}
	}
	_ = file.Close()
	return nil, err
}

// ResolveRequest finds a lifetime CLI receipt in the same cwd scope as Recent.
// The caller must hold LockRequest across lookup, creation and admission.
func ResolveRequest(ctx context.Context, dir, cwd, requestID string) (string, error) {
	root := filepath.Join(dir, encodeCwd(cwd))
	entries, err := os.ReadDir(root)
	if os.IsNotExist(err) {
		return "", ErrSessionNotFound
	}
	if err != nil {
		return "", err
	}
	key := durable.SubmissionKey("", requestID)
	var found string
	for _, entry := range entries {
		if err = ctx.Err(); err != nil {
			return "", err
		}
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".jsonl") {
			continue
		}
		path := filepath.Join(root, entry.Name())
		header, err := loadHeader(ctx, path)
		if err != nil {
			return "", err
		}
		if header.Cwd != cwd || header.Version < 4 {
			continue
		}
		matched, err := catalogHasRequest(ctx, path, header.ID, key)
		if err != nil {
			return "", err
		}
		if matched {
			if found != "" {
				return "", fmt.Errorf("ambiguous request id %q: pass -resume with the original session", requestID)
			}
			found = path
		}
	}
	if found == "" {
		return "", ErrSessionNotFound
	}
	return found, nil
}

func catalogHasRequest(ctx context.Context, path, sessionID, key string) (bool, error) {
	var metadata catalogMetadata
	raw, err := os.ReadFile(path + ".catalog")
	if err == nil && json.Unmarshal(raw, &metadata) == nil && metadata.RequestsIndexed {
		info, err := os.Stat(path)
		if err != nil {
			return false, err
		}
		if metadata.JournalBytes == info.Size() {
			return slices.Contains(metadata.Requests, key), nil
		}
	} else if err != nil && !os.IsNotExist(err) {
		return false, err
	}
	// A crash can leave the rebuildable index behind its admission commit.
	contents, err := os.ReadFile(path)
	if err != nil {
		return false, err
	}
	_, log, err := expandV4(ctx, path, contents)
	if err != nil {
		return false, err
	}
	page, err := log.Read(ctx, durable.Query{SessionID: sessionID, Kind: durable.SubmissionKind, Namespace: "_tempest/submissions", ID: key, Limit: 1})
	return len(page.Records) != 0, err
}
