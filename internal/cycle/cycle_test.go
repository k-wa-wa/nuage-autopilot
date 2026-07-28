package cycle

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/k-wa-wa/nuage-workspace/autopilot/internal/github"
)

// call は mockServer が記録した 1 回のミューテーション系リクエスト（POST/DELETE）を表す。
type call struct {
	Method string
	Path   string
	Body   string
}

// mockServer は internal/github.Client の向き先となる GitHub API のスタブである。
// テストは実際の GitHub API には一切到達しない。
type mockServer struct {
	t        *testing.T
	server   *httptest.Server
	issues   []map[string]any
	prs      []map[string]any
	comments map[int][]map[string]any // issue/PR 番号 -> コメント一覧
	login    string

	mu    sync.Mutex
	calls []call
}

func newMockServer(t *testing.T) *mockServer {
	t.Helper()
	m := &mockServer{t: t, login: "nuage-autopilot", comments: map[int][]map[string]any{}}
	m.server = httptest.NewServer(http.HandlerFunc(m.handle))
	t.Cleanup(m.server.Close)
	return m
}

func (m *mockServer) client() *github.Client {
	return github.NewClient("test-token", github.WithBaseURL(m.server.URL))
}

func (m *mockServer) recordCall(r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	m.mu.Lock()
	m.calls = append(m.calls, call{Method: r.Method, Path: r.URL.Path, Body: string(body)})
	m.mu.Unlock()
}

func (m *mockServer) mutatingCallCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.calls)
}

func (m *mockServer) handle(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/repos/k-wa-wa/pechka/issues":
		_ = json.NewEncoder(w).Encode(m.issues)
		return
	case r.Method == http.MethodGet && r.URL.Path == "/repos/k-wa-wa/pechka/pulls":
		_ = json.NewEncoder(w).Encode(m.prs)
		return
	case r.Method == http.MethodGet && r.URL.Path == "/user":
		_ = json.NewEncoder(w).Encode(map[string]string{"login": m.login})
		return
	case r.Method == http.MethodPost && r.URL.Path == "/repos/k-wa-wa/pechka/labels":
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{}`))
		return
	case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/check-runs"):
		_, _ = w.Write([]byte(`{"total_count": 0, "check_runs": []}`))
		return
	}

	// /repos/k-wa-wa/pechka/issues/{number}/comments
	var number int
	if n, _ := fmt.Sscanf(r.URL.Path, "/repos/k-wa-wa/pechka/issues/%d/comments", &number); n == 1 && r.Method == http.MethodGet {
		_ = json.NewEncoder(w).Encode(m.comments[number])
		return
	}

	// ラベル付与・解除・コメント投稿はすべてミューテーションとして記録する。
	if r.Method == http.MethodPost || r.Method == http.MethodDelete {
		m.recordCall(r)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{}`))
		return
	}

	http.Error(w, fmt.Sprintf("unhandled request: %s %s", r.Method, r.URL.Path), http.StatusNotFound)
}

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func rfc3339(t time.Time) string {
	return t.UTC().Format(time.RFC3339)
}

// fakeDispatcher は Dispatcher のテスト用フェイクである。実際の claude は一切起動せず、
// 呼び出しを記録し、あらかじめ設定した decision/ok/err を返すのみである。
type fakeDispatcher struct {
	decision Decision
	ok       bool
	err      error

	calls []dispatchCall
}

type dispatchCall struct {
	Repo       string
	Candidates []DispatchCandidate
}

func (f *fakeDispatcher) Dispatch(_ context.Context, repo string, candidates []DispatchCandidate) (Decision, bool, error) {
	f.calls = append(f.calls, dispatchCall{Repo: repo, Candidates: candidates})
	return f.decision, f.ok, f.err
}

// fakeExecutor は LLMExecutor のテスト用フェイクである。実際の git/gh/claude は
// 一切起動せず、呼び出しを記録し、あらかじめ設定した err を返すのみである。
type fakeExecutor struct {
	err   error
	calls []fakeExecutorCall
}

type fakeExecutorCall struct {
	Repo   string
	Number int
	Kind   itemKind
	Worker string
}

func (f *fakeExecutor) Execute(_ context.Context, repoName string, item Item, worker string) error {
	f.calls = append(f.calls, fakeExecutorCall{Repo: repoName, Number: item.Number, Kind: item.Kind, Worker: worker})
	return f.err
}

func TestRun_NoActionWhenRepoHasNothingOpen(t *testing.T) {
	m := newMockServer(t)
	dispatcher := &fakeDispatcher{}

	result, err := Run(context.Background(), testLogger(), m.client(), dispatcher, &fakeExecutor{}, "k-wa-wa/pechka", "/var/lib/nuage-autopilot", nil)
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if result.Action != ActionNoop {
		t.Fatalf("result.Action = %q, want %q", result.Action, ActionNoop)
	}
	if result.Repo != "k-wa-wa/pechka" || result.StateDir != "/var/lib/nuage-autopilot" {
		t.Fatalf("result = %+v, want Repo/StateDir preserved", result)
	}
	if len(dispatcher.calls) != 0 {
		t.Fatalf("dispatcher.calls = %+v, want dispatcher not to be called when there is nothing open", dispatcher.calls)
	}
	if got := m.mutatingCallCount(); got != 0 {
		t.Fatalf("mutating call count = %d, want 0", got)
	}
}

func TestRun_ExcludesItemsWithAnyAgentLabel(t *testing.T) {
	m := newMockServer(t)
	m.issues = []map[string]any{
		{"number": 1, "title": "already claimed", "state": "open", "labels": []any{map[string]string{"name": "agent:running"}}, "user": map[string]string{"login": "alice", "type": "User"}, "created_at": rfc3339(time.Now()), "updated_at": rfc3339(time.Now())},
		{"number": 2, "title": "awaiting human", "state": "open", "labels": []any{map[string]string{"name": "agent:awaiting_user_review"}}, "user": map[string]string{"login": "alice", "type": "User"}, "created_at": rfc3339(time.Now()), "updated_at": rfc3339(time.Now())},
	}

	dispatcher := &fakeDispatcher{}
	result, err := Run(context.Background(), testLogger(), m.client(), dispatcher, &fakeExecutor{}, "k-wa-wa/pechka", "/var/lib/nuage-autopilot", nil)
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if result.Action != ActionNoop {
		t.Fatalf("result.Action = %q, want %q (all items are opted out)", result.Action, ActionNoop)
	}
	if len(dispatcher.calls) != 0 {
		t.Fatalf("dispatcher.calls = %+v, want dispatcher not to be called when every item is opted out", dispatcher.calls)
	}
}

func TestRun_PassesOnlyEligibleItemsAsCandidatesToDispatcher(t *testing.T) {
	m := newMockServer(t)
	m.issues = []map[string]any{
		{"number": 1, "title": "already claimed", "state": "open", "labels": []any{map[string]string{"name": "agent:running"}}, "user": map[string]string{"login": "alice", "type": "User"}, "created_at": rfc3339(time.Now()), "updated_at": rfc3339(time.Now())},
		{"number": 3, "title": "fresh issue", "state": "open", "body": "please implement X because Y", "labels": []any{}, "user": map[string]string{"login": "alice", "type": "User"}, "created_at": rfc3339(time.Now()), "updated_at": rfc3339(time.Now())},
	}

	dispatcher := &fakeDispatcher{ok: false}
	_, err := Run(context.Background(), testLogger(), m.client(), dispatcher, &fakeExecutor{}, "k-wa-wa/pechka", "/var/lib/nuage-autopilot", nil)
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	if len(dispatcher.calls) != 1 {
		t.Fatalf("dispatcher.calls = %+v, want exactly 1 call", dispatcher.calls)
	}
	candidates := dispatcher.calls[0].Candidates
	if len(candidates) != 1 || candidates[0].Number != 3 {
		t.Fatalf("candidates = %+v, want only issue #3 (agent:running item excluded)", candidates)
	}
	if candidates[0].Author != "alice" || candidates[0].Title != "fresh issue" {
		t.Fatalf("candidates[0] = %+v, want Author=alice Title=%q", candidates[0], "fresh issue")
	}
	// Issue 本文が GitHub API のレスポンスから Item を経由して DispatchCandidate まで
	// 届いていることを確認する（本タスクの主眼）。
	if candidates[0].Body != "please implement X because Y" {
		t.Fatalf("candidates[0].Body = %q, want %q", candidates[0].Body, "please implement X because Y")
	}
}

func TestRun_LoopLimitLabelsAwaitingUserReviewAndSkipsDispatch(t *testing.T) {
	m := newMockServer(t)
	m.issues = []map[string]any{
		{"number": 4, "title": "stuck in a loop", "state": "open", "labels": []any{}, "user": map[string]string{"login": "alice", "type": "User"}, "created_at": rfc3339(time.Now()), "updated_at": rfc3339(time.Now())},
	}
	// LoopLimit (既定 5) 以上の Bot コメントが、最後の人間コメントより後に並んでいる。
	comments := make([]map[string]any, 0, LoopLimit)
	for i := 0; i < LoopLimit; i++ {
		comments = append(comments, map[string]any{
			"id": i, "body": "still working on it", "user": map[string]string{"login": "nuage-autopilot", "type": "User"},
			"created_at": rfc3339(time.Now().Add(time.Duration(i) * time.Minute)),
		})
	}
	m.comments[4] = comments

	dispatcher := &fakeDispatcher{}
	result, err := Run(context.Background(), testLogger(), m.client(), dispatcher, &fakeExecutor{}, "k-wa-wa/pechka", "/var/lib/nuage-autopilot", nil)
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if result.Action != ActionAwaitingUserReview {
		t.Fatalf("result.Action = %q, want %q", result.Action, ActionAwaitingUserReview)
	}
	if result.ItemNumber != 4 {
		t.Fatalf("result.ItemNumber = %d, want 4", result.ItemNumber)
	}
	if len(dispatcher.calls) != 0 {
		t.Fatalf("dispatcher.calls = %+v, want dispatcher not to be called this cycle", dispatcher.calls)
	}

	if got := m.mutatingCallCount(); got != 1 {
		t.Fatalf("mutating call count = %d, want 1 (add agent:awaiting_user_review)", got)
	}
	want := "/repos/k-wa-wa/pechka/issues/4/labels"
	if m.calls[0].Method != http.MethodPost || m.calls[0].Path != want {
		t.Fatalf("call = %+v, want POST %s", m.calls[0], want)
	}
	if want := `{"labels":["agent:awaiting_user_review"]}`; m.calls[0].Body != want {
		t.Fatalf("call body = %q, want %q", m.calls[0].Body, want)
	}
}

func TestRun_HumanCommentResetsLoopCounterAndDispatcherIsCalled(t *testing.T) {
	m := newMockServer(t)
	m.issues = []map[string]any{
		{"number": 9, "title": "recovered", "state": "open", "labels": []any{}, "user": map[string]string{"login": "alice", "type": "User"}, "created_at": rfc3339(time.Now()), "updated_at": rfc3339(time.Now())},
	}
	comments := []map[string]any{
		{"id": 1, "body": "bot 1", "user": map[string]string{"login": "nuage-autopilot", "type": "User"}, "created_at": rfc3339(time.Now().Add(-time.Hour))},
		// 人間の最新コメントがあるため、それより前の Bot コメントは数えない。
		{"id": 2, "body": "looks fine, continue", "user": map[string]string{"login": "alice", "type": "User"}, "created_at": rfc3339(time.Now())},
	}
	m.comments[9] = comments

	dispatcher := &fakeDispatcher{ok: false}
	result, err := Run(context.Background(), testLogger(), m.client(), dispatcher, &fakeExecutor{}, "k-wa-wa/pechka", "/var/lib/nuage-autopilot", nil)
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if result.Action != ActionNoop {
		t.Fatalf("result.Action = %q, want %q", result.Action, ActionNoop)
	}
	if len(dispatcher.calls) != 1 {
		t.Fatalf("dispatcher.calls = %+v, want exactly 1 call (loop limit must not trigger)", dispatcher.calls)
	}
}

func TestRun_DispatchesToWorkerAndTogglesRunningLabel(t *testing.T) {
	m := newMockServer(t)
	m.issues = []map[string]any{
		{"number": 7, "title": "needs review", "state": "open", "labels": []any{}, "user": map[string]string{"login": "alice", "type": "User"}, "created_at": rfc3339(time.Now()), "updated_at": rfc3339(time.Now())},
	}

	dispatcher := &fakeDispatcher{ok: true, decision: Decision{Number: 7, Kind: "issue", Worker: WorkerDev, Reason: "spec approved"}}
	executor := &fakeExecutor{}
	result, err := Run(context.Background(), testLogger(), m.client(), dispatcher, executor, "k-wa-wa/pechka", "/var/lib/nuage-autopilot", nil)
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	if result.Action != ActionWorkerExecuted {
		t.Fatalf("result.Action = %q, want %q", result.Action, ActionWorkerExecuted)
	}
	if result.Worker != WorkerDev || result.ItemNumber != 7 {
		t.Fatalf("result = %+v, want Worker=%s ItemNumber=7", result, WorkerDev)
	}

	if len(executor.calls) != 1 {
		t.Fatalf("executor.calls = %d, want 1", len(executor.calls))
	}

	call := executor.calls[0]
	if call.Repo != "k-wa-wa/pechka" || call.Number != 7 || call.Worker != WorkerDev {
		t.Fatalf("executor call = %+v, want repo=k-wa-wa/pechka number=7 worker=%s", call, WorkerDev)
	}

	// agent:running が起動直前に付与され、終了後に外されたことを確認する。
	if got := m.mutatingCallCount(); got != 2 {
		t.Fatalf("mutating call count = %d, want 2 (add agent:running + remove agent:running)", got)
	}
	addWant := "/repos/k-wa-wa/pechka/issues/7/labels"
	if m.calls[0].Method != http.MethodPost || m.calls[0].Path != addWant {
		t.Fatalf("calls[0] = %+v, want POST %s (agent:running)", m.calls[0], addWant)
	}
	removeWant := "/repos/k-wa-wa/pechka/issues/7/labels/agent:running"
	if m.calls[1].Method != http.MethodDelete || m.calls[1].Path != removeWant {
		t.Fatalf("calls[1] = %+v, want DELETE %s", m.calls[1], removeWant)
	}
}

func TestRun_AllowedAuthorsFilter(t *testing.T) {
	m := newMockServer(t)
	m.issues = []map[string]any{
		{"number": 1, "title": "allowed issue", "state": "open", "labels": []any{}, "user": map[string]string{"login": "k-wa-wa", "type": "User"}, "created_at": rfc3339(time.Now()), "updated_at": rfc3339(time.Now())},
		{"number": 2, "title": "unallowed issue", "state": "open", "labels": []any{}, "user": map[string]string{"login": "untrusted", "type": "User"}, "created_at": rfc3339(time.Now()), "updated_at": rfc3339(time.Now())},
	}

	dispatcher := &fakeDispatcher{ok: true, decision: Decision{Number: 1, Kind: "issue", Worker: WorkerSpec}}
	executor := &fakeExecutor{}
	result, err := Run(context.Background(), testLogger(), m.client(), dispatcher, executor, "k-wa-wa/pechka", "/var/lib/nuage-autopilot", []string{"k-wa-wa", "bot-wa-wa"})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if result.ItemNumber != 1 {
		t.Fatalf("result.ItemNumber = %d, want 1", result.ItemNumber)
	}
	if len(dispatcher.calls) != 1 || len(dispatcher.calls[0].Candidates) != 1 {
		t.Fatalf("dispatcher candidates count = %d, want 1 (unallowed author excluded)", len(dispatcher.calls[0].Candidates))
	}
}

func TestRun_DispatcherNoDecisionResultsInNoop(t *testing.T) {
	m := newMockServer(t)
	m.issues = []map[string]any{
		{"number": 10, "title": "ambiguous", "state": "open", "labels": []any{}, "user": map[string]string{"login": "alice", "type": "User"}, "created_at": rfc3339(time.Now()), "updated_at": rfc3339(time.Now())},
	}

	dispatcher := &fakeDispatcher{ok: false}
	executor := &fakeExecutor{}
	result, err := Run(context.Background(), testLogger(), m.client(), dispatcher, executor, "k-wa-wa/pechka", "/var/lib/nuage-autopilot", nil)
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if result.Action != ActionNoop {
		t.Fatalf("result.Action = %q, want %q", result.Action, ActionNoop)
	}
	if len(executor.calls) != 0 {
		t.Fatalf("executor.calls = %+v, want no worker execution", executor.calls)
	}
	if got := m.mutatingCallCount(); got != 0 {
		t.Fatalf("mutating call count = %d, want 0 (no label changes when dispatcher has nothing to do)", got)
	}
}

func TestRun_DispatcherErrorPropagates(t *testing.T) {
	m := newMockServer(t)
	m.issues = []map[string]any{
		{"number": 11, "title": "x", "state": "open", "labels": []any{}, "user": map[string]string{"login": "alice", "type": "User"}, "created_at": rfc3339(time.Now()), "updated_at": rfc3339(time.Now())},
	}

	dispatcher := &fakeDispatcher{err: errors.New("boom")}
	_, err := Run(context.Background(), testLogger(), m.client(), dispatcher, &fakeExecutor{}, "k-wa-wa/pechka", "/var/lib/nuage-autopilot", nil)
	if err == nil {
		t.Fatalf("Run() error = nil, want an error when the dispatcher itself fails")
	}
}

func TestRun_WorkerFailureIsNotFatalAndClearsRunningLabel(t *testing.T) {
	// worker (claude) 自体の実行や、実行した結果としてのタスク失敗は nuage-autopilot 自体の
	// 異常ではない。agent:running のみを外して次サイクルの再検討に委ねる。
	// Run 自体は error を返さない。
	m := newMockServer(t)
	m.issues = []map[string]any{
		{"number": 12, "title": "hard bug", "state": "open", "labels": []any{}, "user": map[string]string{"login": "alice", "type": "User"}, "created_at": rfc3339(time.Now()), "updated_at": rfc3339(time.Now())},
	}

	dispatcher := &fakeDispatcher{ok: true, decision: Decision{Number: 12, Kind: "issue", Worker: WorkerDev}}
	executor := &fakeExecutor{err: errors.New("claude exited with non-zero status")}
	result, err := Run(context.Background(), testLogger(), m.client(), dispatcher, executor, "k-wa-wa/pechka", "/var/lib/nuage-autopilot", nil)
	if err != nil {
		t.Fatalf("Run() error = %v, want nil (worker failure must not fail the cycle)", err)
	}

	if result.Action != ActionWorkerFailed {
		t.Fatalf("result.Action = %q, want %q", result.Action, ActionWorkerFailed)
	}
	if result.Worker != WorkerDev || result.ItemNumber != 12 {
		t.Fatalf("result = %+v, want Worker=%s ItemNumber=12", result, WorkerDev)
	}

	if got := m.mutatingCallCount(); got != 2 {
		t.Fatalf("mutating call count = %d, want 2 (add agent:running + remove agent:running)", got)
	}
}

func TestRun_ProcessesOnlyOneItemPerCycleEvenWithMultipleCandidates(t *testing.T) {
	m := newMockServer(t)
	m.issues = []map[string]any{
		{"number": 20, "title": "first", "state": "open", "labels": []any{}, "user": map[string]string{"login": "alice", "type": "User"}, "created_at": rfc3339(time.Now()), "updated_at": rfc3339(time.Now())},
		{"number": 21, "title": "second", "state": "open", "labels": []any{}, "user": map[string]string{"login": "alice", "type": "User"}, "created_at": rfc3339(time.Now()), "updated_at": rfc3339(time.Now())},
	}

	dispatcher := &fakeDispatcher{ok: true, decision: Decision{Number: 20, Kind: "issue", Worker: WorkerSpec}}
	executor := &fakeExecutor{}
	result, err := Run(context.Background(), testLogger(), m.client(), dispatcher, executor, "k-wa-wa/pechka", "/var/lib/nuage-autopilot", nil)
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if result.ItemNumber != 20 {
		t.Fatalf("result.ItemNumber = %d, want 20", result.ItemNumber)
	}
	if len(dispatcher.calls) != 1 || len(dispatcher.calls[0].Candidates) != 2 {
		t.Fatalf("dispatcher.calls = %+v, want exactly 1 call with 2 candidates", dispatcher.calls)
	}
	if len(executor.calls) != 1 {
		t.Fatalf("executor.calls = %+v, want exactly 1 call (only the dispatcher's chosen item)", executor.calls)
	}
}
