package githubsource_test

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"go.harness.dev/harness/internal/engine/stream"
	"go.harness.dev/harness/internal/engine/types"
	"go.harness.dev/harness/internal/githubsource"
	"go.harness.dev/harness/internal/githubsource/ghfake"
	"go.harness.dev/harness/internal/githubsource/model"
	"go.harness.dev/harness/internal/prompt"
	"go.harness.dev/harness/internal/provider/faux"
	"go.harness.dev/harness/internal/serve"
	"go.harness.dev/harness/tempest"
	"go.harness.dev/harness/tempest/extension"
)

const (
	repoName = "acme/widgets"
	// waitLimit bounds every eventual condition.
	waitLimit = 60 * time.Second
	// workerMarker opens the github-role section; the Ticket ID follows.
	workerMarker = "You are the worker for GitHub ticket "
)

var (
	epoch = time.Date(2026, 11, 3, 9, 0, 0, 0, time.UTC)
	bot   = ghfake.User{Login: "tempest-bot", ID: 100, Type: ghfake.TypeUser}
	parth = ghfake.User{Login: "parth", ID: 1111, Type: ghfake.TypeUser}
	alice = ghfake.User{Login: "alice", ID: 2222, Type: ghfake.TypeUser}
)

// reply is one scripted provider answer; gate, when set, holds it until
// closed or until the run is cancelled.
type reply struct {
	gate    chan struct{}
	content []types.ContentBlock
}

// workers routes each provider request to the script of the issue or PR
// number its Ticket was requested on, read from the worker's system prompt.
type workers struct {
	provider *faux.Faux
	mu       sync.Mutex
	scripts  map[int][]reply
	calls    map[int][]string
	active   map[int]int
}

func newWorkers() *workers {
	return &workers{
		provider: faux.New(faux.Options{Models: []faux.ModelDef{{ID: "worker-model", Cost: types.ModelCost{Input: 1, Output: 1}}}}),
		scripts:  make(map[int][]reply), calls: make(map[int][]string), active: make(map[int]int),
	}
}

func (w *workers) script(number int, replies ...reply) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.scripts[number] = append(w.scripts[number], replies...)
}

func (w *workers) requests(number int) []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]string(nil), w.calls[number]...)
}

func (w *workers) running(number int) int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.active[number]
}

func (w *workers) stream(ctx context.Context, worker types.Model, request types.Context, options *types.SimpleStreamOptions) *stream.AssistantStream {
	w.provider.AppendResponses(w.respond)
	return w.provider.StreamSimple(ctx, worker, request, options)
}

// threadOf reads the Ticket's thread number from its ID ("i42-..." or
// "p42-...").
func threadOf(system string) int {
	at := strings.Index(system, workerMarker)
	if at < 0 {
		return 0
	}
	id := strings.Fields(system[at+len(workerMarker):])[0]
	var number int
	if _, err := fmt.Sscanf(id[1:], "%d-", &number); err != nil {
		return 0
	}
	return number
}

func (w *workers) respond(request types.Context, options *types.StreamOptions, state faux.State, worker types.Model) (types.AssistantMessage, error) {
	number := threadOf(request.SystemPrompt)
	since := 0
	for i := len(request.Messages) - 1; i >= 0; i-- {
		if _, ok := request.Messages[i].(types.AssistantMessage); ok {
			since = i + 1
			break
		}
		if _, ok := request.Messages[i].(*types.AssistantMessage); ok {
			since = i + 1
			break
		}
	}
	transcript, err := json.Marshal(request.Messages[since:])
	if err != nil {
		return types.AssistantMessage{}, err
	}
	w.mu.Lock()
	w.calls[number] = append(w.calls[number], string(transcript))
	next := reply{content: []types.ContentBlock{faux.Text("done")}}
	if queued := w.scripts[number]; len(queued) > 0 {
		next, w.scripts[number] = queued[0], queued[1:]
	}
	w.active[number]++
	w.mu.Unlock()
	defer func() {
		w.mu.Lock()
		w.active[number]--
		w.mu.Unlock()
	}()
	if next.gate != nil {
		select {
		case <-next.gate:
		case <-state.Context.Done():
			next.content = []types.ContentBlock{faux.Text("cancelled")}
		}
	}
	return faux.Respond(next.content...)(request, options, state, worker)
}

var callSeq struct {
	sync.Mutex
	n int
}

// tool is one tool call with a process-unique call ID.
func tool(name string, arguments any) reply {
	raw, err := json.Marshal(arguments)
	if err != nil {
		panic(err)
	}
	callSeq.Lock()
	callSeq.n++
	id := fmt.Sprintf("call-%d", callSeq.n)
	callSeq.Unlock()
	return reply{content: []types.ContentBlock{faux.ToolCall(name, raw, id)}}
}

func github(name string, arguments any) reply {
	return tool(extension.ToolName(model.ExtensionID, name), arguments)
}

func bash(command string) reply { return tool("bash", map[string]string{"command": command}) }

func text(body string) reply { return reply{content: []types.ContentBlock{faux.Text(body)}} }

func gated(gate chan struct{}, r reply) reply {
	r.gate = gate
	return r
}

// commitFile is the worker's shell step that commits one file.
func commitFile(name, content string) reply {
	return bash(fmt.Sprintf("printf '%s\\n' > %s && git add %s && git commit -q -m 'Change %s'", content, name, name, name))
}

// world is a repository on the fake GitHub, its bare git storage, an
// operator clone whose origin names the repository, and a serve process
// with the GitHub runner. restart closes serve and starts another over the
// same directories, as a process restart would.
type world struct {
	t       *testing.T
	ctx     context.Context
	dir     string
	bare    string
	server  *ghfake.Server
	repo    *ghfake.Repo
	writes  *ghfake.Writes
	workers *workers
	config  model.Config

	serve *serve.Server
}

func newWorld(t *testing.T, configure func(*model.Config)) *world {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	w := &world{t: t, ctx: context.Background(), dir: dir, bare: filepath.Join(dir, "remote.git"), workers: newWorkers()}
	w.git(dir, "init", "--quiet", "--bare", "-b", "main", w.bare)
	seed := filepath.Join(dir, "seed")
	w.git(dir, "init", "--quiet", "-b", "main", seed)
	if err := os.WriteFile(filepath.Join(seed, "README.md"), []byte("widgets\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	w.git(seed, "add", "README.md")
	w.git(seed, "commit", "--quiet", "-m", "base")
	w.git(seed, "push", "--quiet", w.bare, "main")
	clone := filepath.Join(dir, "clone")
	w.git(dir, "clone", "--quiet", w.bare, clone)
	// The clone names the repository; fetches reach the bare repository.
	w.git(clone, "remote", "set-url", "origin", "https://github.com/"+repoName+".git")
	w.git(clone, "config", "url."+w.bare+".insteadOf", "https://github.com/"+repoName+".git")
	// Worker worktrees share the clone's configuration, so the worker's
	// shell commits with this identity.
	w.git(clone, "config", "user.name", "worker")
	w.git(clone, "config", "user.email", "worker@example.com")

	w.server = ghfake.NewServer(t)
	w.repo = ghfake.NewRepo(w.server, repoName, bot, epoch)
	w.writes = w.repo.ServeWrites(w.server, w.bare)
	w.config = model.Config{
		Version:    1,
		Repository: repoName,
		Account:    model.AccountConfig{Login: bot.Login, ID: bot.ID, GHPath: ghfake.Executable(), GHConfigDir: filepath.Join(dir, "gh")},
		Clone:      clone,
		Poll:       model.PollConfig{Interval: model.Duration(time.Hour)},
		Admission:  model.AdmissionConfig{Mode: model.ModeObserve, Actors: []model.ActorID{{Login: parth.Login, ID: parth.ID}}},
		Workers:    model.WorkersConfig{Max: 2},
		Budgets: model.BudgetsConfig{
			Run:       model.RunBudget{MaxTurns: 30, MaxCostUSD: 1, MaxWallTime: model.Duration(time.Hour)},
			TicketUSD: 10, RepoUSD: 100, RepoPeriod: model.Duration(720 * time.Hour), MaxAutoWakesPerDay: 12,
		},
		Merge: "manual",
	}
	if configure != nil {
		configure(&w.config)
	}
	raw, err := json.Marshal(w.config)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "github.json")
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	// The operator file goes through the same loader as serve -github-config.
	if w.config, err = githubsource.LoadConfig(path, repoName, filepath.Join(dir, "sessions")); err != nil {
		t.Fatalf("load config: %v", err)
	}
	w.start()
	t.Cleanup(w.stop)
	return w
}

func (w *world) git(dir string, args ...string) string {
	w.t.Helper()
	command := exec.Command("git", append([]string{"-C", dir}, args...)...)
	command.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_AUTHOR_NAME=seed", "GIT_AUTHOR_EMAIL=seed@example.com", "GIT_COMMITTER_NAME=seed", "GIT_COMMITTER_EMAIL=seed@example.com")
	out, err := command.CombinedOutput()
	if err != nil {
		w.t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

// start opens a serve process with the runner.
func (w *world) start() {
	w.t.Helper()
	server, err := serve.New(serve.Options{
		SessionDir:        filepath.Join(w.dir, "sessions"),
		Cwd:               w.dir,
		PermissionTimeout: 5 * time.Second,
		ShutdownTimeout:   30 * time.Second,
		Stack:             &tempest.StackConfig{Model: w.workers.provider.Model(), StreamFn: w.workers.stream, ContextFiles: []prompt.ContextFile{}},
		GitHub: &githubsource.Options{
			Config: w.config, Clock: w.repo,
			Environ:          []string{"PATH=" + os.Getenv("PATH")},
			GHEnv:            w.server.Env(),
			PushRepository:   w.bare,
			DispatchInterval: 100 * time.Millisecond,
			OutboxInterval:   100 * time.Millisecond,
		},
	})
	if err != nil {
		w.t.Fatalf("serve: %v", err)
	}
	w.serve = server
}

func (w *world) stop() {
	if w.serve == nil {
		return
	}
	if err := w.serve.Close(w.ctx); err != nil {
		w.t.Errorf("close serve: %v", err)
	}
	w.serve = nil
}

func (w *world) restart() {
	w.t.Helper()
	w.stop()
	w.start()
}

func (w *world) service() *githubsource.Service { return w.serve.GitHub() }

// poll advances GitHub's clock a little, so every write sorts after what
// was read, and runs one round now.
func (w *world) poll() model.PollReport {
	w.t.Helper()
	w.repo.Advance(5 * time.Second)
	report, err := w.service().PollNow(w.ctx)
	if err != nil {
		w.t.Fatalf("poll: %v", err)
	}
	for _, failure := range report.Errors {
		w.t.Logf("poll resource %s failed: %s", failure.Resource, failure.Message)
	}
	return report
}

func (w *world) eventually(what string, cond func() bool) {
	w.t.Helper()
	deadline := time.Now().Add(waitLimit)
	for !cond() {
		if time.Now().After(deadline) {
			w.t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// mention posts "@bot <text>" from author on number and notifies the bot.
func (w *world) mention(number int, author ghfake.User, text string) {
	w.repo.Advance(time.Second)
	w.repo.Comment(number, author, "@"+bot.Login+" "+text)
	w.repo.Notify(number)
}

func (w *world) tickets() []model.TicketSummary {
	w.t.Helper()
	page, err := w.service().Control().Tickets(w.ctx, model.TicketQuery{Filter: model.FilterAll, Limit: 100})
	if err != nil {
		w.t.Fatalf("tickets: %v", err)
	}
	return page.Tickets
}

// ticketOn is the Ticket requested on thread number, empty when none.
func (w *world) ticketOn(number int) model.TicketID {
	for _, ticket := range w.tickets() {
		if ticket.Thread.Number == number {
			return ticket.ID
		}
	}
	return ""
}

func (w *world) detail(id model.TicketID) model.TicketDetail {
	w.t.Helper()
	detail, err := w.service().Control().Ticket(w.ctx, id)
	if err != nil {
		w.t.Fatalf("ticket %s: %v", id, err)
	}
	return detail
}

// idle reports that the Ticket has no live dispatch and, while open and
// not held, handled every Input.
func (w *world) idle(id model.TicketID) bool {
	detail := w.detail(id)
	for _, dispatch := range detail.Dispatches {
		switch dispatch.State {
		case model.DispatchFinished, model.DispatchCancelled:
		default:
			return false
		}
	}
	ticket := detail.Ticket
	return ticket.Lifecycle == model.LifecycleEnded || ticket.Hold != model.HoldNone || ticket.HandledSeq >= ticket.InputSeq
}

func (w *world) settled(id model.TicketID) {
	w.t.Helper()
	w.eventually("ticket "+string(id)+" to settle", func() bool { return w.idle(id) })
}

// botComments are the account's conversation comments on number.
func (w *world) botComments(number int) []ghfake.Comment {
	var out []ghfake.Comment
	for _, comment := range w.repo.Comments(number) {
		if comment.User.ID == bot.ID {
			out = append(out, comment)
		}
	}
	return out
}

func (w *world) remoteHead(branch string) string {
	command := exec.Command("git", "--git-dir", w.bare, "rev-parse", "--verify", "--quiet", "refs/heads/"+branch)
	out, err := command.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

func (w *world) worktreeHead(id model.TicketID) string {
	return w.git(filepath.Join(w.config.StateDir, "worktrees", string(id)), "rev-parse", "HEAD")
}

func (w *world) requestsContaining(number int, needle string) int {
	count := 0
	for _, request := range w.workers.requests(number) {
		if strings.Contains(request, needle) {
			count++
		}
	}
	return count
}

// TestGitHubEndToEndTicketLifecycle walks one Ticket from an authorized
// mention to a merged PR through serve: the worker commits in its
// worktree, opens the PR and replies through the outbox, a review comment
// wakes it, it pushes a fix, CI and an approval land on the new head, and
// the merge ends the Ticket.
func TestGitHubEndToEndTicketLifecycle(t *testing.T) {
	t.Parallel()
	w := newWorld(t, nil)
	issue := w.repo.OpenIssue(parth, "Widget is broken", "It breaks on Tuesdays.")
	w.workers.script(issue,
		github("report", map[string]string{"status": "progress", "summary": "Looking at the widget."}),
		commitFile("fix.txt", "fixed"),
		github("open_pr", map[string]any{"title": "Fix the widget", "body": "Fixes the Tuesday break.", "draft": false}),
		github("reply", map[string]string{"body": "I opened a pull request with the fix."}),
		github("report", map[string]string{"status": "ready", "summary": "PR is ready for review."}),
		text("Ready for review."),
	)
	w.mention(issue, parth, "please fix this")
	w.poll()
	w.eventually("a ticket on the issue", func() bool { return w.ticketOn(issue) != "" })
	id := w.ticketOn(issue)
	w.settled(id)

	detail := w.detail(id)
	if len(detail.Ticket.PRs) != 1 || !detail.Ticket.PRs[0].Own {
		t.Fatalf("ticket PRs after open_pr = %+v; outbox %+v; worker saw %q", detail.Ticket.PRs, detail.Outbox, w.workers.requests(issue))
	}
	pr := detail.Ticket.PRs[0].Number
	pulls := w.repo.Pulls()
	if len(pulls) != 1 || pulls[0].Number != pr || pulls[0].Head.Ref != model.TicketBranch(id) || pulls[0].User.ID != bot.ID {
		t.Fatalf("fake pulls = %+v", pulls)
	}
	head := w.worktreeHead(id)
	if got := w.remoteHead(model.TicketBranch(id)); got != head || pulls[0].Head.SHA != head {
		t.Fatalf("pushed head %q, PR head %q, worktree head %q", got, pulls[0].Head.SHA, head)
	}
	if comments := w.botComments(issue); len(comments) != 1 || !strings.Contains(comments[0].Body, "I opened a pull request") || !strings.Contains(comments[0].Body, "<!-- tempest:") {
		t.Fatalf("bot comments on the issue = %+v", comments)
	}
	if detail.Stage != model.StageReview {
		t.Fatalf("stage after a ready report with an open PR = %q", detail.Stage)
	}
	for _, intent := range detail.Outbox {
		if intent.State != model.EffectConfirmed {
			t.Fatalf("outbox intent %s is %s", intent.ID, intent.State)
		}
	}

	// A review comment from the requester wakes the worker, which pushes a
	// fix to the same branch.
	w.workers.script(issue,
		commitFile("edge.txt", "edge case"),
		github("push", map[string]any{}),
		github("report", map[string]string{"status": "waiting", "summary": "Pushed the edge case; waiting on CI."}),
		text("Waiting on CI."),
	)
	w.repo.Advance(time.Second)
	w.repo.Comment(pr, parth, "Please also handle the edge case.")
	w.repo.Notify(pr)
	w.poll()
	w.eventually("the review comment to reach the worker", func() bool {
		return w.requestsContaining(issue, "Please also handle the edge case.") > 0
	})
	w.settled(id)
	fix := w.worktreeHead(id)
	if fix == head || w.remoteHead(model.TicketBranch(id)) != fix {
		t.Fatalf("fix %q not pushed (remote %q, first head %q)", fix, w.remoteHead(model.TicketBranch(id)), head)
	}

	// GitHub moves the PR head; CI passes and the requester approves there.
	w.repo.Advance(time.Second)
	w.repo.Push(pr, fix)
	w.repo.SetCheckRun(fix, "ci", "completed", "success")
	w.repo.Review(pr, parth, ghfake.ReviewApproved, "")
	w.poll()
	w.eventually("CI on the new head to be recorded", func() bool {
		for _, record := range w.detail(id).PullRequests {
			if record.Number == pr && record.HeadSHA == fix && record.CI != nil && record.CI.SHA == fix {
				return true
			}
		}
		return false
	})
	w.settled(id)

	w.repo.Advance(time.Second)
	w.repo.Merge(pr, fix)
	w.poll()
	w.eventually("the ticket to end merged", func() bool {
		ticket := w.detail(id).Ticket
		return ticket.Lifecycle == model.LifecycleEnded && ticket.Outcome == model.OutcomeMerged
	})
	if pulls := w.repo.Pulls(); len(pulls) != 1 {
		t.Fatalf("pulls after merge = %d", len(pulls))
	}
	if comments := w.botComments(issue); len(comments) != 1 {
		t.Fatalf("bot comments after merge = %d", len(comments))
	}
	if comments := w.botComments(pr); len(comments) != 0 {
		t.Fatalf("bot comments on the PR = %+v", comments)
	}
}

// TestGitHubEndToEndUnlistedActorDenied: a mention from someone outside
// admission.actors creates no Ticket, runs no worker and posts nothing.
func TestGitHubEndToEndUnlistedActorDenied(t *testing.T) {
	t.Parallel()
	w := newWorld(t, nil)
	issue := w.repo.OpenIssue(alice, "Please do my homework", "")
	w.mention(issue, alice, "please fix this")
	w.poll()
	page, err := w.service().Control().Events(w.ctx, model.EventQuery{Thread: issue, Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	denied := 0
	for _, view := range page.Events {
		if view.Decision != nil && view.Decision.Kind == model.DecisionDenied {
			denied++
		}
	}
	if denied != 1 {
		t.Fatalf("denied decisions = %d in %+v", denied, page.Events)
	}
	if tickets := w.tickets(); len(tickets) != 0 {
		t.Fatalf("tickets = %+v", tickets)
	}
	if calls := w.workers.requests(issue); len(calls) != 0 {
		t.Fatalf("worker ran %d requests", len(calls))
	}
	if comments := w.botComments(issue); len(comments) != 0 {
		t.Fatalf("bot replied to a denied actor: %+v", comments)
	}
}

// TestGitHubEndToEndStopCommand: "@bot stop" from a listed actor cancels
// the running worker and ends the Ticket stopped for good. A later fresh
// mention never reopens it; per admission rule 4 it requests a new Ticket.
func TestGitHubEndToEndStopCommand(t *testing.T) {
	t.Parallel()
	w := newWorld(t, nil)
	issue := w.repo.OpenIssue(parth, "Long task", "")
	gate := make(chan struct{})
	defer close(gate)
	w.workers.script(issue, gated(gate, text("never")))
	w.mention(issue, parth, "please start")
	w.poll()
	w.eventually("the worker to start", func() bool { return w.workers.running(issue) == 1 })
	id := w.ticketOn(issue)

	w.repo.Advance(time.Second)
	w.repo.Comment(issue, parth, "@"+bot.Login+" stop")
	w.repo.Notify(issue)
	w.poll()
	w.eventually("the run to be cancelled", func() bool { return w.workers.running(issue) == 0 })
	w.eventually("the ticket to end stopped", func() bool {
		ticket := w.detail(id).Ticket
		return ticket.Stopped && ticket.Lifecycle == model.LifecycleEnded
	})
	w.settled(id)
	dispatches := len(w.detail(id).Dispatches)
	if outcome := w.detail(id).Ticket.Outcome; outcome != model.OutcomeStopped {
		t.Fatalf("outcome = %q", outcome)
	}

	w.mention(issue, parth, "actually please continue")
	w.poll()
	w.eventually("the fresh mention to request a new ticket", func() bool { return len(w.tickets()) == 2 })
	for _, ticket := range w.tickets() {
		if ticket.ID != id {
			w.settled(ticket.ID)
		}
	}
	detail := w.detail(id)
	if !detail.Ticket.Stopped || detail.Ticket.Lifecycle != model.LifecycleEnded || len(detail.Dispatches) != dispatches {
		t.Fatalf("stopped ticket moved: %+v with %d dispatches (was %d)", detail.Ticket, len(detail.Dispatches), dispatches)
	}
}

// TestGitHubEndToEndHumanSlot: with workers.max 1, a human follow-up on a
// quiescent Ticket runs in the extra human slot beside a busy normal
// slot, while a new Ticket's launch waits for the normal slot.
func TestGitHubEndToEndHumanSlot(t *testing.T) {
	t.Parallel()
	w := newWorld(t, func(config *model.Config) { config.Workers.Max = 1 })
	first := w.repo.OpenIssue(parth, "First", "")
	w.mention(first, parth, "please do the first thing")
	w.poll()
	w.eventually("the first ticket", func() bool { return w.ticketOn(first) != "" })
	firstID := w.ticketOn(first)
	w.settled(firstID)

	second := w.repo.OpenIssue(parth, "Second", "")
	busy := make(chan struct{})
	w.workers.script(second, gated(busy, text("second done")))
	w.mention(second, parth, "please do the second thing")
	w.poll()
	w.eventually("the second worker to hold the normal slot", func() bool { return w.workers.running(second) == 1 })

	third := w.repo.OpenIssue(parth, "Third", "")
	w.mention(third, parth, "please do the third thing")
	followUp := make(chan struct{})
	w.workers.script(first, gated(followUp, text("follow-up done")))
	w.repo.Advance(time.Second)
	w.repo.Comment(first, parth, "One more thing on the first.")
	w.repo.Notify(first)
	w.poll()
	w.eventually("the first ticket's follow-up to run in the human slot", func() bool { return w.workers.running(first) == 1 })
	status, err := w.service().Control().Status(w.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if status.Occupancy.Normal != 1 || status.Occupancy.Human != 1 || status.Queued != 1 {
		t.Fatalf("occupancy %+v, queued %d; want 1 normal, 1 human, the third launch queued", status.Occupancy, status.Queued)
	}
	if len(w.workers.requests(third)) != 0 {
		t.Fatal("the third ticket launched without a free normal slot")
	}
	close(followUp)
	w.settled(firstID)
	if len(w.workers.requests(third)) != 0 {
		t.Fatal("the third ticket launched in the human slot")
	}
	close(busy)
	w.eventually("the third ticket to launch once the normal slot frees", func() bool { return len(w.workers.requests(third)) > 0 })
	w.settled(w.ticketOn(third))
}

// TestGitHubEndToEndRestartBoundaries restarts serve mid-run before any
// write, after the PR was opened while the worker still runs, with the PR
// reply lost, and after the Ticket ended. Each restart recovers the
// worker under a new generation; GitHub ends with one PR and one comment.
func TestGitHubEndToEndRestartBoundaries(t *testing.T) {
	t.Parallel()
	w := newWorld(t, nil)
	issue := w.repo.OpenIssue(parth, "Restartable", "")

	// Boundary 1: the launch run is in flight (submitted, no write yet).
	launch := make(chan struct{})
	w.workers.script(issue, gated(launch, text("never")))
	w.mention(issue, parth, "please fix")
	w.poll()
	w.eventually("the launch run", func() bool { return w.workers.running(issue) == 1 })
	id := w.ticketOn(issue)
	w.restart()
	close(launch)
	w.eventually("a recovery generation", func() bool { return w.detail(id).Ticket.Gen >= 2 })

	// Boundary 2: the PR was opened, GitHub's reply to it was lost, and
	// the worker is still running when serve restarts.
	afterPR := make(chan struct{})
	w.writes.LoseAck("POST", "/repos/"+repoName+"/pulls", 502, 1)
	w.workers.script(issue,
		commitFile("fix.txt", "fixed"),
		github("open_pr", map[string]any{"title": "Fix", "body": "Fix.", "draft": false}),
		gated(afterPR, text("never")),
	)
	w.eventually("the recovery run to deliver the open_pr result", func() bool {
		return w.requestsContaining(issue, extension.ToolName(model.ExtensionID, model.ToolOpenPR)) > 0 && w.workers.running(issue) == 1
	})
	w.eventually("the PR intent to settle", func() bool {
		for _, intent := range w.detail(id).Outbox {
			if intent.Kind == model.EffectOpenPR && intent.State == model.EffectConfirmed {
				return true
			}
		}
		return false
	})
	gen := w.detail(id).Ticket.Gen
	w.restart()
	close(afterPR)
	w.eventually("another recovery generation", func() bool { return w.detail(id).Ticket.Gen > gen })

	// Boundary 3: the recovered worker replies and reports ready; the
	// restart lands while that run is in flight.
	afterReply := make(chan struct{})
	w.workers.script(issue,
		github("reply", map[string]string{"body": "PR is up."}),
		gated(afterReply, github("report", map[string]string{"status": "ready", "summary": "Ready."})),
		text("Ready."),
	)
	w.eventually("the reply to be posted", func() bool { return len(w.botComments(issue)) == 1 })
	w.eventually("the worker to wait before its report", func() bool { return w.workers.running(issue) == 1 })
	w.restart()
	close(afterReply)
	w.settled(id)

	// Boundary 4: the merge ends the Ticket; a restart afterwards
	// dispatches nothing.
	pulls := w.repo.Pulls()
	if len(pulls) != 1 {
		t.Fatalf("pulls = %+v", pulls)
	}
	w.repo.Advance(time.Second)
	w.repo.Merge(pulls[0].Number, pulls[0].Head.SHA)
	w.poll()
	w.eventually("the ticket to end merged", func() bool { return w.detail(id).Ticket.Outcome == model.OutcomeMerged })
	requests := len(w.workers.requests(issue))
	dispatches := len(w.detail(id).Dispatches)
	w.restart()
	w.poll()
	time.Sleep(300 * time.Millisecond)

	if got := len(w.repo.Pulls()); got != 1 {
		t.Fatalf("pulls after restarts = %d", got)
	}
	if got := countRequests(w.server.Requests(), "POST", "/pulls"); got != 1 {
		t.Fatalf("POST /pulls sent %d times", got)
	}
	if comments := w.botComments(issue); len(comments) != 1 {
		t.Fatalf("bot comments after restarts = %+v", comments)
	}
	if got := len(w.workers.requests(issue)); got != requests {
		t.Fatalf("worker ran after the ticket ended: %d -> %d requests", requests, got)
	}
	if got := len(w.detail(id).Dispatches); got != dispatches {
		t.Fatalf("dispatches after the ticket ended: %d -> %d", dispatches, got)
	}
}

func countRequests(requests []ghfake.RecordedRequest, method, suffix string) int {
	count := 0
	for _, request := range requests {
		if request.Method == method && strings.HasSuffix(request.Path, suffix) {
			count++
		}
	}
	return count
}

// TestGitHubEndToEndBotLoopStopsAtWakeLimit: two bots that keep answering
// each other on a Ticket's thread wake the worker only
// budgets.maxAutoWakesPerDay times; then the Ticket holds wake_limit and
// later bot comments wake nothing.
func TestGitHubEndToEndBotLoopStopsAtWakeLimit(t *testing.T) {
	t.Parallel()
	reviewer := ghfake.User{Login: "reviewer[bot]", ID: 3333, Type: ghfake.TypeBot}
	linter := ghfake.User{Login: "linter[bot]", ID: 4444, Type: ghfake.TypeBot}
	w := newWorld(t, func(config *model.Config) {
		config.Admission.Bots = []model.ActorID{{Login: reviewer.Login, ID: reviewer.ID}, {Login: linter.Login, ID: linter.ID}}
		config.Budgets.MaxAutoWakesPerDay = 2
	})
	issue := w.repo.OpenIssue(parth, "Bots", "")
	w.mention(issue, parth, "please look")
	w.poll()
	w.eventually("the ticket", func() bool { return w.ticketOn(issue) != "" })
	id := w.ticketOn(issue)
	w.settled(id)

	launched := len(w.detail(id).Dispatches)
	for round := range 4 {
		author := reviewer
		if round%2 == 1 {
			author = linter
		}
		w.repo.Advance(time.Second)
		w.repo.Comment(issue, author, fmt.Sprintf("bot note %d", round))
		w.poll()
		w.settled(id)
	}
	detail := w.detail(id)
	if detail.Ticket.Hold != model.HoldWakeLimit {
		t.Fatalf("hold = %q after four bot comments", detail.Ticket.Hold)
	}
	if woken := len(detail.Dispatches) - launched; woken != 2 {
		t.Fatalf("bot comments woke the worker %d times; want 2", woken)
	}
	if got := w.requestsContaining(issue, "bot note 3"); got != 0 {
		t.Fatalf("the worker saw a bot comment past the wake limit %d times", got)
	}
}

// TestGitHubEndToEndPauseHoldsDispatch: while the operator has paused the
// repository, intake still records an authorized request as a Ticket but
// no worker runs; unpausing launches it.
func TestGitHubEndToEndPauseHoldsDispatch(t *testing.T) {
	t.Parallel()
	w := newWorld(t, nil)
	control := w.service().Control()
	if err := control.SetPaused(w.ctx, model.OperatorRequest{ID: "pause-1", Reason: "maintenance", At: w.repo.Now()}, true); err != nil {
		t.Fatal(err)
	}
	issue := w.repo.OpenIssue(parth, "Paused", "")
	w.mention(issue, parth, "please fix")
	w.poll()
	w.eventually("the ticket", func() bool { return w.ticketOn(issue) != "" })
	time.Sleep(300 * time.Millisecond)
	if calls := w.workers.requests(issue); len(calls) != 0 {
		t.Fatalf("worker ran while paused: %d requests", len(calls))
	}
	if err := control.SetPaused(w.ctx, model.OperatorRequest{ID: "unpause-1", Reason: "done", At: w.repo.Now()}, false); err != nil {
		t.Fatal(err)
	}
	w.eventually("the launch after unpause", func() bool { return len(w.workers.requests(issue)) > 0 })
	w.settled(w.ticketOn(issue))
}
