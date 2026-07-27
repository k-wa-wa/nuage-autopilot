package cycle

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
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

func TestRun_AssignsSpecToUnlabeledIssue(t *testing.T) {
	m := newMockServer(t)
	m.issues = []map[string]any{
		{"number": 1, "title": "do something", "state": "open", "labels": []any{}, "user": map[string]string{"login": "alice", "type": "User"}, "created_at": rfc3339(time.Now()), "updated_at": rfc3339(time.Now())},
	}

	result, err := Run(context.Background(), testLogger(), m.client(), "k-wa-wa/pechka", "/var/lib/nuage-autopilot")
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	if result.Action != ActionAssignSpec {
		t.Fatalf("result.Action = %q, want %q", result.Action, ActionAssignSpec)
	}
	if result.ItemNumber != 1 || result.Label != LabelSpec {
		t.Fatalf("result = %+v, want ItemNumber=1 Label=%s", result, LabelSpec)
	}

	if got := m.mutatingCallCount(); got != 1 {
		t.Fatalf("mutating call count = %d, want 1", got)
	}
	if m.calls[0].Method != http.MethodPost || m.calls[0].Path != "/repos/k-wa-wa/pechka/issues/1/labels" {
		t.Fatalf("call = %+v, want POST .../issues/1/labels", m.calls[0])
	}
}

func TestRun_DoesNotAssignSpecToUnlabeledPullRequest(t *testing.T) {
	m := newMockServer(t)
	m.prs = []map[string]any{
		{"number": 2, "title": "a pr", "state": "open", "labels": []any{}, "user": map[string]string{"login": "alice", "type": "User"}, "created_at": rfc3339(time.Now()), "updated_at": rfc3339(time.Now())},
	}

	result, err := Run(context.Background(), testLogger(), m.client(), "k-wa-wa/pechka", "/var/lib/nuage-autopilot")
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	if result.Action != ActionNoop {
		t.Fatalf("result.Action = %q, want %q (unlabeled PRs must not get agent:spec)", result.Action, ActionNoop)
	}
	if got := m.mutatingCallCount(); got != 0 {
		t.Fatalf("mutating call count = %d, want 0", got)
	}
}

func TestRun_ClearsWaitAfterHumanComment(t *testing.T) {
	m := newMockServer(t)
	m.issues = []map[string]any{
		{"number": 3, "title": "ambiguous request", "state": "open", "labels": []any{map[string]string{"name": "agent:spec"}, map[string]string{"name": "agent:wait"}}, "user": map[string]string{"login": "alice", "type": "User"}, "created_at": rfc3339(time.Now()), "updated_at": rfc3339(time.Now())},
	}
	m.comments[3] = []map[string]any{
		{"id": 1, "body": "what did you mean?", "user": map[string]string{"login": "nuage-autopilot", "type": "User"}, "created_at": rfc3339(time.Now().Add(-time.Hour))},
		{"id": 2, "body": "I meant X", "user": map[string]string{"login": "alice", "type": "User"}, "created_at": rfc3339(time.Now())},
	}

	result, err := Run(context.Background(), testLogger(), m.client(), "k-wa-wa/pechka", "/var/lib/nuage-autopilot")
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	if result.Action != ActionClearWait {
		t.Fatalf("result.Action = %q, want %q", result.Action, ActionClearWait)
	}
	if result.Label != LabelWait {
		t.Fatalf("result.Label = %q, want %q", result.Label, LabelWait)
	}

	if got := m.mutatingCallCount(); got != 1 {
		t.Fatalf("mutating call count = %d, want 1", got)
	}
	want := "/repos/k-wa-wa/pechka/issues/3/labels/agent:wait"
	if m.calls[0].Method != http.MethodDelete || m.calls[0].Path != want {
		t.Fatalf("call = %+v, want DELETE %s", m.calls[0], want)
	}
}

func TestRun_KeepsWaitWhenLatestCommentIsFromBot(t *testing.T) {
	m := newMockServer(t)
	m.issues = []map[string]any{
		{"number": 4, "title": "ambiguous request", "state": "open", "labels": []any{map[string]string{"name": "agent:spec"}, map[string]string{"name": "agent:wait"}}, "user": map[string]string{"login": "alice", "type": "User"}, "created_at": rfc3339(time.Now()), "updated_at": rfc3339(time.Now())},
	}
	m.comments[4] = []map[string]any{
		{"id": 1, "body": "first question", "user": map[string]string{"login": "alice", "type": "User"}, "created_at": rfc3339(time.Now().Add(-time.Hour))},
		{"id": 2, "body": "still unclear, could you clarify?", "user": map[string]string{"login": "nuage-autopilot", "type": "User"}, "created_at": rfc3339(time.Now())},
	}

	result, err := Run(context.Background(), testLogger(), m.client(), "k-wa-wa/pechka", "/var/lib/nuage-autopilot")
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	if result.Action != ActionNoop {
		t.Fatalf("result.Action = %q, want %q (wait should not clear when bot posted last)", result.Action, ActionNoop)
	}
	if got := m.mutatingCallCount(); got != 0 {
		t.Fatalf("mutating call count = %d, want 0", got)
	}
}

func TestRun_KeepsWaitWhenNoComments(t *testing.T) {
	m := newMockServer(t)
	m.issues = []map[string]any{
		{"number": 6, "title": "ambiguous request", "state": "open", "labels": []any{map[string]string{"name": "agent:spec"}, map[string]string{"name": "agent:wait"}}, "user": map[string]string{"login": "alice", "type": "User"}, "created_at": rfc3339(time.Now()), "updated_at": rfc3339(time.Now())},
	}

	result, err := Run(context.Background(), testLogger(), m.client(), "k-wa-wa/pechka", "/var/lib/nuage-autopilot")
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if result.Action != ActionNoop {
		t.Fatalf("result.Action = %q, want %q", result.Action, ActionNoop)
	}
	if got := m.mutatingCallCount(); got != 0 {
		t.Fatalf("mutating call count = %d, want 0", got)
	}
}

func TestRun_EscalatesTimedOutActivePhaseToTriage(t *testing.T) {
	m := newMockServer(t)
	m.issues = []map[string]any{
		{"number": 5, "title": "stuck in dev", "state": "open", "labels": []any{map[string]string{"name": "agent:dev"}}, "user": map[string]string{"login": "alice", "type": "User"}, "created_at": rfc3339(time.Now().Add(-48 * time.Hour)), "updated_at": rfc3339(time.Now().Add(-48 * time.Hour))},
	}

	result, err := Run(context.Background(), testLogger(), m.client(), "k-wa-wa/pechka", "/var/lib/nuage-autopilot")
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	if result.Action != ActionEscalateTriage {
		t.Fatalf("result.Action = %q, want %q", result.Action, ActionEscalateTriage)
	}
	if result.Label != LabelDev {
		t.Fatalf("result.Label = %q, want %q", result.Label, LabelDev)
	}

	if got := m.mutatingCallCount(); got != 2 {
		t.Fatalf("mutating call count = %d, want 2 (remove old label + add agent:triage)", got)
	}
	removeWant := "/repos/k-wa-wa/pechka/issues/5/labels/agent:dev"
	if m.calls[0].Method != http.MethodDelete || m.calls[0].Path != removeWant {
		t.Fatalf("calls[0] = %+v, want DELETE %s", m.calls[0], removeWant)
	}
	if m.calls[1].Method != http.MethodPost || m.calls[1].Path != "/repos/k-wa-wa/pechka/issues/5/labels" {
		t.Fatalf("calls[1] = %+v, want POST .../issues/5/labels", m.calls[1])
	}
}

func TestRun_LogsLLMPhasePendingWithoutActing(t *testing.T) {
	m := newMockServer(t)
	m.issues = []map[string]any{
		{"number": 7, "title": "needs review", "state": "open", "labels": []any{map[string]string{"name": "agent:review-general"}}, "user": map[string]string{"login": "alice", "type": "User"}, "created_at": rfc3339(time.Now()), "updated_at": rfc3339(time.Now())},
	}

	result, err := Run(context.Background(), testLogger(), m.client(), "k-wa-wa/pechka", "/var/lib/nuage-autopilot")
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	if result.Action != ActionLLMPhasePending {
		t.Fatalf("result.Action = %q, want %q", result.Action, ActionLLMPhasePending)
	}
	if result.Label != LabelReviewGeneral || result.ItemNumber != 7 {
		t.Fatalf("result = %+v, want Label=%s ItemNumber=7", result, LabelReviewGeneral)
	}
	if got := m.mutatingCallCount(); got != 0 {
		t.Fatalf("mutating call count = %d, want 0 (phase 2 must not act on LLM phases)", got)
	}
}

func TestRun_SkipsTriageItems(t *testing.T) {
	m := newMockServer(t)
	m.issues = []map[string]any{
		{"number": 8, "title": "escalated", "state": "open", "labels": []any{map[string]string{"name": "agent:triage"}}, "user": map[string]string{"login": "alice", "type": "User"}, "created_at": rfc3339(time.Now()), "updated_at": rfc3339(time.Now())},
	}

	result, err := Run(context.Background(), testLogger(), m.client(), "k-wa-wa/pechka", "/var/lib/nuage-autopilot")
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if result.Action != ActionNoop {
		t.Fatalf("result.Action = %q, want %q", result.Action, ActionNoop)
	}
	if got := m.mutatingCallCount(); got != 0 {
		t.Fatalf("mutating call count = %d, want 0", got)
	}
}

func TestRun_ProcessesOnlyOneItemPerCycle(t *testing.T) {
	m := newMockServer(t)
	m.issues = []map[string]any{
		{"number": 10, "title": "first", "state": "open", "labels": []any{}, "user": map[string]string{"login": "alice", "type": "User"}, "created_at": rfc3339(time.Now()), "updated_at": rfc3339(time.Now())},
		{"number": 11, "title": "second", "state": "open", "labels": []any{}, "user": map[string]string{"login": "alice", "type": "User"}, "created_at": rfc3339(time.Now()), "updated_at": rfc3339(time.Now())},
	}

	result, err := Run(context.Background(), testLogger(), m.client(), "k-wa-wa/pechka", "/var/lib/nuage-autopilot")
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if result.Action != ActionAssignSpec || result.ItemNumber != 10 {
		t.Fatalf("result = %+v, want ActionAssignSpec on issue #10 only", result)
	}
	if got := m.mutatingCallCount(); got != 1 {
		t.Fatalf("mutating call count = %d, want 1 (only one item may be processed per cycle)", got)
	}
}

func TestRun_NoActionWhenRepoHasNothingOpen(t *testing.T) {
	m := newMockServer(t)

	result, err := Run(context.Background(), testLogger(), m.client(), "k-wa-wa/pechka", "/var/lib/nuage-autopilot")
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if result.Action != ActionNoop {
		t.Fatalf("result.Action = %q, want %q", result.Action, ActionNoop)
	}
	if result.Repo != "k-wa-wa/pechka" || result.StateDir != "/var/lib/nuage-autopilot" {
		t.Fatalf("result = %+v, want Repo/StateDir preserved", result)
	}
}

func TestRun_ActivePhaseTimeoutIsConfigurable(t *testing.T) {
	m := newMockServer(t)
	m.issues = []map[string]any{
		{"number": 9, "title": "recently active", "state": "open", "labels": []any{map[string]string{"name": "agent:qa"}}, "user": map[string]string{"login": "alice", "type": "User"}, "created_at": rfc3339(time.Now().Add(-2 * time.Minute)), "updated_at": rfc3339(time.Now().Add(-2 * time.Minute))},
	}

	result, err := Run(context.Background(), testLogger(), m.client(), "k-wa-wa/pechka", "/var/lib/nuage-autopilot",
		WithActivePhaseTimeout(time.Minute))
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	if result.Action != ActionEscalateTriage {
		t.Fatalf("result.Action = %q, want %q (2m elapsed > 1m configured timeout)", result.Action, ActionEscalateTriage)
	}
}
