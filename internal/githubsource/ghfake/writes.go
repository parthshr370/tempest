package ghfake

import (
	"cmp"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"sync"
)

// Writes is the write side of a Repo: conversation comments, review
// replies, pull request creation, listing and edits, and the git ref read.
// Branch heads come from a bare git repository standing in for GitHub's git
// storage, so a test pushes with real git and the API sees the result.
//
// LoseAck injects the fault a write path must survive: GitHub applied the
// write, then the reply was lost.
type Writes struct {
	repo *Repo
	bare string
	mu   sync.Mutex
	lost []*fault
}

// ServeWrites registers repo's write routes and GET git/ref on server.
// bare is the bare repository that branch heads are read from; empty reads
// the heads set with SetBranch.
func (r *Repo) ServeWrites(server *Server, bare string) *Writes {
	writes := &Writes{repo: r, bare: bare}
	prefix := "/repos/" + r.fullName
	handlers := map[string]http.HandlerFunc{
		"POST " + prefix + "/issues/{number}/comments":                  writes.createComment,
		"POST " + prefix + "/pulls/{number}/comments/{comment}/replies": writes.createReply,
		"POST " + prefix + "/pulls":                                     writes.createPull,
		"GET " + prefix + "/pulls":                                      writes.listPulls,
		"PATCH " + prefix + "/pulls/{number}":                           writes.updatePull,
		"GET " + prefix + "/git/ref/{ref...}":                           writes.getRef,
	}
	for pattern, handler := range handlers {
		server.HandleFunc(pattern, writes.lossy(r.rated(handler)))
	}
	return writes
}

// LoseAck applies the next times requests for method and path (no query)
// and then loses their reply. Status 0 drops the connection, so gh prints no
// status line; another status replaces the reply, like a gateway error
// after GitHub committed the write.
func (w *Writes) LoseAck(method, path string, status, times int) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.lost = append(w.lost, &fault{method: method, path: path, status: status, remaining: times})
}

func (w *Writes) takeLoss(method, path string) *fault {
	w.mu.Lock()
	defer w.mu.Unlock()
	for _, candidate := range w.lost {
		if candidate.remaining > 0 && candidate.method == method && candidate.path == path {
			candidate.remaining--
			return candidate
		}
	}
	return nil
}

func (w *Writes) lossy(handler http.HandlerFunc) http.HandlerFunc {
	return func(rw http.ResponseWriter, request *http.Request) {
		lost := w.takeLoss(request.Method, request.URL.Path)
		if lost == nil {
			handler(rw, request)
			return
		}
		// The write is applied; only its reply is lost.
		handler(httptest.NewRecorder(), request)
		if lost.status != 0 {
			WriteJSON(rw, lost.status, map[string]string{"message": http.StatusText(lost.status)})
			return
		}
		hijacker, ok := rw.(http.Hijacker)
		if !ok {
			WriteJSON(rw, http.StatusInternalServerError, map[string]string{"message": "cannot drop the connection"})
			return
		}
		conn, _, err := hijacker.Hijack()
		if err == nil {
			_ = conn.Close()
		}
	}
}

// Comments of an issue are listed by Repo.Comments; these list the rest of
// what writes create.

// ReviewComments returns the inline comments of a pull request in ID order,
// including ones still hidden by the index delay.
func (r *Repo) ReviewComments(number int) []ReviewComment {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []ReviewComment
	for _, comment := range r.reviewComment {
		if comment.number == number {
			out = append(out, *comment)
		}
	}
	return out
}

// Pulls returns every pull request in number order.
func (r *Repo) Pulls() []Pull {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []Pull
	for _, found := range r.threads {
		if found.pull != nil {
			out = append(out, r.pullView(found))
		}
	}
	slices.SortFunc(out, func(a, b Pull) int { return cmp.Compare(a.Number, b.Number) })
	return out
}

func (row Pull) order() rowOrder { return rowOrder{int64(row.Number), row.CreatedAt, row.UpdatedAt} }

// decode reads a JSON request body; false after a 400 was answered.
func decode(rw http.ResponseWriter, request *http.Request, out any) bool {
	if err := json.NewDecoder(request.Body).Decode(out); err != nil {
		WriteJSON(rw, http.StatusBadRequest, map[string]string{"message": "Problems parsing JSON"})
		return false
	}
	return true
}

func (w *Writes) createComment(rw http.ResponseWriter, request *http.Request) {
	var input struct {
		Body string `json:"body"`
	}
	if !decode(rw, request, &input) {
		return
	}
	r := w.repo
	r.mu.Lock()
	defer r.mu.Unlock()
	found, ok := r.number(rw, request)
	if !ok {
		return
	}
	if strings.TrimSpace(input.Body) == "" {
		invalid(rw, "Validation Failed: body is missing")
		return
	}
	id := r.newID()
	comment := &Comment{
		ID: id, Body: input.Body, User: r.account, CreatedAt: r.now, UpdatedAt: r.now,
		HTMLURL: fmt.Sprintf("%s#issuecomment-%d", found.issue.HTMLURL, id), IssueURL: r.issueURL(found.issue.Number),
		number: found.issue.Number, visibleAt: r.now.Add(r.indexDelay),
	}
	r.comments = append(r.comments, comment)
	found.issue.UpdatedAt = r.now
	WriteJSON(rw, http.StatusCreated, comment)
}

// createReply answers a top-level inline comment in its thread. Like
// GitHub, it refuses a reply to a reply: "Replies to replies are not
// supported" (docs.github.com/en/rest/pulls/comments).
func (w *Writes) createReply(rw http.ResponseWriter, request *http.Request) {
	var input struct {
		Body string `json:"body"`
	}
	if !decode(rw, request, &input) {
		return
	}
	r := w.repo
	r.mu.Lock()
	defer r.mu.Unlock()
	found, ok := r.number(rw, request)
	if !ok {
		return
	}
	commentID, err := strconv.ParseInt(request.PathValue("comment"), 10, 64)
	index := slices.IndexFunc(r.reviewComment, func(comment *ReviewComment) bool {
		return comment.ID == commentID && comment.number == found.issue.Number
	})
	if found.pull == nil || err != nil || index < 0 {
		notFound(rw)
		return
	}
	target := r.reviewComment[index]
	switch {
	case strings.TrimSpace(input.Body) == "":
		invalid(rw, "Validation Failed: body is missing")
		return
	case target.InReplyToID != 0:
		invalid(rw, "Validation Failed: replies to replies are not supported")
		return
	}
	id := r.newID()
	reply := &ReviewComment{
		ID: id, Body: input.Body, User: r.account, Path: target.Path, Line: target.Line, OriginalLine: target.OriginalLine,
		CommitID: target.CommitID, InReplyToID: target.ID, CreatedAt: r.now, UpdatedAt: r.now,
		HTMLURL: fmt.Sprintf("%s#discussion_r%d", found.issue.HTMLURL, id), PullRequestURL: r.pullURL(found.issue.Number),
		number: found.issue.Number, visibleAt: r.now.Add(r.indexDelay),
	}
	r.reviewComment = append(r.reviewComment, reply)
	found.issue.UpdatedAt = r.now
	WriteJSON(rw, http.StatusCreated, reply)
}

// owner is the repository owner, the user part of a "user:ref" head.
func (r *Repo) owner() string {
	owner, _, _ := strings.Cut(r.fullName, "/")
	return owner
}

// headRef strips the "owner:" of a same-repository head. ok is false for a
// head in another owner's fork, which the fake does not model.
func (r *Repo) headRef(head string) (string, bool) {
	owner, ref, found := strings.Cut(head, ":")
	if !found {
		return head, true
	}
	return ref, owner == r.owner()
}

func validationFailed(rw http.ResponseWriter, field, code string) {
	WriteJSON(rw, http.StatusUnprocessableEntity, map[string]any{
		"message": "Validation Failed",
		"errors":  []map[string]string{{"resource": "PullRequest", "field": field, "code": code}},
	})
}

func (w *Writes) createPull(rw http.ResponseWriter, request *http.Request) {
	var input struct {
		Title string `json:"title"`
		Body  string `json:"body"`
		Head  string `json:"head"`
		Base  string `json:"base"`
		Draft bool   `json:"draft"`
	}
	if !decode(rw, request, &input) {
		return
	}
	r := w.repo
	r.mu.Lock()
	defer r.mu.Unlock()
	head, sameRepo := r.headRef(input.Head)
	headSHA, headFound := w.ref(head)
	baseSHA, baseFound := w.ref(input.Base)
	switch {
	case strings.TrimSpace(input.Title) == "":
		validationFailed(rw, "title", "missing_field")
		return
	case !sameRepo || !headFound:
		validationFailed(rw, "head", "invalid")
		return
	case !baseFound:
		validationFailed(rw, "base", "invalid")
		return
	}
	for _, found := range r.threads {
		if found.pull != nil && found.issue.State == StateOpen && found.pull.head.Ref == head && found.pull.base.Ref == input.Base {
			WriteJSON(rw, http.StatusUnprocessableEntity, map[string]any{
				"message": "Validation Failed",
				"errors":  []map[string]string{{"resource": "PullRequest", "code": "custom", "message": fmt.Sprintf("A pull request already exists for %s:%s.", r.owner(), head)}},
			})
			return
		}
	}
	pull := &pullState{
		head:  Branch{Ref: head, SHA: headSHA, Repo: &RepoRef{FullName: r.fullName}},
		base:  Branch{Ref: input.Base, SHA: baseSHA, Repo: &RepoRef{FullName: r.fullName}},
		draft: input.Draft,
	}
	number := r.openThread(r.account, input.Title, input.Body, pull)
	WriteJSON(rw, http.StatusCreated, r.pullView(r.threads[number]))
}

// listPulls serves GET pulls with GitHub's state, head ("owner:ref"), base,
// sort and direction filters, newest created first by default.
func (w *Writes) listPulls(rw http.ResponseWriter, request *http.Request) {
	r := w.repo
	r.mu.Lock()
	defer r.mu.Unlock()
	query := request.URL.Query()
	state := cmp.Or(query.Get("state"), StateOpen)
	head, headFilter := query.Get("head"), query.Has("head")
	headOwner, headRef, _ := strings.Cut(head, ":")
	base := query.Get("base")
	rows := []Pull{}
	for _, found := range r.threads {
		if found.pull == nil || state != "all" && found.issue.State != state || base != "" && found.pull.base.Ref != base {
			continue
		}
		if headFilter {
			headRepo := ""
			if found.pull.head.Repo != nil {
				headRepo = found.pull.head.Repo.FullName
			}
			owner, _, _ := strings.Cut(headRepo, "/")
			if owner != headOwner || found.pull.head.Ref != headRef {
				continue
			}
		}
		rows = append(rows, r.pullView(found))
	}
	ordered(request, rows, "created", "desc")
	if page, ok := window(rw, request, rows, DefaultPerPage, maxPerPage); ok {
		reply(rw, request, page)
	}
}

func (w *Writes) updatePull(rw http.ResponseWriter, request *http.Request) {
	var input struct {
		Title *string `json:"title"`
		Body  *string `json:"body"`
		State *string `json:"state"`
	}
	if !decode(rw, request, &input) {
		return
	}
	r := w.repo
	r.mu.Lock()
	defer r.mu.Unlock()
	found, ok := r.number(rw, request)
	if !ok {
		return
	}
	if found.pull == nil {
		notFound(rw)
		return
	}
	if input.Title != nil {
		found.issue.Title = *input.Title
	}
	if input.Body != nil {
		found.issue.Body = *input.Body
	}
	if input.State != nil {
		switch *input.State {
		case StateOpen:
			found.issue.State, found.issue.ClosedAt = StateOpen, nil
		case StateClosed:
			closedAt := r.now
			found.issue.State, found.issue.ClosedAt = StateClosed, &closedAt
		default:
			invalid(rw, "Validation Failed: state")
			return
		}
	}
	found.issue.UpdatedAt = r.now
	WriteJSON(rw, http.StatusOK, r.pullView(found))
}

type refObject struct {
	SHA  string `json:"sha"`
	Type string `json:"type"`
}

type refReply struct {
	Ref    string    `json:"ref"`
	Object refObject `json:"object"`
}

// getRef serves GET git/ref/heads/<branch>: an exact match or 404.
func (w *Writes) getRef(rw http.ResponseWriter, request *http.Request) {
	r := w.repo
	r.mu.Lock()
	defer r.mu.Unlock()
	branch, isHead := strings.CutPrefix(request.PathValue("ref"), "heads/")
	sha, found := w.ref(branch)
	if !isHead || !found {
		notFound(rw)
		return
	}
	reply(rw, request, refReply{Ref: "refs/heads/" + branch, Object: refObject{SHA: sha, Type: "commit"}})
}

// ref resolves a branch head from the bare repository, or from SetBranch
// without one. The caller holds r.mu.
func (w *Writes) ref(branch string) (string, bool) {
	if branch == "" {
		return "", false
	}
	if w.bare == "" {
		sha, ok := w.repo.branches[branch]
		return sha, ok
	}
	out, err := exec.Command("git", "--git-dir", w.bare, "rev-parse", "--verify", "--quiet", "refs/heads/"+branch+"^{commit}").Output()
	if err != nil {
		return "", false
	}
	return strings.TrimSpace(string(out)), true
}
