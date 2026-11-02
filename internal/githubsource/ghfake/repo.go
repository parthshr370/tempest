package ghfake

import (
	"cmp"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Account types GitHub reports in a user's "type" field.
const (
	TypeUser = "User"
	TypeBot  = "Bot"
)

// Issue, pull request, check and status states the fake serves.
const (
	StateOpen   = "open"
	StateClosed = "closed"

	CheckQueued     = "queued"
	CheckInProgress = "in_progress"
	CheckCompleted  = "completed"

	StatusPending = "pending"
	StatusSuccess = "success"
	StatusFailure = "failure"
	StatusError   = "error"

	ReviewApproved         = "APPROVED"
	ReviewChangesRequested = "CHANGES_REQUESTED"
	ReviewCommented        = "COMMENTED"
	ReviewDismissed        = "DISMISSED"
)

// Page sizes and limits GitHub applies to the routes the fake serves.
const (
	maxPerPage                = 100
	defaultNotificationsPage  = 50
	maxNotificationsPage      = 50
	defaultPollIntervalSecond = 60
	etagDigestBytes           = 8
)

// User is an account as GitHub serializes it.
type User struct {
	Login string `json:"login"`
	ID    int64  `json:"id"`
	Type  string `json:"type"`
}

// RepoRef names a repository inside another object.
type RepoRef struct {
	FullName string `json:"full_name"`
}

// PullLink marks an issue object that is a pull request.
type PullLink struct {
	URL      string     `json:"url"`
	HTMLURL  string     `json:"html_url"`
	MergedAt *time.Time `json:"merged_at"`
}

// Issue is an issue, or the issue view of a pull request.
type Issue struct {
	ID          int64      `json:"id"`
	Number      int        `json:"number"`
	Title       string     `json:"title"`
	Body        string     `json:"body"`
	User        User       `json:"user"`
	State       string     `json:"state"`
	Comments    int        `json:"comments"`
	CreatedAt   time.Time  `json:"created_at"`
	UpdatedAt   time.Time  `json:"updated_at"`
	ClosedAt    *time.Time `json:"closed_at"`
	URL         string     `json:"url"`
	HTMLURL     string     `json:"html_url"`
	PullRequest *PullLink  `json:"pull_request,omitempty"`
}

// Branch is a pull request's head or base.
type Branch struct {
	Ref  string   `json:"ref"`
	SHA  string   `json:"sha"`
	Repo *RepoRef `json:"repo"`
}

// Pull is the pulls/<n> view of a pull request.
type Pull struct {
	ID             int64      `json:"id"`
	Number         int        `json:"number"`
	Title          string     `json:"title"`
	Body           string     `json:"body"`
	User           User       `json:"user"`
	State          string     `json:"state"`
	Draft          bool       `json:"draft"`
	Merged         bool       `json:"merged"`
	MergeCommitSHA *string    `json:"merge_commit_sha"`
	CreatedAt      time.Time  `json:"created_at"`
	UpdatedAt      time.Time  `json:"updated_at"`
	ClosedAt       *time.Time `json:"closed_at"`
	MergedAt       *time.Time `json:"merged_at"`
	HTMLURL        string     `json:"html_url"`
	Head           Branch     `json:"head"`
	Base           Branch     `json:"base"`
}

// Comment is an issue or pull request conversation comment.
type Comment struct {
	ID        int64     `json:"id"`
	Body      string    `json:"body"`
	User      User      `json:"user"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
	HTMLURL   string    `json:"html_url"`
	IssueURL  string    `json:"issue_url"`
	number    int
	visibleAt time.Time
}

// ReviewComment is an inline pull request review comment.
type ReviewComment struct {
	ID             int64     `json:"id"`
	ReviewID       int64     `json:"pull_request_review_id,omitempty"`
	Body           string    `json:"body"`
	User           User      `json:"user"`
	Path           string    `json:"path"`
	Line           *int      `json:"line"`
	OriginalLine   *int      `json:"original_line"`
	CommitID       string    `json:"commit_id"`
	InReplyToID    int64     `json:"in_reply_to_id,omitempty"`
	CreatedAt      time.Time `json:"created_at"`
	UpdatedAt      time.Time `json:"updated_at"`
	HTMLURL        string    `json:"html_url"`
	PullRequestURL string    `json:"pull_request_url"`
	number         int
	visibleAt      time.Time
}

// Review is a submitted pull request review.
type Review struct {
	ID          int64      `json:"id"`
	User        User       `json:"user"`
	Body        string     `json:"body"`
	State       string     `json:"state"`
	CommitID    string     `json:"commit_id"`
	SubmittedAt *time.Time `json:"submitted_at"`
	HTMLURL     string     `json:"html_url"`
	number      int
	visibleAt   time.Time
}

// CheckRun is one check run on a commit.
type CheckRun struct {
	ID         int64   `json:"id"`
	Name       string  `json:"name"`
	HeadSHA    string  `json:"head_sha"`
	Status     string  `json:"status"`
	Conclusion *string `json:"conclusion"`
}

// CommitStatus is one commit status context.
type CommitStatus struct {
	ID        int64     `json:"id"`
	Context   string    `json:"context"`
	State     string    `json:"state"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
	sha       string
}

// NotificationSubject is what a notification is about.
type NotificationSubject struct {
	Title string `json:"title"`
	URL   string `json:"url"`
	Type  string `json:"type"`
}

// Notification is one thread in the account's inbox.
type Notification struct {
	ID         string              `json:"id"`
	Unread     bool                `json:"unread"`
	Reason     string              `json:"reason"`
	UpdatedAt  time.Time           `json:"updated_at"`
	Subject    NotificationSubject `json:"subject"`
	Repository RepoRef             `json:"repository"`
	visibleAt  time.Time
}

// PullSpec describes a pull request to open.
type PullSpec struct {
	Title   string
	Body    string
	HeadRef string
	HeadSHA string
	// HeadRepo defaults to the repository itself.
	HeadRepo string
	BaseRef  string
	Draft    bool
}

// ReviewCommentSpec describes an inline comment to add.
type ReviewCommentSpec struct {
	Body      string
	Path      string
	Line      int
	InReplyTo int64
}

// thread is an issue with, for a pull request, its branch state.
type thread struct {
	issue Issue
	pull  *pullState
}

type pullState struct {
	head     Branch
	base     Branch
	draft    bool
	mergeSHA string
	mergedAt *time.Time
}

// Repo is an in-memory GitHub repository served through a Server's read
// routes, with GitHub's since, sort, direction and page semantics. Tests
// change it through its methods and read time from it (it is a clock).
//
// IndexDelay models GitHub's indexing lag: a new comment, review, inline
// comment or notification stays out of every listing until IndexDelay has
// passed on the repo clock, while its timestamps say when it was made.
type Repo struct {
	mu            sync.Mutex
	fullName      string
	id            int64
	defaultBranch string
	account       User
	now           time.Time
	indexDelay    time.Duration
	pollInterval  int
	nextID        int64
	nextNumber    int
	threads       map[int]*thread
	comments      []*Comment
	reviewComment []*ReviewComment
	reviews       []*Review
	checkRuns     []*CheckRun
	statuses      []*CommitStatus
	branches      map[string]string
	notifications []*Notification
	rateLimit     int
	rateRemaining int
	rateReset     time.Time
}

// NewRepo serves fullName ("owner/repo") on server. Time starts at start,
// truncated to seconds like GitHub's timestamps. account answers GET /user.
func NewRepo(server *Server, fullName string, account User, start time.Time) *Repo {
	repo := &Repo{
		fullName:      fullName,
		id:            1,
		defaultBranch: "main",
		account:       account,
		now:           start.UTC().Truncate(time.Second),
		pollInterval:  defaultPollIntervalSecond,
		nextID:        1000,
		nextNumber:    1,
		threads:       map[int]*thread{},
		branches:      map[string]string{},
	}
	repo.routes(server)
	return repo
}

// Now is the repo clock.
func (r *Repo) Now() time.Time {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.now
}

// Advance moves the repo clock forward.
func (r *Repo) Advance(d time.Duration) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.now = r.now.Add(d).Truncate(time.Second)
}

// SetIndexDelay sets the indexing lag of items created from now on.
func (r *Repo) SetIndexDelay(d time.Duration) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.indexDelay = d
}

// SetPollInterval sets the X-Poll-Interval seconds of notification replies.
func (r *Repo) SetPollInterval(seconds int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.pollInterval = seconds
}

// SetRateLimit makes every routed reply report limit and the remaining
// budget, which each non-304 reply then decrements. A limit of 0 removes
// the rate headers.
func (r *Repo) SetRateLimit(limit, remaining int, reset time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.rateLimit, r.rateRemaining, r.rateReset = limit, remaining, reset
}

// SetBranch points a branch at sha.
func (r *Repo) SetBranch(name, sha string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.branches[name] = sha
}

func (r *Repo) newID() int64 {
	r.nextID++
	return r.nextID
}

func (r *Repo) issueURL(number int) string {
	return fmt.Sprintf("https://api.github.com/repos/%s/issues/%d", r.fullName, number)
}

func (r *Repo) pullURL(number int) string {
	return fmt.Sprintf("https://api.github.com/repos/%s/pulls/%d", r.fullName, number)
}

func (r *Repo) htmlURL(number int, isPull bool) string {
	kind := "issues"
	if isPull {
		kind = "pull"
	}
	return fmt.Sprintf("https://github.com/%s/%s/%d", r.fullName, kind, number)
}

// OpenIssue opens an issue and returns its number.
func (r *Repo) OpenIssue(author User, title, body string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.openThread(author, title, body, nil)
}

// OpenPull opens a pull request and returns its number.
func (r *Repo) OpenPull(author User, spec PullSpec) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	headRepo := cmp.Or(spec.HeadRepo, r.fullName)
	pull := &pullState{
		head:  Branch{Ref: spec.HeadRef, SHA: spec.HeadSHA, Repo: &RepoRef{FullName: headRepo}},
		base:  Branch{Ref: cmp.Or(spec.BaseRef, r.defaultBranch), SHA: r.branches[cmp.Or(spec.BaseRef, r.defaultBranch)], Repo: &RepoRef{FullName: r.fullName}},
		draft: spec.Draft,
	}
	return r.openThread(author, spec.Title, spec.Body, pull)
}

func (r *Repo) openThread(author User, title, body string, pull *pullState) int {
	number := r.nextNumber
	r.nextNumber++
	issue := Issue{
		ID: r.newID(), Number: number, Title: title, Body: body, User: author, State: StateOpen,
		CreatedAt: r.now, UpdatedAt: r.now, URL: r.issueURL(number), HTMLURL: r.htmlURL(number, pull != nil),
	}
	if pull != nil {
		issue.PullRequest = &PullLink{URL: r.pullURL(number), HTMLURL: issue.HTMLURL}
	}
	r.threads[number] = &thread{issue: issue, pull: pull}
	return number
}

// mustThread returns a thread or panics: tests name threads they created.
func (r *Repo) mustThread(number int) *thread {
	found, ok := r.threads[number]
	if !ok {
		panic(fmt.Sprintf("ghfake: no issue #%d", number))
	}
	return found
}

func (r *Repo) mustPull(number int) *thread {
	found := r.mustThread(number)
	if found.pull == nil {
		panic(fmt.Sprintf("ghfake: #%d is not a pull request", number))
	}
	return found
}

// EditIssue replaces an issue or pull request body.
func (r *Repo) EditIssue(number int, body string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	found := r.mustThread(number)
	found.issue.Body = body
	found.issue.UpdatedAt = r.now
}

// Close closes an issue or pull request without merging.
func (r *Repo) Close(number int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	found := r.mustThread(number)
	closedAt := r.now
	found.issue.State, found.issue.ClosedAt, found.issue.UpdatedAt = StateClosed, &closedAt, r.now
}

// Reopen reopens a closed issue or pull request.
func (r *Repo) Reopen(number int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	found := r.mustThread(number)
	found.issue.State, found.issue.ClosedAt, found.issue.UpdatedAt = StateOpen, nil, r.now
}

// Merge merges a pull request with mergeSHA.
func (r *Repo) Merge(number int, mergeSHA string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	found := r.mustPull(number)
	at := r.now
	found.pull.mergeSHA, found.pull.mergedAt = mergeSHA, &at
	found.issue.State, found.issue.ClosedAt, found.issue.UpdatedAt = StateClosed, &at, r.now
	found.issue.PullRequest.MergedAt = &at
}

// Push moves a pull request head to sha.
func (r *Repo) Push(number int, sha string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	found := r.mustPull(number)
	found.pull.head.SHA = sha
	found.issue.UpdatedAt = r.now
}

// Comment adds a conversation comment and returns its ID.
func (r *Repo) Comment(number int, author User, body string) int64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	found := r.mustThread(number)
	id := r.newID()
	r.comments = append(r.comments, &Comment{
		ID: id, Body: body, User: author, CreatedAt: r.now, UpdatedAt: r.now,
		HTMLURL: fmt.Sprintf("%s#issuecomment-%d", found.issue.HTMLURL, id), IssueURL: r.issueURL(number),
		number: number, visibleAt: r.now.Add(r.indexDelay),
	})
	found.issue.UpdatedAt = r.now
	return id
}

// EditComment replaces a conversation comment body.
func (r *Repo) EditComment(id int64, body string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, comment := range r.comments {
		if comment.ID == id {
			comment.Body, comment.UpdatedAt = body, r.now
			return
		}
	}
	panic(fmt.Sprintf("ghfake: no comment %d", id))
}

// DeleteComment removes a conversation comment.
func (r *Repo) DeleteComment(id int64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.comments = slices.DeleteFunc(r.comments, func(comment *Comment) bool { return comment.ID == id })
}

// Comments returns the conversation comments of an issue in ID order,
// including ones still hidden by the index delay.
func (r *Repo) Comments(number int) []Comment {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []Comment
	for _, comment := range r.comments {
		if comment.number == number {
			out = append(out, *comment)
		}
	}
	return out
}

// ReviewComment adds an inline comment on the pull request head.
func (r *Repo) ReviewComment(number int, author User, spec ReviewCommentSpec) int64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	found := r.mustPull(number)
	id := r.newID()
	line := spec.Line
	r.reviewComment = append(r.reviewComment, &ReviewComment{
		ID: id, Body: spec.Body, User: author, Path: spec.Path, Line: &line, OriginalLine: &line,
		CommitID: found.pull.head.SHA, InReplyToID: spec.InReplyTo, CreatedAt: r.now, UpdatedAt: r.now,
		HTMLURL: fmt.Sprintf("%s#discussion_r%d", found.issue.HTMLURL, id), PullRequestURL: r.pullURL(number),
		number: number, visibleAt: r.now.Add(r.indexDelay),
	})
	found.issue.UpdatedAt = r.now
	return id
}

// EditReviewComment replaces an inline comment body.
func (r *Repo) EditReviewComment(id int64, body string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, comment := range r.reviewComment {
		if comment.ID == id {
			comment.Body, comment.UpdatedAt = body, r.now
			return
		}
	}
	panic(fmt.Sprintf("ghfake: no review comment %d", id))
}

// DeleteReviewComment removes an inline comment.
func (r *Repo) DeleteReviewComment(id int64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.reviewComment = slices.DeleteFunc(r.reviewComment, func(comment *ReviewComment) bool { return comment.ID == id })
}

// Review submits a review of the pull request head and returns its ID.
func (r *Repo) Review(number int, author User, state, body string) int64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	found := r.mustPull(number)
	id := r.newID()
	at := r.now
	r.reviews = append(r.reviews, &Review{
		ID: id, User: author, Body: body, State: state, CommitID: found.pull.head.SHA, SubmittedAt: &at,
		HTMLURL: fmt.Sprintf("%s#pullrequestreview-%d", found.issue.HTMLURL, id), number: number, visibleAt: r.now.Add(r.indexDelay),
	})
	found.issue.UpdatedAt = r.now
	return id
}

// DismissReview dismisses a submitted review.
func (r *Repo) DismissReview(id int64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, review := range r.reviews {
		if review.ID == id {
			review.State = ReviewDismissed
			return
		}
	}
	panic(fmt.Sprintf("ghfake: no review %d", id))
}

// SetCheckRun records a check run on sha. conclusion is ignored unless
// status is completed. A later run with the same name supersedes earlier
// ones in the default (latest) listing.
func (r *Repo) SetCheckRun(sha, name, status, conclusion string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	run := &CheckRun{ID: r.newID(), Name: name, HeadSHA: sha, Status: status}
	if status == CheckCompleted {
		run.Conclusion = &conclusion
	}
	r.checkRuns = append(r.checkRuns, run)
}

// SetStatus records a commit status on sha.
func (r *Repo) SetStatus(sha, context, state string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.statuses = append(r.statuses, &CommitStatus{ID: r.newID(), Context: context, State: state, CreatedAt: r.now, UpdatedAt: r.now, sha: sha})
}

// Notify bumps the account's notification for an issue or pull request
// of this repository, marking it unread.
func (r *Repo) Notify(number int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	found := r.mustThread(number)
	subjectType, subjectURL := "Issue", r.issueURL(number)
	if found.pull != nil {
		subjectType, subjectURL = "PullRequest", r.pullURL(number)
	}
	r.notify(RepoRef{FullName: r.fullName}, NotificationSubject{Title: found.issue.Title, URL: subjectURL, Type: subjectType})
}

// NotifyElsewhere bumps a notification about another repository.
func (r *Repo) NotifyElsewhere(fullName string, number int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	url := fmt.Sprintf("https://api.github.com/repos/%s/issues/%d", fullName, number)
	r.notify(RepoRef{FullName: fullName}, NotificationSubject{Title: "elsewhere", URL: url, Type: "Issue"})
}

func (r *Repo) notify(repo RepoRef, subject NotificationSubject) {
	for _, existing := range r.notifications {
		if existing.Repository == repo && existing.Subject.URL == subject.URL {
			existing.UpdatedAt, existing.Unread, existing.visibleAt = r.now, true, r.now.Add(r.indexDelay)
			return
		}
	}
	r.notifications = append(r.notifications, &Notification{
		ID: strconv.FormatInt(r.newID(), 10), Unread: true, Reason: "mention", UpdatedAt: r.now,
		Subject: subject, Repository: repo, visibleAt: r.now.Add(r.indexDelay),
	})
}

// routes registers the read routes. Writes live in writes.go.
func (r *Repo) routes(server *Server) {
	prefix := "GET /repos/" + r.fullName
	handlers := map[string]http.HandlerFunc{
		"GET /user":                          r.getUser,
		"GET /notifications":                 r.listNotifications,
		prefix:                               r.getRepo,
		prefix + "/issues":                   r.listIssues,
		prefix + "/issues/comments":          r.listAllComments,
		prefix + "/issues/{number}":          r.getIssue,
		prefix + "/issues/{number}/comments": r.listIssueComments,
		prefix + "/pulls/comments":           r.listAllReviewComments,
		prefix + "/pulls/{number}":           r.getPull,
		prefix + "/pulls/{number}/reviews":   r.listReviews,
		prefix + "/pulls/{number}/comments":  r.listPullReviewComments,
		prefix + "/commits/{ref}":            r.getCommit,
		prefix + "/commits/{ref}/check-runs": r.listCheckRuns,
		prefix + "/commits/{ref}/status":     r.getCombinedStatus,
		prefix + "/branches/{branch}":        r.getBranch,
	}
	for pattern, handler := range handlers {
		server.HandleFunc(pattern, r.rated(handler))
	}
}

// rated adds the Date header from the repository clock, which is GitHub's
// clock (the source takes its cursors from it), adds rate-limit headers
// and charges the budget for replies that are not 304, which GitHub does
// not count.
func (r *Repo) rated(handler http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, request *http.Request) {
		r.mu.Lock()
		w.Header().Set("Date", r.now.UTC().Format(http.TimeFormat))
		if r.rateLimit > 0 {
			w.Header().Set("X-RateLimit-Limit", strconv.Itoa(r.rateLimit))
			w.Header().Set("X-RateLimit-Remaining", strconv.Itoa(max(r.rateRemaining-1, 0)))
			w.Header().Set("X-RateLimit-Reset", strconv.FormatInt(r.rateReset.Unix(), 10))
		}
		r.mu.Unlock()
		recorder := &statusRecorder{ResponseWriter: w}
		handler(recorder, request)
		r.mu.Lock()
		if r.rateLimit > 0 && recorder.status != http.StatusNotModified {
			r.rateRemaining = max(r.rateRemaining-1, 0)
		}
		r.mu.Unlock()
	}
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (s *statusRecorder) WriteHeader(status int) {
	s.status = status
	s.ResponseWriter.WriteHeader(status)
}

// reply writes value with a content ETag and answers a matching
// If-None-Match with 304.
func reply(w http.ResponseWriter, request *http.Request, value any) {
	raw, err := json.Marshal(value)
	if err != nil {
		WriteJSON(w, http.StatusInternalServerError, map[string]string{"message": err.Error()})
		return
	}
	sum := sha256.Sum256(raw)
	etag := `"` + hex.EncodeToString(sum[:etagDigestBytes]) + `"`
	w.Header().Set("ETag", etag)
	if request.Header.Get("If-None-Match") == etag {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	if _, err := w.Write(raw); err != nil {
		fmt.Fprintf(os.Stderr, "ghfake: write reply: %v\n", err)
	}
}

func notFound(w http.ResponseWriter) {
	WriteJSON(w, http.StatusNotFound, map[string]string{"message": "Not Found"})
}

func invalid(w http.ResponseWriter, message string) {
	WriteJSON(w, http.StatusUnprocessableEntity, map[string]string{"message": message})
}

// window selects the requested page of rows, at most limit per page, and
// sets the Link header. ok is false after an invalid page parameter was
// answered.
func window[T any](w http.ResponseWriter, request *http.Request, rows []T, defaultPerPage, limit int) ([]T, bool) {
	query := request.URL.Query()
	perPage, page := defaultPerPage, 1
	for _, param := range []struct {
		name   string
		target *int
	}{{"per_page", &perPage}, {"page", &page}} {
		text := query.Get(param.name)
		if text == "" {
			continue
		}
		parsed, err := strconv.Atoi(text)
		if err != nil || parsed < 1 {
			invalid(w, "invalid "+param.name)
			return nil, false
		}
		*param.target = parsed
	}
	perPage = min(perPage, limit)
	start := min((page-1)*perPage, len(rows))
	end := min(start+perPage, len(rows))
	if end < len(rows) {
		next := *request.URL
		nextQuery := next.Query()
		nextQuery.Set("page", strconv.Itoa(page+1))
		nextQuery.Set("per_page", strconv.Itoa(perPage))
		next.RawQuery = nextQuery.Encode()
		w.Header().Set("Link", fmt.Sprintf("<https://api.github.com%s>; rel=\"next\"", next.RequestURI()))
	}
	return rows[start:end], true
}

// since parses the since parameter; zero when absent.
func since(w http.ResponseWriter, request *http.Request) (time.Time, bool) {
	return timeParam(w, request, "since")
}

// timeParam parses an RFC 3339 query parameter; zero when absent.
func timeParam(w http.ResponseWriter, request *http.Request, name string) (time.Time, bool) {
	text := request.URL.Query().Get(name)
	if text == "" {
		return time.Time{}, true
	}
	parsed, err := time.Parse(time.RFC3339, text)
	if err != nil {
		invalid(w, "invalid "+name)
		return time.Time{}, false
	}
	return parsed, true
}

// rowOrder is what the sort parameter can order a listed row by.
type rowOrder struct {
	id      int64
	created time.Time
	updated time.Time
}

type sortable interface{ order() rowOrder }

func (row Issue) order() rowOrder         { return rowOrder{row.ID, row.CreatedAt, row.UpdatedAt} }
func (row Comment) order() rowOrder       { return rowOrder{row.ID, row.CreatedAt, row.UpdatedAt} }
func (row ReviewComment) order() rowOrder { return rowOrder{row.ID, row.CreatedAt, row.UpdatedAt} }

// ordered sorts rows by the sort and direction parameters, falling back to
// the route's defaults. "created" and "updated" break ties by ID; any
// other sort lists by ID ascending, as GitHub does without a sort.
func ordered[T sortable](request *http.Request, rows []T, defaultSort, defaultDirection string) {
	query := request.URL.Query()
	field := cmp.Or(query.Get("sort"), defaultSort)
	descending := cmp.Or(query.Get("direction"), defaultDirection) == "desc"
	slices.SortStableFunc(rows, func(a, b T) int {
		left, right := a.order(), b.order()
		order := 0
		switch field {
		case "created":
			order = left.created.Compare(right.created)
		case "updated":
			order = left.updated.Compare(right.updated)
		default:
			return cmp.Compare(left.id, right.id)
		}
		if order == 0 {
			order = cmp.Compare(left.id, right.id)
		}
		if descending {
			return -order
		}
		return order
	})
}

func (r *Repo) getUser(w http.ResponseWriter, request *http.Request) {
	r.mu.Lock()
	defer r.mu.Unlock()
	reply(w, request, r.account)
}

type repoReply struct {
	ID            int64           `json:"id"`
	FullName      string          `json:"full_name"`
	DefaultBranch string          `json:"default_branch"`
	Permissions   map[string]bool `json:"permissions"`
}

func (r *Repo) getRepo(w http.ResponseWriter, request *http.Request) {
	r.mu.Lock()
	defer r.mu.Unlock()
	reply(w, request, repoReply{ID: r.id, FullName: r.fullName, DefaultBranch: r.defaultBranch, Permissions: map[string]bool{"pull": true, "push": true}})
}

func (r *Repo) number(w http.ResponseWriter, request *http.Request) (*thread, bool) {
	number, err := strconv.Atoi(request.PathValue("number"))
	if err != nil {
		notFound(w)
		return nil, false
	}
	found, ok := r.threads[number]
	if !ok {
		notFound(w)
		return nil, false
	}
	return found, true
}

// issueView renders an issue with its visible comment count.
func (r *Repo) issueView(found *thread) Issue {
	view := found.issue
	for _, comment := range r.comments {
		if comment.number == view.Number && !comment.visibleAt.After(r.now) {
			view.Comments++
		}
	}
	return view
}

func (r *Repo) getIssue(w http.ResponseWriter, request *http.Request) {
	r.mu.Lock()
	defer r.mu.Unlock()
	found, ok := r.number(w, request)
	if !ok {
		return
	}
	reply(w, request, r.issueView(found))
}

func (r *Repo) listIssues(w http.ResponseWriter, request *http.Request) {
	r.mu.Lock()
	defer r.mu.Unlock()
	after, ok := since(w, request)
	if !ok {
		return
	}
	state := cmp.Or(request.URL.Query().Get("state"), StateOpen)
	var rows []Issue
	for _, found := range r.threads {
		if (state == "all" || found.issue.State == state) && !found.issue.UpdatedAt.Before(after) {
			rows = append(rows, r.issueView(found))
		}
	}
	ordered(request, rows, "created", "desc")
	if page, ok := window(w, request, rows, DefaultPerPage, maxPerPage); ok {
		reply(w, request, page)
	}
}

func (r *Repo) visibleComments(number int, after time.Time) []Comment {
	var rows []Comment
	for _, comment := range r.comments {
		if (number == 0 || comment.number == number) && !comment.visibleAt.After(r.now) && !comment.UpdatedAt.Before(after) {
			rows = append(rows, *comment)
		}
	}
	return rows
}

func (r *Repo) listIssueComments(w http.ResponseWriter, request *http.Request) {
	r.mu.Lock()
	defer r.mu.Unlock()
	found, ok := r.number(w, request)
	if !ok {
		return
	}
	after, ok := since(w, request)
	if !ok {
		return
	}
	// The per-issue listing has no sort parameter: always ID ascending.
	rows := r.visibleComments(found.issue.Number, after)
	slices.SortFunc(rows, func(a, b Comment) int { return cmp.Compare(a.ID, b.ID) })
	if page, ok := window(w, request, rows, DefaultPerPage, maxPerPage); ok {
		reply(w, request, page)
	}
}

func (r *Repo) listAllComments(w http.ResponseWriter, request *http.Request) {
	r.mu.Lock()
	defer r.mu.Unlock()
	after, ok := since(w, request)
	if !ok {
		return
	}
	rows := r.visibleComments(0, after)
	ordered(request, rows, "", "asc")
	if page, ok := window(w, request, rows, DefaultPerPage, maxPerPage); ok {
		reply(w, request, page)
	}
}

func (r *Repo) pullView(found *thread) Pull {
	issue := found.issue
	view := Pull{
		ID: issue.ID, Number: issue.Number, Title: issue.Title, Body: issue.Body, User: issue.User, State: issue.State,
		Draft: found.pull.draft, Merged: found.pull.mergedAt != nil, CreatedAt: issue.CreatedAt, UpdatedAt: issue.UpdatedAt,
		ClosedAt: issue.ClosedAt, MergedAt: found.pull.mergedAt, HTMLURL: issue.HTMLURL, Head: found.pull.head, Base: found.pull.base,
	}
	if found.pull.mergeSHA != "" {
		mergeSHA := found.pull.mergeSHA
		view.MergeCommitSHA = &mergeSHA
	}
	return view
}

func (r *Repo) getPull(w http.ResponseWriter, request *http.Request) {
	r.mu.Lock()
	defer r.mu.Unlock()
	found, ok := r.number(w, request)
	if !ok {
		return
	}
	if found.pull == nil {
		notFound(w)
		return
	}
	reply(w, request, r.pullView(found))
}

func (r *Repo) listReviews(w http.ResponseWriter, request *http.Request) {
	r.mu.Lock()
	defer r.mu.Unlock()
	found, ok := r.number(w, request)
	if !ok {
		return
	}
	var rows []Review
	for _, review := range r.reviews {
		if review.number == found.issue.Number && !review.visibleAt.After(r.now) {
			rows = append(rows, *review)
		}
	}
	if page, ok := window(w, request, rows, DefaultPerPage, maxPerPage); ok {
		reply(w, request, page)
	}
}

func (r *Repo) visibleReviewComments(number int, after time.Time) []ReviewComment {
	var rows []ReviewComment
	for _, comment := range r.reviewComment {
		if (number == 0 || comment.number == number) && !comment.visibleAt.After(r.now) && !comment.UpdatedAt.Before(after) {
			rows = append(rows, *comment)
		}
	}
	return rows
}

func (r *Repo) listReviewComments(w http.ResponseWriter, request *http.Request, number int) {
	after, ok := since(w, request)
	if !ok {
		return
	}
	rows := r.visibleReviewComments(number, after)
	ordered(request, rows, "", "asc")
	if page, ok := window(w, request, rows, DefaultPerPage, maxPerPage); ok {
		reply(w, request, page)
	}
}

func (r *Repo) listPullReviewComments(w http.ResponseWriter, request *http.Request) {
	r.mu.Lock()
	defer r.mu.Unlock()
	found, ok := r.number(w, request)
	if !ok {
		return
	}
	r.listReviewComments(w, request, found.issue.Number)
}

func (r *Repo) listAllReviewComments(w http.ResponseWriter, request *http.Request) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.listReviewComments(w, request, 0)
}

// resolve turns a branch name or SHA into a SHA.
func (r *Repo) resolve(ref string) string {
	if sha, ok := r.branches[ref]; ok {
		return sha
	}
	return ref
}

type commitReply struct {
	SHA string `json:"sha"`
}

func (r *Repo) getCommit(w http.ResponseWriter, request *http.Request) {
	r.mu.Lock()
	defer r.mu.Unlock()
	ref := request.PathValue("ref")
	if _, branch := r.branches[ref]; !branch && !isSHA(ref) {
		notFound(w)
		return
	}
	reply(w, request, commitReply{SHA: r.resolve(ref)})
}

// minSHALength is the shortest abbreviated commit SHA GitHub resolves.
const minSHALength = 7

func isSHA(ref string) bool {
	if len(ref) < minSHALength {
		return false
	}
	_, err := hex.DecodeString(ref[:len(ref)/2*2])
	return err == nil
}

type branchReply struct {
	Name   string      `json:"name"`
	Commit commitReply `json:"commit"`
}

func (r *Repo) getBranch(w http.ResponseWriter, request *http.Request) {
	r.mu.Lock()
	defer r.mu.Unlock()
	name := request.PathValue("branch")
	sha, ok := r.branches[name]
	if !ok {
		notFound(w)
		return
	}
	reply(w, request, branchReply{Name: name, Commit: commitReply{SHA: sha}})
}

type checkRunsReply struct {
	TotalCount int        `json:"total_count"`
	CheckRuns  []CheckRun `json:"check_runs"`
}

func (r *Repo) listCheckRuns(w http.ResponseWriter, request *http.Request) {
	r.mu.Lock()
	defer r.mu.Unlock()
	sha := r.resolve(request.PathValue("ref"))
	latestOnly := request.URL.Query().Get("filter") != "all"
	var rows []CheckRun
	seen := map[string]bool{}
	for i := len(r.checkRuns) - 1; i >= 0; i-- {
		run := r.checkRuns[i]
		if run.HeadSHA != sha || (latestOnly && seen[run.Name]) {
			continue
		}
		seen[run.Name] = true
		rows = append(rows, *run)
	}
	slices.SortFunc(rows, func(a, b CheckRun) int { return cmp.Compare(a.ID, b.ID) })
	page, ok := window(w, request, rows, DefaultPerPage, maxPerPage)
	if ok {
		reply(w, request, checkRunsReply{TotalCount: len(rows), CheckRuns: page})
	}
}

type combinedStatusReply struct {
	State      string         `json:"state"`
	SHA        string         `json:"sha"`
	TotalCount int            `json:"total_count"`
	Statuses   []CommitStatus `json:"statuses"`
}

func (r *Repo) getCombinedStatus(w http.ResponseWriter, request *http.Request) {
	r.mu.Lock()
	defer r.mu.Unlock()
	sha := r.resolve(request.PathValue("ref"))
	latest := map[string]CommitStatus{}
	for _, status := range r.statuses {
		if status.sha == sha {
			latest[status.Context] = *status
		}
	}
	rows := make([]CommitStatus, 0, len(latest))
	for _, status := range latest {
		rows = append(rows, status)
	}
	slices.SortFunc(rows, func(a, b CommitStatus) int { return strings.Compare(a.Context, b.Context) })
	state := StatusSuccess
	if len(rows) == 0 {
		state = StatusPending
	}
	for _, status := range rows {
		switch status.State {
		case StatusFailure, StatusError:
			state = StatusFailure
		case StatusPending:
			if state != StatusFailure {
				state = StatusPending
			}
		}
	}
	page, ok := window(w, request, rows, maxPerPage, maxPerPage)
	if ok {
		reply(w, request, combinedStatusReply{State: state, SHA: sha, TotalCount: len(rows), Statuses: page})
	}
}

// listNotifications serves the inbox newest first, ties by descending
// numeric ID, with since (updated at or after), before (updated strictly
// before), Last-Modified, If-Modified-Since and X-Poll-Interval like
// GitHub, at most 50 rows a page. Last-Modified is when the listing last
// changed: a late-indexed notification changes it when it appears,
// although its updated_at is older.
func (r *Repo) listNotifications(w http.ResponseWriter, request *http.Request) {
	r.mu.Lock()
	defer r.mu.Unlock()
	after, ok := since(w, request)
	if !ok {
		return
	}
	before, ok := timeParam(w, request, "before")
	if !ok {
		return
	}
	all := request.URL.Query().Get("all") == "true"
	var rows []Notification
	var lastModified time.Time
	for _, notification := range r.notifications {
		if notification.visibleAt.After(r.now) {
			continue
		}
		changedAt := notification.UpdatedAt
		if notification.visibleAt.After(changedAt) {
			changedAt = notification.visibleAt
		}
		if changedAt.After(lastModified) {
			lastModified = changedAt
		}
		if (all || notification.Unread) && !notification.UpdatedAt.Before(after) && (before.IsZero() || notification.UpdatedAt.Before(before)) {
			rows = append(rows, *notification)
		}
	}
	w.Header().Set("X-Poll-Interval", strconv.Itoa(r.pollInterval))
	if !lastModified.IsZero() {
		w.Header().Set("Last-Modified", lastModified.UTC().Format(http.TimeFormat))
	}
	if text := request.Header.Get("If-Modified-Since"); text != "" {
		if at, err := http.ParseTime(text); err == nil && !lastModified.After(at) {
			w.WriteHeader(http.StatusNotModified)
			return
		}
	}
	slices.SortStableFunc(rows, func(a, b Notification) int {
		if order := b.UpdatedAt.Compare(a.UpdatedAt); order != 0 {
			return order
		}
		// IDs are decimal strings without leading zeros: a longer one is
		// larger.
		return cmp.Or(cmp.Compare(len(b.ID), len(a.ID)), strings.Compare(b.ID, a.ID))
	})
	if page, ok := window(w, request, rows, defaultNotificationsPage, maxNotificationsPage); ok {
		reply(w, request, page)
	}
}
