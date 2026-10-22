package a2a

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"sort"
	"strings"
	"time"

	model "github.com/a2aproject/a2a-go/v2/a2a"
)

const (
	defaultPageSize    = 50
	maxPageSize        = 100
	streamWriteTimeout = 10 * time.Second
)

func (s *Server) stream(w http.ResponseWriter, r *http.Request, id json.RawMessage, t *task, history *int, rejectTerminal bool) {
	t.refreshAsks()
	t.mu.Lock()
	if rejectTerminal && t.value.Status.State.Terminal() {
		t.mu.Unlock()
		writeResponse(w, id, nil, protocolError(-32004, "terminal_task", nil))
		return
	}
	first := t.snapshotLocked(history, true)
	sub := &subscriber{events: make(chan model.Event, taskQueueSize)}
	if !t.value.Status.State.Terminal() {
		t.subscribers[sub] = struct{}{}
	} else {
		close(sub.events)
	}
	t.mu.Unlock()
	defer func() { t.mu.Lock(); delete(t.subscribers, sub); t.mu.Unlock() }()
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	controller := http.NewResponseController(w)
	defer func() { _ = controller.SetWriteDeadline(time.Time{}) }()
	send := func(event model.Event, e *rpcError) bool {
		var result json.RawMessage
		var err error
		if event != nil {
			result, err = json.Marshal(model.StreamResponse{Event: event})
			if err != nil {
				e = protocolError(-32006, "invalid_projection", nil)
			}
		}
		raw, err := json.Marshal(response{JSONRPC: "2.0", ID: id, Result: result, Error: e})
		if err != nil {
			return false
		}
		_ = controller.SetWriteDeadline(time.Now().Add(streamWriteTimeout))
		if _, err = w.Write(append(append([]byte("data: "), raw...), '\n', '\n')); err != nil {
			return false
		}
		return controller.Flush() == nil
	}
	if !send(first, nil) {
		return
	}
	for {
		select {
		case <-r.Context().Done():
			return
		case event, ok := <-sub.events:
			t.mu.Lock()
			gap := sub.gap
			t.mu.Unlock()
			if gap {
				send(nil, protocolError(-32004, "stream_gap", nil))
				return
			}
			if !ok {
				return
			}
			if !send(event, nil) {
				return
			}
		}
	}
}

type pageCursor struct {
	Fingerprint string       `json:"f"`
	Watermark   uint64       `json:"w"`
	Timestamp   time.Time    `json:"t"`
	ID          model.TaskID `json:"i"`
}

func (s *Server) encodeCursor(c pageCursor) string {
	raw, _ := json.Marshal(c)
	mac := hmac.New(sha256.New, s.cursorKey[:])
	_, _ = mac.Write(raw)
	return base64.RawURLEncoding.EncodeToString(raw) + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}
func (s *Server) decodeCursor(token string) (pageCursor, bool) {
	var c pageCursor
	pieces := strings.Split(token, ".")
	if len(pieces) != 2 {
		return c, false
	}
	raw, e := base64.RawURLEncoding.DecodeString(pieces[0])
	if e != nil {
		return c, false
	}
	sig, e := base64.RawURLEncoding.DecodeString(pieces[1])
	if e != nil {
		return c, false
	}
	mac := hmac.New(sha256.New, s.cursorKey[:])
	_, _ = mac.Write(raw)
	if !hmac.Equal(sig, mac.Sum(nil)) {
		return c, false
	}
	if json.Unmarshal(raw, &c) != nil || c.ID == "" || c.Timestamp.IsZero() {
		return c, false
	}
	return c, true
}
func validState(state model.TaskState) bool {
	switch state {
	case model.TaskStateUnspecified, model.TaskStateSubmitted, model.TaskStateWorking, model.TaskStateCompleted, model.TaskStateFailed, model.TaskStateCanceled, model.TaskStateInputRequired, model.TaskStateRejected, model.TaskStateAuthRequired:
		return true
	}
	return false
}
func (s *Server) list(raw json.RawMessage) (*model.ListTasksResponse, *rpcError) {
	var p model.ListTasksRequest
	var fields map[string]json.RawMessage
	if json.Unmarshal(raw, &p) != nil || json.Unmarshal(raw, &fields) != nil || !validHistory(p.HistoryLength) {
		return nil, invalid()
	}
	_, hasStatus := fields["status"]
	if status, ok := fields["status"]; ok {
		var wire string
		if json.Unmarshal(status, &wire) != nil || (wire != "TASK_STATE_UNSPECIFIED" && (wire == "" || !validState(model.TaskState(wire)))) {
			return nil, invalid()
		}
	}
	if _, ok := fields["pageSize"]; ok && (p.PageSize < 1 || p.PageSize > maxPageSize) {
		return nil, invalid()
	}
	if p.PageSize == 0 {
		p.PageSize = defaultPageSize
	}
	token := p.PageToken
	p.PageToken = ""
	// SDK wire timestamps use milliseconds. Cursor identity must preserve
	// the actual filter precision, not collapse distinct nanosecond cutoffs.
	type filters model.ListTasksRequest
	fingerprintRaw, _ := json.Marshal(struct {
		Request   filters
		HasStatus bool
	}{filters(p), hasStatus})
	digest := sha256.Sum256(fingerprintRaw)
	fingerprint := hex.EncodeToString(digest[:])
	s.mu.Lock()
	s.expireLocked()
	watermark := s.serial
	var cursor pageCursor
	if token != "" {
		var ok bool
		cursor, ok = s.decodeCursor(token)
		if !ok || cursor.Fingerprint != fingerprint || cursor.Watermark > watermark {
			s.mu.Unlock()
			return nil, invalid()
		}
		watermark = cursor.Watermark
	}
	tasks := make([]*task, 0, len(s.tasks))
	for _, t := range s.tasks {
		if t.serial <= watermark {
			tasks = append(tasks, t)
		}
	}
	s.mu.Unlock()
	eligible := make([]*model.Task, 0, len(tasks))
	for _, t := range tasks {
		v := t.snapshot(p.HistoryLength, p.IncludeArtifacts)
		if p.ContextID != "" && v.ContextID != p.ContextID {
			continue
		}
		if hasStatus && v.Status.State != p.Status {
			continue
		}
		if p.StatusTimestampAfter != nil && v.Status.Timestamp.Before(*p.StatusTimestampAfter) {
			continue
		}
		eligible = append(eligible, v)
	}
	sort.Slice(eligible, func(i, j int) bool {
		a, b := eligible[i], eligible[j]
		if a.Status.Timestamp.Equal(*b.Status.Timestamp) {
			return a.ID > b.ID
		}
		return a.Status.Timestamp.After(*b.Status.Timestamp)
	})
	result := &model.ListTasksResponse{Tasks: []*model.Task{}, PageSize: p.PageSize, TotalSize: len(eligible)}
	for _, v := range eligible {
		if token != "" && (v.Status.Timestamp.After(cursor.Timestamp) || (v.Status.Timestamp.Equal(cursor.Timestamp) && v.ID >= cursor.ID)) {
			continue
		}
		if len(result.Tasks) == p.PageSize {
			last := result.Tasks[len(result.Tasks)-1]
			result.NextPageToken = s.encodeCursor(pageCursor{Fingerprint: fingerprint, Watermark: watermark, Timestamp: *last.Status.Timestamp, ID: last.ID})
			break
		}
		result.Tasks = append(result.Tasks, v)
	}
	return result, nil
}
