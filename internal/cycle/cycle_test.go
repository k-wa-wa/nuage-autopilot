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

	"autopilot/internal/github"
	"autopilot/internal/report"
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
	singlePR map[int]map[string]any   // GET /pulls/{number}（単体取得）用。無指定なら空の PR を返す。
	login    string

	// failCreateComment が true の場合、POST .../comments（CreateComment）を
	// 常に 500 で失敗させる。internal/cycle/executor_test.go のコメント投稿失敗
	// シナリオの検証用。
	failCreateComment bool

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
	case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/reviews"):
		http.Error(w, `{"message": "Not Found"}`, http.StatusNotFound)
		return
	}

	// /repos/k-wa-wa/pechka/issues/{number}/comments
	var number int
	if n, _ := fmt.Sscanf(r.URL.Path, "/repos/k-wa-wa/pechka/issues/%d/comments", &number); n == 1 && r.Method == http.MethodGet {
		_ = json.NewEncoder(w).Encode(m.comments[number])
		return
	}

	// /repos/k-wa-wa/pechka/pulls/{number}（単体取得。一覧取得の "/pulls" とはパスの
	// セグメント数で区別され、"/reviews" 等はこれより先の switch で処理済みのためここには来ない）。
	if n, _ := fmt.Sscanf(r.URL.Path, "/repos/k-wa-wa/pechka/pulls/%d", &number); n == 1 && r.Method == http.MethodGet {
		body := m.singlePR[number]
		if body == nil {
			body = map[string]any{"number": number, "head": map[string]string{"sha": ""}}
		}
		_ = json.NewEncoder(w).Encode(body)
		return
	}

	if r.Method == http.MethodPost && m.failCreateComment && strings.HasSuffix(r.URL.Path, "/comments") {
		http.Error(w, `{"message": "boom"}`, http.StatusInternalServerError)
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

func issueFixture(number int, author string, updatedAt time.Time, labels ...string) map[string]any {
	labelObjs := make([]any, 0, len(labels))
	for _, l := range labels {
		labelObjs = append(labelObjs, map[string]string{"name": l})
	}
	return map[string]any{
		"number": number, "title": fmt.Sprintf("issue %d", number), "state": "open",
		"labels": labelObjs, "user": map[string]string{"login": author, "type": "User"},
		"created_at": rfc3339(updatedAt), "updated_at": rfc3339(updatedAt),
	}
}

// botStatusComment は nuage-autopilot 自身が投稿した状態行付きコメントの GitHub API
// 表現を作る。
func botStatusComment(id int, worker, status, sha string, at time.Time) map[string]any {
	return map[string]any{
		"id": id, "body": report.Render(worker, status, sha, "summary"),
		"user": map[string]string{"login": "nuage-autopilot", "type": "User"}, "created_at": rfc3339(at),
	}
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
	now := time.Now()
	m.issues = []map[string]any{
		issueFixture(1, "alice", now, "agent:running"),
		issueFixture(2, "alice", now, "agent:awaiting_user_review"),
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

func TestRun_FreshIssueGoesStraightToWorkWithoutCallingDispatcher(t *testing.T) {
	// 状態行が 1 件も無い新規 Issue は遷移表だけで worker=work に決まるため、
	// dispatcher (LLM) を呼ぶ必要が無い。
	m := newMockServer(t)
	now := time.Now()
	m.issues = []map[string]any{
		{"number": 1, "title": "already claimed", "state": "open", "labels": []any{map[string]string{"name": "agent:running"}}, "user": map[string]string{"login": "alice", "type": "User"}, "created_at": rfc3339(now), "updated_at": rfc3339(now)},
		{"number": 3, "title": "fresh issue", "state": "open", "body": "please implement X because Y", "labels": []any{}, "user": map[string]string{"login": "alice", "type": "User"}, "created_at": rfc3339(now), "updated_at": rfc3339(now)},
	}

	dispatcher := &fakeDispatcher{}
	executor := &fakeExecutor{}
	result, err := Run(context.Background(), testLogger(), m.client(), dispatcher, executor, "k-wa-wa/pechka", "/var/lib/nuage-autopilot", nil)
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	if len(dispatcher.calls) != 0 {
		t.Fatalf("dispatcher.calls = %+v, want dispatcher not to be called (decided by the transition table)", dispatcher.calls)
	}
	if result.ItemNumber != 3 || result.Worker != WorkerWork {
		t.Fatalf("result = %+v, want ItemNumber=3 Worker=%s (agent:running item #1 excluded)", result, WorkerWork)
	}
	if len(executor.calls) != 1 || executor.calls[0].Number != 3 || executor.calls[0].Worker != WorkerWork {
		t.Fatalf("executor.calls = %+v, want a single call for issue #3 with worker=%s", executor.calls, WorkerWork)
	}
}

func TestRun_LoopLimitLabelsAwaitingUserReviewAndSkipsDispatch(t *testing.T) {
	m := newMockServer(t)
	now := time.Now()
	m.issues = []map[string]any{
		issueFixture(4, "alice", now),
	}
	// LoopLimit (既定 5) 以上の Bot コメントが、最後の人間コメントより後に並んでいる。
	comments := make([]map[string]any, 0, LoopLimit)
	for i := 0; i < LoopLimit; i++ {
		comments = append(comments, map[string]any{
			"id": i, "body": "still working on it", "user": map[string]string{"login": "nuage-autopilot", "type": "User"},
			"created_at": rfc3339(now.Add(time.Duration(i) * time.Minute)),
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

func TestRun_HumanCommentResetsLoopCounterAndProceeds(t *testing.T) {
	m := newMockServer(t)
	now := time.Now()
	m.issues = []map[string]any{
		issueFixture(9, "alice", now),
	}
	comments := []map[string]any{
		{"id": 1, "body": "bot 1", "user": map[string]string{"login": "nuage-autopilot", "type": "User"}, "created_at": rfc3339(now.Add(-time.Hour))},
		// 人間の最新コメントがあるため、それより前の Bot コメントは数えない。
		{"id": 2, "body": "looks fine, continue", "user": map[string]string{"login": "alice", "type": "User"}, "created_at": rfc3339(now)},
	}
	m.comments[9] = comments

	dispatcher := &fakeDispatcher{}
	executor := &fakeExecutor{}
	result, err := Run(context.Background(), testLogger(), m.client(), dispatcher, executor, "k-wa-wa/pechka", "/var/lib/nuage-autopilot", nil)
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	// どちらのコメントも状態行の形式ではないため、遷移表は「未着手」として扱い、
	// dispatcher を呼ばず直接 work に決める。
	if result.Action != ActionWorkerExecuted || result.Worker != WorkerWork || result.ItemNumber != 9 {
		t.Fatalf("result = %+v, want Action=%s Worker=%s ItemNumber=9", result, ActionWorkerExecuted, WorkerWork)
	}
	if len(dispatcher.calls) != 0 {
		t.Fatalf("dispatcher.calls = %+v, want dispatcher not to be called (loop limit must not trigger, transition table decides directly)", dispatcher.calls)
	}
}

func TestRun_AskOnlyWhenHumanCommentsAfterStatusLine(t *testing.T) {
	// work が status=done を報告した後に人間が追加のコメントを残した場合のみ、
	// 遷移表は判断を保留し (ask) dispatcher を呼ぶ。
	m := newMockServer(t)
	now := time.Now()
	m.issues = []map[string]any{
		issueFixture(7, "alice", now),
	}
	m.comments[7] = []map[string]any{
		botStatusComment(1, WorkerWork, report.StatusDone, "", now.Add(-time.Hour)),
		{"id": 2, "body": "also handle the edge case please", "user": map[string]string{"login": "alice", "type": "User"}, "created_at": rfc3339(now)},
	}

	dispatcher := &fakeDispatcher{ok: true, decision: Decision{Number: 7, Kind: "issue", Worker: WorkerWork, Reason: "human asked for more"}}
	executor := &fakeExecutor{}
	result, err := Run(context.Background(), testLogger(), m.client(), dispatcher, executor, "k-wa-wa/pechka", "/var/lib/nuage-autopilot", nil)
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	if len(dispatcher.calls) != 1 {
		t.Fatalf("dispatcher.calls = %+v, want exactly 1 call", dispatcher.calls)
	}
	candidates := dispatcher.calls[0].Candidates
	if len(candidates) != 1 || candidates[0].Number != 7 {
		t.Fatalf("candidates = %+v, want only issue #7", candidates)
	}
	if candidates[0].PendingReason == "" {
		t.Fatalf("candidates[0].PendingReason is empty, want the transition table's reason for asking")
	}

	if result.Action != ActionWorkerExecuted || result.Worker != WorkerWork || result.ItemNumber != 7 {
		t.Fatalf("result = %+v, want Action=%s Worker=%s ItemNumber=7", result, ActionWorkerExecuted, WorkerWork)
	}
}

func TestRun_DecisiveCandidatesAreNeverPassedToDispatcher(t *testing.T) {
	m := newMockServer(t)
	now := time.Now()
	m.issues = []map[string]any{
		issueFixture(7, "alice", now),
	}

	dispatcher := &fakeDispatcher{}
	executor := &fakeExecutor{}
	result, err := Run(context.Background(), testLogger(), m.client(), dispatcher, executor, "k-wa-wa/pechka", "/var/lib/nuage-autopilot", nil)
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	if result.Action != ActionWorkerExecuted {
		t.Fatalf("result.Action = %q, want %q", result.Action, ActionWorkerExecuted)
	}
	if result.Worker != WorkerWork || result.ItemNumber != 7 {
		t.Fatalf("result = %+v, want Worker=%s ItemNumber=7", result, WorkerWork)
	}

	if len(executor.calls) != 1 {
		t.Fatalf("executor.calls = %d, want 1", len(executor.calls))
	}

	call := executor.calls[0]
	if call.Repo != "k-wa-wa/pechka" || call.Number != 7 || call.Worker != WorkerWork {
		t.Fatalf("executor call = %+v, want repo=k-wa-wa/pechka number=7 worker=%s", call, WorkerWork)
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
	now := time.Now()
	m.issues = []map[string]any{
		issueFixture(1, "k-wa-wa", now),
		issueFixture(2, "untrusted", now),
	}

	dispatcher := &fakeDispatcher{}
	executor := &fakeExecutor{}
	result, err := Run(context.Background(), testLogger(), m.client(), dispatcher, executor, "k-wa-wa/pechka", "/var/lib/nuage-autopilot", []string{"k-wa-wa", "bot-wa-wa"})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if result.ItemNumber != 1 {
		t.Fatalf("result.ItemNumber = %d, want 1 (unallowed author excluded)", result.ItemNumber)
	}
	if len(executor.calls) != 1 {
		t.Fatalf("executor.calls = %+v, want exactly 1 (unallowed author excluded)", executor.calls)
	}
}

func TestRun_DispatcherNoDecisionResultsInNoop(t *testing.T) {
	m := newMockServer(t)
	now := time.Now()
	m.issues = []map[string]any{
		issueFixture(10, "alice", now),
	}
	// ask に分類させるため、状態行 + それより新しい人間コメントを用意する。
	m.comments[10] = []map[string]any{
		botStatusComment(1, WorkerWork, report.StatusDone, "", now.Add(-time.Hour)),
		{"id": 2, "body": "hmm, not sure about this", "user": map[string]string{"login": "alice", "type": "User"}, "created_at": rfc3339(now)},
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
	if len(dispatcher.calls) != 1 {
		t.Fatalf("dispatcher.calls = %+v, want exactly 1 call", dispatcher.calls)
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
	now := time.Now()
	m.issues = []map[string]any{
		issueFixture(11, "alice", now),
	}
	m.comments[11] = []map[string]any{
		botStatusComment(1, WorkerWork, report.StatusDone, "", now.Add(-time.Hour)),
		{"id": 2, "body": "please double check this", "user": map[string]string{"login": "alice", "type": "User"}, "created_at": rfc3339(now)},
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
	now := time.Now()
	m.issues = []map[string]any{
		issueFixture(12, "alice", now),
	}

	dispatcher := &fakeDispatcher{}
	executor := &fakeExecutor{err: errors.New("claude exited with non-zero status")}
	result, err := Run(context.Background(), testLogger(), m.client(), dispatcher, executor, "k-wa-wa/pechka", "/var/lib/nuage-autopilot", nil)
	if err != nil {
		t.Fatalf("Run() error = %v, want nil (worker failure must not fail the cycle)", err)
	}

	if result.Action != ActionWorkerFailed {
		t.Fatalf("result.Action = %q, want %q", result.Action, ActionWorkerFailed)
	}
	if result.Worker != WorkerWork || result.ItemNumber != 12 {
		t.Fatalf("result = %+v, want Worker=%s ItemNumber=12", result, WorkerWork)
	}

	if got := m.mutatingCallCount(); got != 2 {
		t.Fatalf("mutating call count = %d, want 2 (add agent:running + remove agent:running)", got)
	}
}

func TestRun_ProcessesOnlyOneItemPerCycleEvenWithMultipleCandidates(t *testing.T) {
	m := newMockServer(t)
	now := time.Now()
	m.issues = []map[string]any{
		issueFixture(20, "alice", now),
		issueFixture(21, "alice", now.Add(-time.Hour)), // より長く放置されている方を優先する。
	}

	dispatcher := &fakeDispatcher{}
	executor := &fakeExecutor{}
	result, err := Run(context.Background(), testLogger(), m.client(), dispatcher, executor, "k-wa-wa/pechka", "/var/lib/nuage-autopilot", nil)
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if result.ItemNumber != 21 {
		t.Fatalf("result.ItemNumber = %d, want 21 (older updated_at wins among decisive candidates)", result.ItemNumber)
	}
	if len(dispatcher.calls) != 0 {
		t.Fatalf("dispatcher.calls = %+v, want dispatcher not to be called (both candidates are decisive)", dispatcher.calls)
	}
	if len(executor.calls) != 1 {
		t.Fatalf("executor.calls = %+v, want exactly 1 call", executor.calls)
	}
}

func TestSelectDecisiveCandidate_PrefersPullRequestsOverIssues(t *testing.T) {
	now := time.Now()
	cands := []classifiedCandidate{
		{Item: Item{Kind: kindIssue, Number: 1, UpdatedAt: now.Add(-2 * time.Hour)}, Action: WorkerWork},
		{Item: Item{Kind: kindPullRequest, Number: 2, UpdatedAt: now}, Action: WorkerVerify},
	}
	got := selectDecisiveCandidate(cands)
	if got.Item.Number != 2 {
		t.Fatalf("selectDecisiveCandidate() = %+v, want the PR (#2) to win over the older issue", got)
	}
}

func TestSelectDecisiveCandidate_OldestUpdatedAtWinsWithinSameKind(t *testing.T) {
	now := time.Now()
	cands := []classifiedCandidate{
		{Item: Item{Kind: kindIssue, Number: 1, UpdatedAt: now}, Action: WorkerWork},
		{Item: Item{Kind: kindIssue, Number: 2, UpdatedAt: now.Add(-time.Hour)}, Action: WorkerWork},
	}
	got := selectDecisiveCandidate(cands)
	if got.Item.Number != 2 {
		t.Fatalf("selectDecisiveCandidate() = %+v, want the older item (#2) to win", got)
	}
}

func TestIsAllowedAuthor(t *testing.T) {
	if !isAllowedAuthor("anyone", nil) {
		t.Fatalf("isAllowedAuthor with no allowlist must allow everyone")
	}
	if !isAllowedAuthor("K-Wa-Wa", []string{"k-wa-wa"}) {
		t.Fatalf("isAllowedAuthor must be case-insensitive")
	}
	if isAllowedAuthor("untrusted", []string{"k-wa-wa"}) {
		t.Fatalf("isAllowedAuthor must reject authors not in the allowlist")
	}
}
