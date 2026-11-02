package source

import (
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"go.harness.dev/harness/internal/githubsource/model"
)

// GitHub reply shapes. Only the fields the source reads are declared.

type ghUser struct {
	Login string `json:"login"`
	ID    int64  `json:"id"`
	Type  string `json:"type"`
}

type ghIssue struct {
	Number      int        `json:"number"`
	Title       string     `json:"title"`
	Body        *string    `json:"body"`
	User        ghUser     `json:"user"`
	State       string     `json:"state"`
	CreatedAt   time.Time  `json:"created_at"`
	UpdatedAt   time.Time  `json:"updated_at"`
	HTMLURL     string     `json:"html_url"`
	PullRequest *struct{}  `json:"pull_request"`
	ClosedAt    *time.Time `json:"closed_at"`
}

type ghBranch struct {
	Ref  string `json:"ref"`
	SHA  string `json:"sha"`
	Repo *struct {
		FullName string `json:"full_name"`
	} `json:"repo"`
}

type ghPull struct {
	Number         int        `json:"number"`
	User           ghUser     `json:"user"`
	State          string     `json:"state"`
	Draft          bool       `json:"draft"`
	Merged         bool       `json:"merged"`
	MergeCommitSHA *string    `json:"merge_commit_sha"`
	UpdatedAt      time.Time  `json:"updated_at"`
	ClosedAt       *time.Time `json:"closed_at"`
	MergedAt       *time.Time `json:"merged_at"`
	HTMLURL        string     `json:"html_url"`
	Head           ghBranch   `json:"head"`
	Base           ghBranch   `json:"base"`
}

// ghComment is a conversation comment or an inline review comment.
type ghComment struct {
	ID             int64     `json:"id"`
	Body           string    `json:"body"`
	User           ghUser    `json:"user"`
	CreatedAt      time.Time `json:"created_at"`
	UpdatedAt      time.Time `json:"updated_at"`
	HTMLURL        string    `json:"html_url"`
	IssueURL       string    `json:"issue_url"`
	PullRequestURL string    `json:"pull_request_url"`
	Path           string    `json:"path"`
	Line           *int      `json:"line"`
	OriginalLine   *int      `json:"original_line"`
	CommitID       string    `json:"commit_id"`
	InReplyToID    int64     `json:"in_reply_to_id"`
}

type ghReview struct {
	ID          int64      `json:"id"`
	User        ghUser     `json:"user"`
	Body        string     `json:"body"`
	State       string     `json:"state"`
	CommitID    string     `json:"commit_id"`
	SubmittedAt *time.Time `json:"submitted_at"`
	HTMLURL     string     `json:"html_url"`
}

type ghNotification struct {
	UpdatedAt time.Time `json:"updated_at"`
	Subject   struct {
		Title string `json:"title"`
		URL   string `json:"url"`
		Type  string `json:"type"`
	} `json:"subject"`
	Repository struct {
		FullName string `json:"full_name"`
	} `json:"repository"`
}

type ghCheckRuns struct {
	CheckRuns []struct {
		Name       string  `json:"name"`
		Status     string  `json:"status"`
		Conclusion *string `json:"conclusion"`
	} `json:"check_runs"`
}

type ghCombinedStatus struct {
	Statuses []struct {
		Context string `json:"context"`
		State   string `json:"state"`
	} `json:"statuses"`
}

type ghBranchHead struct {
	Commit struct {
		SHA string `json:"sha"`
	} `json:"commit"`
}

// Review states GitHub reports.
const (
	reviewPending   = "PENDING"
	reviewDismissed = "DISMISSED"
	stateClosed     = "closed"
	stateOpen       = "open"
)

// Check outcomes: conclusions and commit-status states that fail CI, and
// those that count as skipped rather than passing.
var (
	failingOutcomes = []string{"failure", "timed_out", "cancelled", "action_required", "startup_failure", "error"}
	skippedOutcomes = []string{"skipped", "stale"}
)

// text is the account-relative reading of a body: mention and command
// detection run on the full text, storage keeps a bounded prefix.
type text struct {
	login   string
	mention *regexp.Regexp
	maxBody int
}

func newText(login string, maxBody int) text {
	pattern := `(?i)(^|[^a-z0-9_@])@` + regexp.QuoteMeta(login) + `([^a-z0-9_-]|$)`
	return text{login: login, mention: regexp.MustCompile(pattern), maxBody: maxBody}
}

// Mentions reports whether body addresses the account with an @login
// token not embedded in a longer name or address.
func (t text) Mentions(body string) bool { return t.mention.MatchString(body) }

// Command parses "@login stop" or "@login status" on the first non-empty
// line, case-insensitive and trimmed. Anything else is no command.
func (t text) Command(body string) model.Command {
	for line := range strings.SplitSeq(body, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		if len(fields) != 2 || !strings.EqualFold(fields[0], "@"+t.login) {
			return model.CommandNone
		}
		switch strings.ToLower(fields[1]) {
		case string(model.CommandStop):
			return model.CommandStop
		case string(model.CommandStatus):
			return model.CommandStatus
		}
		return model.CommandNone
	}
	return model.CommandNone
}

// withBody stores body on event, bounded, and marks mention and command.
func (t text) withBody(event model.Event, body string, commands bool) model.Event {
	event.Mention = t.Mentions(body)
	if commands {
		event.Command = t.Command(body)
	}
	event.BodyBytes = len(body)
	if len(body) > t.maxBody {
		cut := t.maxBody
		for cut > 0 && !utf8.RuneStart(body[cut]) {
			cut--
		}
		body = body[:cut]
		event.Truncated = true
	}
	event.Body = body
	return event
}

func actor(user ghUser) model.Actor {
	return model.Actor{Login: user.Login, ID: user.ID, Type: user.Type}
}

// isAccount reports the dedicated account's own activity, which the source
// never stores (admission rule 1).
func (s *Source) isAccount(user ghUser) bool {
	if s.account.ID != 0 && user.ID == s.account.ID {
		return true
	}
	return strings.EqualFold(user.Login, s.account.Login)
}

// openedEvent normalizes an Issue or PR title and body.
func (s *Source) openedEvent(issue ghIssue, observedAt time.Time) (model.Event, bool) {
	if s.isAccount(issue.User) {
		return model.Event{}, false
	}
	ref := model.ThreadRef{Number: issue.Number, IsPR: issue.PullRequest != nil}
	kind := model.EventIssueOpened
	if ref.IsPR {
		kind = model.EventPROpened
	}
	event := model.Event{
		ID: model.IssueOpenedID(issue.Number), Kind: kind, Thread: ref, Actor: actor(issue.User),
		At: issue.CreatedAt, ObservedAt: observedAt, URL: issue.HTMLURL, Title: issue.Title,
	}
	body := ""
	if issue.Body != nil {
		body = *issue.Body
	}
	return s.text.withBody(event, body, false), true
}

// edited reports whether a comment has a version after its creation.
func edited(created, updated time.Time) bool {
	return updated.Truncate(time.Second).After(created.Truncate(time.Second))
}

// commentEvents normalizes one conversation or inline comment. A comment
// first seen already edited also yields its creation version with the plain
// kind, so a mention fixed by a quick edit before the poll still counts as
// written; known IDs make the extra event free on rereads.
func (s *Source) commentEvents(row ghComment, ref model.ThreadRef, inline bool, observedAt time.Time) []model.Event {
	if s.isAccount(row.User) {
		return nil
	}
	kind, editedKind, id := model.EventComment, model.EventCommentEdited, model.CommentID
	if inline {
		kind, editedKind, id = model.EventReviewComment, model.EventReviewCommentEdited, model.ReviewCommentID
	}
	base := model.Event{Thread: ref, Actor: actor(row.User), ObservedAt: observedAt, URL: row.HTMLURL, CommentID: row.ID}
	if inline {
		base.InReplyTo, base.Path, base.CommitSHA = row.InReplyToID, row.Path, row.CommitID
		switch {
		case row.Line != nil:
			base.Line = *row.Line
		case row.OriginalLine != nil:
			base.Line = *row.OriginalLine
		}
	}
	base = s.text.withBody(base, row.Body, !inline)
	created := base
	created.ID, created.Kind, created.At = id(row.ID, row.CreatedAt), kind, row.CreatedAt
	if !edited(row.CreatedAt, row.UpdatedAt) {
		return []model.Event{created}
	}
	current := base
	current.ID, current.Kind, current.At = id(row.ID, row.UpdatedAt), editedKind, row.UpdatedAt
	return []model.Event{created, current}
}

// reviewEvent normalizes a submitted review, including a bodyless approval.
func (s *Source) reviewEvent(row ghReview, ref model.ThreadRef, dismissed bool, observedAt time.Time) model.Event {
	event := model.Event{
		ID: model.ReviewID(row.ID), Kind: model.EventReview, Thread: ref, Actor: actor(row.User),
		ObservedAt: observedAt, URL: row.HTMLURL, CommentID: row.ID, ReviewState: row.State, CommitSHA: row.CommitID,
	}
	if row.SubmittedAt != nil {
		event.At = *row.SubmittedAt
	}
	if dismissed {
		event.ID, event.Kind, event.ReviewState = model.ReviewDismissedID(row.ID), model.EventReviewDismissed, reviewDismissed
		event.At = observedAt
		return event
	}
	return s.text.withBody(event, row.Body, false)
}

// subjectNumber extracts the issue or PR number from an API URL of the
// configured repository; false for other repositories or subjects.
func (s *Source) subjectNumber(apiURL string) (int, bool) {
	marker := "/repos/" + strings.ToLower(s.repo) + "/"
	lower := strings.ToLower(apiURL)
	index := strings.Index(lower, marker)
	if index < 0 {
		return 0, false
	}
	rest := apiURL[index+len(marker):]
	kind, number, found := strings.Cut(rest, "/")
	if !found || (kind != "issues" && kind != "pulls") {
		return 0, false
	}
	parsed, err := strconv.Atoi(number)
	if err != nil || parsed <= 0 {
		return 0, false
	}
	return parsed, true
}

// ciOutcome is one check or status context of a commit.
type ciOutcome struct {
	name    string
	outcome string
	pending bool
}

// snapshot reduces outcomes to a settled CISnapshot. ok is false while any
// check is pending or when the commit reports no checks at all, so a fresh
// push is never presented as green before its checks exist.
func snapshot(sha string, outcomes []ciOutcome) (model.CISnapshot, bool) {
	if len(outcomes) == 0 {
		return model.CISnapshot{}, false
	}
	lines := make([]string, 0, len(outcomes))
	result := model.CISnapshot{SHA: sha}
	for _, check := range outcomes {
		if check.pending {
			return model.CISnapshot{}, false
		}
		lines = append(lines, check.name+"="+check.outcome)
		switch {
		case slices.Contains(failingOutcomes, check.outcome):
			result.Failing = append(result.Failing, check.name)
		case slices.Contains(skippedOutcomes, check.outcome):
			result.Skipped++
		default:
			result.Passing++
		}
	}
	slices.Sort(lines)
	slices.Sort(result.Failing)
	result.Fingerprint = model.CIFingerprint(lines)
	return result, true
}

func ciEvent(id model.EventID, kind model.EventKind, ref model.ThreadRef, ci model.CISnapshot, url string, observedAt time.Time) model.Event {
	return model.Event{ID: id, Kind: kind, Thread: ref, At: observedAt, ObservedAt: observedAt, URL: url, CommitSHA: ci.SHA, CI: &ci}
}

func threadResource(number int, part string) string {
	if part == "" {
		return fmt.Sprintf("thread:%d", number)
	}
	return fmt.Sprintf("thread:%d:%s", number, part)
}
