package engine

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"autopilot/internal/github"
	"autopilot/internal/repo"
	"autopilot/internal/store"
)

// このテストファイルは実際の GitHub API・実際の claude を一切使わない。
// GitHub は httptest.Server、claude はフェイクスクリプト、git のリモートは
// ローカルの bare リポジトリに差し替える。

func requireGit(t *testing.T) string {
	t.Helper()
	path, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git not found in PATH, skipping engine package tests")
	}
	return path
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

func runOrFatal(t *testing.T, gitBin, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command(gitBin, args...)
	cmd.Dir = dir
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	if err := cmd.Run(); err != nil {
		t.Fatalf("git %s (dir=%s) failed: %v\n%s", strings.Join(args, " "), dir, err, out.String())
	}
}

// newBareRemote は初期コミットを1つ持つ bare リポジトリを作り、その絶対パスを返す。
func newBareRemote(t *testing.T, gitBin string) string {
	t.Helper()
	root := t.TempDir()
	bareDir := filepath.Join(root, "remote.git")
	runOrFatal(t, gitBin, root, "init", "--bare", "-q", bareDir)

	workDir := filepath.Join(root, "seed-work")
	runOrFatal(t, gitBin, root, "clone", "-q", bareDir, workDir)
	runOrFatal(t, gitBin, workDir, "config", "user.email", "seed@example.invalid")
	runOrFatal(t, gitBin, workDir, "config", "user.name", "seed")
	if err := os.WriteFile(filepath.Join(workDir, "README.md"), []byte("v1\n"), 0o644); err != nil {
		t.Fatalf("write seed file: %v", err)
	}
	runOrFatal(t, gitBin, workDir, "add", "README.md")
	runOrFatal(t, gitBin, workDir, "commit", "-q", "-m", "initial commit")
	runOrFatal(t, gitBin, workDir, "push", "-q", "origin", "HEAD")

	return bareDir
}

// writeFakeGH は `gh auth setup-git` 等の呼び出しに対して何もせず成功するだけの
// フェイク gh 実行ファイルを書き出す。
func writeFakeGH(t *testing.T) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("fake gh script assumes a POSIX shell")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "gh")
	if err := os.WriteFile(path, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("write fake gh script: %v", err)
	}
	return path
}

// writeFakeClaude は NUAGE_REPORT_FILE に reportBody をそのまま書き出し、
// 標準出力に session_id/total_cost_usd を含む JSON を 1 行返すフェイク claude
// 実行ファイルを書き出す。reportBody が空文字列の場合、report ファイルは
// 書き出さない（無言終了を模す）。
func writeFakeClaude(t *testing.T, reportBody, sessionID string, costUSD float64) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("fake claude script assumes a POSIX shell")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "claude")

	var writeReport string
	if reportBody != "" {
		writeReport = fmt.Sprintf("cat > \"$NUAGE_REPORT_FILE\" <<'NUAGE_EOF'\n%s\nNUAGE_EOF\n", reportBody)
	}
	script := "#!/bin/sh\ncat > /dev/null\n" + writeReport +
		fmt.Sprintf("echo '{\"session_id\": %q, \"total_cost_usd\": %g}'\n", sessionID, costUSD)

	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake claude script: %v", err)
	}
	return path
}

func testLogger(buf *bytes.Buffer) *slog.Logger {
	return slog.New(slog.NewTextHandler(buf, nil))
}

type testHarness struct {
	engine *Engine
	store  *store.Store
	client *github.Client
}

// newTestEngine は Engine とその依存を一式組み立てる。githubHandler は k-wa-wa/pechka
// リポジトリ向けの GitHub API 応答を提供する。claudePath はフェイク claude 実行ファイルの
// パス（writeFakeClaude で作る）。
func newTestEngine(t *testing.T, githubHandler http.HandlerFunc, claudePath string) *testHarness {
	t.Helper()

	gitBin := requireGit(t)
	ghPath := writeFakeGH(t)
	remote := newBareRemote(t, gitBin)

	server := httptest.NewServer(githubHandler)
	t.Cleanup(server.Close)
	client := github.NewClient(github.WithBaseURL(server.URL), github.WithStaticToken("test-token"))

	st, err := store.Open(context.Background(), ":memory:")
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	e := New(Config{
		Store:        st,
		Client:       client,
		StateDir:     t.TempDir(),
		Repos:        []string{"k-wa-wa/pechka"},
		AgentCommand: claudePath,
		LeaseTTL:     time.Minute,
		AgentTimeout: 10 * time.Second,
		RepoOptions: []repo.Option{
			repo.WithRemoteURL(remote),
			repo.WithGitCommand(gitBin),
			repo.WithGHCommand(ghPath),
		},
	})

	return &testHarness{engine: e, store: st, client: client}
}

func writeJSON(w http.ResponseWriter, body string) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(body))
}

func issueDetailHandler(t *testing.T) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/k-wa-wa/pechka/issues/1":
			writeJSON(w, `{"number": 1, "title": "add dark mode", "body": "please add it", "state": "open", "user": {"login": "alice", "type": "User"}}`)
		default:
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
	}
}

// TestProcessNext_NoUnprocessedEvents は未処理イベントが無ければ何もせず
// processed=false を返すことを検証する。
func TestProcessNext_NoUnprocessedEvents(t *testing.T) {
	h := newTestEngine(t, func(w http.ResponseWriter, r *http.Request) {
		t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
	}, "")

	processed, err := h.engine.ProcessNext(context.Background())
	if err != nil {
		t.Fatalf("ProcessNext() error = %v", err)
	}
	if processed {
		t.Fatalf("processed = true, want false")
	}
}

// TestProcessNext_NewIssueImplemented は phase=new の Issue が opened イベントで
// 新規セッションのエージェントを起動し、outcome="implemented" を in_review へ
// 反映すること、コストとセッション ID が記録されることを検証する。
func TestProcessNext_NewIssueImplemented(t *testing.T) {
	claude := writeFakeClaude(t, `{"outcome": "implemented", "children": []}`, "sess-abc", 0.42)
	h := newTestEngine(t, issueDetailHandler(t), claude)
	ctx := context.Background()

	item, _, err := h.store.UpsertItem(ctx, "k-wa-wa/pechka", 1, store.KindIssue)
	if err != nil {
		t.Fatalf("UpsertItem: %v", err)
	}
	if _, _, err := h.store.EnqueueEvent(ctx, "opened:1", item.ID, "opened", "alice", "please add it", time.Now()); err != nil {
		t.Fatalf("EnqueueEvent: %v", err)
	}

	processed, err := h.engine.ProcessNext(ctx)
	if err != nil {
		t.Fatalf("ProcessNext() error = %v", err)
	}
	if !processed {
		t.Fatalf("processed = false, want true")
	}

	reloaded, ok, err := h.store.GetItemByID(ctx, item.ID)
	if err != nil || !ok {
		t.Fatalf("GetItemByID: ok=%v err=%v", ok, err)
	}
	if reloaded.Phase != store.PhaseInReview {
		t.Fatalf("Phase = %q, want %q", reloaded.Phase, store.PhaseInReview)
	}
	if reloaded.SessionID != "sess-abc" {
		t.Fatalf("SessionID = %q, want sess-abc", reloaded.SessionID)
	}
	if reloaded.CostUSD != 0.42 {
		t.Fatalf("CostUSD = %v, want 0.42", reloaded.CostUSD)
	}
	if reloaded.Runs != 1 {
		t.Fatalf("Runs = %d, want 1", reloaded.Runs)
	}

	if n, err := h.store.CountUnprocessedEvents(ctx); err != nil || n != 0 {
		t.Fatalf("CountUnprocessedEvents = %d, err=%v, want 0", n, err)
	}

	// リースは解放されている。
	if _, ok, err := h.store.GetLease(ctx, item.ID); err != nil || ok {
		t.Fatalf("lease should be released after a successful run: ok=%v err=%v", ok, err)
	}
}

// TestProcessNext_ResumeUsesResumeFlag は既存セッションを持つアイテムに対する
// 起動が --resume <session_id> を claude に渡すことを検証する。
//
// stdout は --output-format json の応答専用（1 行の JSON のみ）にする必要が
// あるため、受け取った引数はログファイルへ別途書き出す（stdout に混ぜると
// JSON のパースが壊れる）。
func TestProcessNext_ResumeUsesResumeFlag(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fake claude script assumes a POSIX shell")
	}
	dir := t.TempDir()
	claudePath := filepath.Join(dir, "claude")
	argLog := filepath.Join(dir, "args.log")
	script := "#!/bin/sh\ncat > /dev/null\nfor a in \"$@\"; do echo \"$a\" >> " + shellQuote(argLog) + "; done\n" +
		"cat > \"$NUAGE_REPORT_FILE\" <<'EOF'\n{\"outcome\": \"idle\", \"children\": []}\nEOF\n" +
		"echo '{\"session_id\": \"sess-continued\", \"total_cost_usd\": 0.01}'\n"
	if err := os.WriteFile(claudePath, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake claude: %v", err)
	}

	h := newTestEngine(t, issueDetailHandler(t), claudePath)
	ctx := context.Background()

	item, _, err := h.store.UpsertItem(ctx, "k-wa-wa/pechka", 1, store.KindIssue)
	if err != nil {
		t.Fatalf("UpsertItem: %v", err)
	}
	if err := h.store.UpdateItemPhase(ctx, item.ID, store.PhaseAwaitingAnswer); err != nil {
		t.Fatalf("UpdateItemPhase: %v", err)
	}
	if err := h.store.UpdateItemSessionID(ctx, item.ID, "sess-original"); err != nil {
		t.Fatalf("UpdateItemSessionID: %v", err)
	}
	if _, _, err := h.store.EnqueueEvent(ctx, "commented:1", item.ID, "commented", "alice", "here's my answer", time.Now()); err != nil {
		t.Fatalf("EnqueueEvent: %v", err)
	}

	if _, err := h.engine.ProcessNext(ctx); err != nil {
		t.Fatalf("ProcessNext() error = %v", err)
	}

	argBytes, err := os.ReadFile(argLog)
	if err != nil {
		t.Fatalf("read arg log: %v", err)
	}
	args := string(argBytes)
	if !strings.Contains(args, "--resume") || !strings.Contains(args, "sess-original") {
		t.Fatalf("args = %q, want it to contain --resume sess-original", args)
	}

	reloaded, ok, err := h.store.GetItemByID(ctx, item.ID)
	if err != nil || !ok {
		t.Fatalf("GetItemByID: ok=%v err=%v", ok, err)
	}
	if reloaded.SessionID != "sess-continued" {
		t.Fatalf("SessionID = %q, want sess-continued", reloaded.SessionID)
	}
}

// TestProcessNext_Asked は outcome="asked" が awaiting_answer に遷移することを検証する。
func TestProcessNext_Asked(t *testing.T) {
	claude := writeFakeClaude(t, `{"outcome": "asked", "children": []}`, "sess-1", 0.1)
	h := newTestEngine(t, issueDetailHandler(t), claude)
	ctx := context.Background()

	item, _, err := h.store.UpsertItem(ctx, "k-wa-wa/pechka", 1, store.KindIssue)
	if err != nil {
		t.Fatalf("UpsertItem: %v", err)
	}
	if _, _, err := h.store.EnqueueEvent(ctx, "opened:1", item.ID, "opened", "alice", "vague request", time.Now()); err != nil {
		t.Fatalf("EnqueueEvent: %v", err)
	}

	if _, err := h.engine.ProcessNext(ctx); err != nil {
		t.Fatalf("ProcessNext() error = %v", err)
	}

	reloaded, ok, err := h.store.GetItemByID(ctx, item.ID)
	if err != nil || !ok {
		t.Fatalf("GetItemByID: ok=%v err=%v", ok, err)
	}
	if reloaded.Phase != store.PhaseAwaitingAnswer {
		t.Fatalf("Phase = %q, want %q", reloaded.Phase, store.PhaseAwaitingAnswer)
	}
}

// TestProcessNext_Blocked は outcome="blocked" が blocked に遷移することを検証する。
func TestProcessNext_Blocked(t *testing.T) {
	claude := writeFakeClaude(t, `{"outcome": "blocked", "children": []}`, "sess-1", 0.1)
	h := newTestEngine(t, issueDetailHandler(t), claude)
	ctx := context.Background()

	item, _, err := h.store.UpsertItem(ctx, "k-wa-wa/pechka", 1, store.KindIssue)
	if err != nil {
		t.Fatalf("UpsertItem: %v", err)
	}
	if _, _, err := h.store.EnqueueEvent(ctx, "opened:1", item.ID, "opened", "alice", "x", time.Now()); err != nil {
		t.Fatalf("EnqueueEvent: %v", err)
	}

	if _, err := h.engine.ProcessNext(ctx); err != nil {
		t.Fatalf("ProcessNext() error = %v", err)
	}

	reloaded, ok, err := h.store.GetItemByID(ctx, item.ID)
	if err != nil || !ok {
		t.Fatalf("GetItemByID: ok=%v err=%v", ok, err)
	}
	if reloaded.Phase != store.PhaseBlocked {
		t.Fatalf("Phase = %q, want %q", reloaded.Phase, store.PhaseBlocked)
	}
}

// TestProcessNext_Idle は outcome="idle" が phase を変更しないことを検証する。
func TestProcessNext_Idle(t *testing.T) {
	claude := writeFakeClaude(t, `{"outcome": "idle", "children": []}`, "sess-1", 0.1)
	h := newTestEngine(t, issueDetailHandler(t), claude)
	ctx := context.Background()

	item, _, err := h.store.UpsertItem(ctx, "k-wa-wa/pechka", 1, store.KindIssue)
	if err != nil {
		t.Fatalf("UpsertItem: %v", err)
	}
	if err := h.store.UpdateItemPhase(ctx, item.ID, store.PhaseReady); err != nil {
		t.Fatalf("UpdateItemPhase: %v", err)
	}
	if _, _, err := h.store.EnqueueEvent(ctx, "commented:1", item.ID, "commented", "alice", "lgtm", time.Now()); err != nil {
		t.Fatalf("EnqueueEvent: %v", err)
	}

	if _, err := h.engine.ProcessNext(ctx); err != nil {
		t.Fatalf("ProcessNext() error = %v", err)
	}

	reloaded, ok, err := h.store.GetItemByID(ctx, item.ID)
	if err != nil || !ok {
		t.Fatalf("GetItemByID: ok=%v err=%v", ok, err)
	}
	if reloaded.Phase != store.PhaseReady {
		t.Fatalf("Phase = %q, want unchanged %q", reloaded.Phase, store.PhaseReady)
	}
}

// TestProcessNext_Split はサブ Issue を登録し、親を delegated にすることを検証する
// （DESIGN.md 9章）。
func TestProcessNext_Split(t *testing.T) {
	claude := writeFakeClaude(t, `{"outcome": "split", "children": [10, 11]}`, "sess-1", 0.1)
	h := newTestEngine(t, issueDetailHandler(t), claude)
	ctx := context.Background()

	item, _, err := h.store.UpsertItem(ctx, "k-wa-wa/pechka", 1, store.KindIssue)
	if err != nil {
		t.Fatalf("UpsertItem: %v", err)
	}
	if _, _, err := h.store.EnqueueEvent(ctx, "opened:1", item.ID, "opened", "alice", "huge request", time.Now()); err != nil {
		t.Fatalf("EnqueueEvent: %v", err)
	}

	if _, err := h.engine.ProcessNext(ctx); err != nil {
		t.Fatalf("ProcessNext() error = %v", err)
	}

	parent, ok, err := h.store.GetItemByID(ctx, item.ID)
	if err != nil || !ok {
		t.Fatalf("GetItemByID: ok=%v err=%v", ok, err)
	}
	if parent.Phase != store.PhaseDelegated {
		t.Fatalf("parent.Phase = %q, want %q", parent.Phase, store.PhaseDelegated)
	}

	children, err := h.store.ListChildren(ctx, parent.ID)
	if err != nil {
		t.Fatalf("ListChildren: %v", err)
	}
	if len(children) != 2 {
		t.Fatalf("len(children) = %d, want 2", len(children))
	}
	gotNumbers := map[int]bool{}
	for _, c := range children {
		gotNumbers[c.Number] = true
		if c.Phase != store.PhaseNew {
			t.Fatalf("child #%d phase = %q, want %q", c.Number, c.Phase, store.PhaseNew)
		}
	}
	if !gotNumbers[10] || !gotNumbers[11] {
		t.Fatalf("children numbers = %v, want {10, 11}", gotNumbers)
	}
}

// TestProcessNext_ChildDoneWakesParentOnlyWhenAllChildrenAreDone は、全子が
// done になった時点でのみ親に child_done イベントが積まれ、親のエージェントが
// resume されることを検証する（DESIGN.md 9章）。
func TestProcessNext_ChildDoneWakesParentOnlyWhenAllChildrenAreDone(t *testing.T) {
	claude := writeFakeClaude(t, `{"outcome": "implemented", "children": []}`, "sess-parent-2", 0.05)
	h := newTestEngine(t, issueDetailHandler(t), claude)
	ctx := context.Background()

	parent, _, err := h.store.UpsertItem(ctx, "k-wa-wa/pechka", 1, store.KindIssue)
	if err != nil {
		t.Fatalf("UpsertItem parent: %v", err)
	}
	if err := h.store.UpdateItemPhase(ctx, parent.ID, store.PhaseDelegated); err != nil {
		t.Fatalf("UpdateItemPhase parent: %v", err)
	}
	if err := h.store.UpdateItemSessionID(ctx, parent.ID, "sess-parent-1"); err != nil {
		t.Fatalf("UpdateItemSessionID parent: %v", err)
	}

	child1, _, err := h.store.UpsertItem(ctx, "k-wa-wa/pechka", 10, store.KindIssue)
	if err != nil {
		t.Fatalf("UpsertItem child1: %v", err)
	}
	if err := h.store.SetItemParent(ctx, child1.ID, parent.ID); err != nil {
		t.Fatalf("SetItemParent child1: %v", err)
	}
	child2, _, err := h.store.UpsertItem(ctx, "k-wa-wa/pechka", 11, store.KindIssue)
	if err != nil {
		t.Fatalf("UpsertItem child2: %v", err)
	}
	if err := h.store.SetItemParent(ctx, child2.ID, parent.ID); err != nil {
		t.Fatalf("SetItemParent child2: %v", err)
	}

	// child1 が closed される。まだ child2 が残っているので親は起こされない。
	if _, _, err := h.store.EnqueueEvent(ctx, "closed:10", child1.ID, "closed", "alice", "", time.Now()); err != nil {
		t.Fatalf("EnqueueEvent closed child1: %v", err)
	}
	if _, err := h.engine.ProcessNext(ctx); err != nil {
		t.Fatalf("ProcessNext() (child1 closed) error = %v", err)
	}
	if n, err := h.store.CountUnprocessedEvents(ctx); err != nil || n != 0 {
		t.Fatalf("CountUnprocessedEvents after child1 alone = %d, err=%v, want 0 (parent must not wake yet)", n, err)
	}
	reloadedParent, ok, err := h.store.GetItemByID(ctx, parent.ID)
	if err != nil || !ok {
		t.Fatalf("GetItemByID parent: ok=%v err=%v", ok, err)
	}
	if reloadedParent.Phase != store.PhaseDelegated {
		t.Fatalf("parent.Phase = %q, want still %q", reloadedParent.Phase, store.PhaseDelegated)
	}

	// child2 も closed される。これで全子が done になるので親に child_done が積まれる。
	if _, _, err := h.store.EnqueueEvent(ctx, "closed:11", child2.ID, "closed", "alice", "", time.Now()); err != nil {
		t.Fatalf("EnqueueEvent closed child2: %v", err)
	}
	if _, err := h.engine.ProcessNext(ctx); err != nil {
		t.Fatalf("ProcessNext() (child2 closed) error = %v", err)
	}

	ev, ok, err := h.store.NextUnprocessedEvent(ctx)
	if err != nil || !ok {
		t.Fatalf("NextUnprocessedEvent: ok=%v err=%v (want a child_done event for the parent)", ok, err)
	}
	if ev.Type != "child_done" || ev.ItemID != parent.ID {
		t.Fatalf("event = %+v, want type=child_done item_id=%d", ev, parent.ID)
	}

	// 親のエージェントが resume される。
	if _, err := h.engine.ProcessNext(ctx); err != nil {
		t.Fatalf("ProcessNext() (parent child_done) error = %v", err)
	}
	reloadedParent, ok, err = h.store.GetItemByID(ctx, parent.ID)
	if err != nil || !ok {
		t.Fatalf("GetItemByID parent (after resume): ok=%v err=%v", ok, err)
	}
	if reloadedParent.Phase != store.PhaseInReview {
		t.Fatalf("parent.Phase = %q, want %q (outcome=implemented)", reloadedParent.Phase, store.PhaseInReview)
	}
	if reloadedParent.SessionID != "sess-parent-2" {
		t.Fatalf("parent.SessionID = %q, want sess-parent-2 (must have used --resume then updated)", reloadedParent.SessionID)
	}
}

// TestProcessNext_ClosedEventNeverLaunchesAgent は closed/merged イベントに
// 対してエージェントを一切起動せず、直接 done に遷移することを検証する。
func TestProcessNext_ClosedEventNeverLaunchesAgent(t *testing.T) {
	h := newTestEngine(t, func(w http.ResponseWriter, r *http.Request) {
		t.Fatalf("unexpected GitHub request (agent should not be launched): %s %s", r.Method, r.URL.Path)
	}, "/nonexistent/claude-must-not-be-invoked")
	ctx := context.Background()

	item, _, err := h.store.UpsertItem(ctx, "k-wa-wa/pechka", 1, store.KindPullRequest)
	if err != nil {
		t.Fatalf("UpsertItem: %v", err)
	}
	if err := h.store.UpdateItemPhase(ctx, item.ID, store.PhaseReady); err != nil {
		t.Fatalf("UpdateItemPhase: %v", err)
	}
	if _, _, err := h.store.EnqueueEvent(ctx, "merged:1", item.ID, "merged", "alice", "", time.Now()); err != nil {
		t.Fatalf("EnqueueEvent: %v", err)
	}

	if _, err := h.engine.ProcessNext(ctx); err != nil {
		t.Fatalf("ProcessNext() error = %v", err)
	}

	reloaded, ok, err := h.store.GetItemByID(ctx, item.ID)
	if err != nil || !ok {
		t.Fatalf("GetItemByID: ok=%v err=%v", ok, err)
	}
	if reloaded.Phase != store.PhaseDone {
		t.Fatalf("Phase = %q, want %q", reloaded.Phase, store.PhaseDone)
	}
}

// TestProcessNext_CiSuccessMovesToReadyWithoutLaunchingAgent は in_review+ci_success
// がエージェントを起動せず ready に遷移するだけであることを検証する
// （DESIGN.md 8.4 節: verify はまだ実装しない）。
func TestProcessNext_CiSuccessMovesToReadyWithoutLaunchingAgent(t *testing.T) {
	h := newTestEngine(t, func(w http.ResponseWriter, r *http.Request) {
		t.Fatalf("unexpected GitHub request (agent should not be launched): %s %s", r.Method, r.URL.Path)
	}, "/nonexistent/claude-must-not-be-invoked")
	ctx := context.Background()

	item, _, err := h.store.UpsertItem(ctx, "k-wa-wa/pechka", 1, store.KindPullRequest)
	if err != nil {
		t.Fatalf("UpsertItem: %v", err)
	}
	if err := h.store.UpdateItemPhase(ctx, item.ID, store.PhaseInReview); err != nil {
		t.Fatalf("UpdateItemPhase: %v", err)
	}
	if _, _, err := h.store.EnqueueEvent(ctx, "checkrun:1", item.ID, "ci_success", "github", "", time.Now()); err != nil {
		t.Fatalf("EnqueueEvent: %v", err)
	}

	if _, err := h.engine.ProcessNext(ctx); err != nil {
		t.Fatalf("ProcessNext() error = %v", err)
	}

	reloaded, ok, err := h.store.GetItemByID(ctx, item.ID)
	if err != nil || !ok {
		t.Fatalf("GetItemByID: ok=%v err=%v", ok, err)
	}
	if reloaded.Phase != store.PhaseReady {
		t.Fatalf("Phase = %q, want %q", reloaded.Phase, store.PhaseReady)
	}
}

// TestProcessNext_BudgetExceededBlocksWithoutLaunchingAgent は予算上限に達した
// アイテムがエージェントを起動せず blocked になり、GitHub にコメントが
// 投稿されることを検証する（DESIGN.md 10章）。
func TestProcessNext_BudgetExceededBlocksWithoutLaunchingAgent(t *testing.T) {
	var commentBody string
	h := newTestEngine(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/repos/k-wa-wa/pechka/issues/1/comments":
			body := make([]byte, r.ContentLength)
			_, _ = r.Body.Read(body)
			commentBody = string(body)
			w.WriteHeader(http.StatusCreated)
			writeJSON(w, `{}`)
		default:
			t.Fatalf("unexpected GitHub request (agent should not be launched): %s %s", r.Method, r.URL.Path)
		}
	}, "/nonexistent/claude-must-not-be-invoked")
	ctx := context.Background()
	h.engine.cfg.MaxRuns = 1

	item, _, err := h.store.UpsertItem(ctx, "k-wa-wa/pechka", 1, store.KindIssue)
	if err != nil {
		t.Fatalf("UpsertItem: %v", err)
	}
	if err := h.store.UpdateItemPhase(ctx, item.ID, store.PhaseInReview); err != nil {
		t.Fatalf("UpdateItemPhase: %v", err)
	}
	if err := h.store.AddItemUsage(ctx, item.ID, 1.0); err != nil {
		t.Fatalf("AddItemUsage (seed 1 run already used): %v", err)
	}
	// ci_failure（人間由来ではない）なので予算はリセットされない。
	if _, _, err := h.store.EnqueueEvent(ctx, "checkrun:1", item.ID, "ci_failure", "github", "", time.Now()); err != nil {
		t.Fatalf("EnqueueEvent: %v", err)
	}

	if _, err := h.engine.ProcessNext(ctx); err != nil {
		t.Fatalf("ProcessNext() error = %v", err)
	}

	reloaded, ok, err := h.store.GetItemByID(ctx, item.ID)
	if err != nil || !ok {
		t.Fatalf("GetItemByID: ok=%v err=%v", ok, err)
	}
	if reloaded.Phase != store.PhaseBlocked {
		t.Fatalf("Phase = %q, want %q", reloaded.Phase, store.PhaseBlocked)
	}
	if !strings.Contains(commentBody, "予算上限") {
		t.Fatalf("comment body = %q, want it to mention the budget limit", commentBody)
	}
}

// TestProcessNext_HumanCommentResetsBudgetBeforeLaunching は、予算を使い切って
// いても人間のコメントがあればリセットされ、エージェントが起動することを検証する
// （DESIGN.md 10章: 人間の関与が唯一の脱出口である）。
func TestProcessNext_HumanCommentResetsBudgetBeforeLaunching(t *testing.T) {
	claude := writeFakeClaude(t, `{"outcome": "implemented", "children": []}`, "sess-1", 0.01)
	h := newTestEngine(t, issueDetailHandler(t), claude)
	ctx := context.Background()
	h.engine.cfg.MaxRuns = 1

	item, _, err := h.store.UpsertItem(ctx, "k-wa-wa/pechka", 1, store.KindIssue)
	if err != nil {
		t.Fatalf("UpsertItem: %v", err)
	}
	if err := h.store.UpdateItemPhase(ctx, item.ID, store.PhaseAwaitingAnswer); err != nil {
		t.Fatalf("UpdateItemPhase: %v", err)
	}
	if err := h.store.AddItemUsage(ctx, item.ID, 1.0); err != nil {
		t.Fatalf("AddItemUsage (seed at limit): %v", err)
	}
	if _, _, err := h.store.EnqueueEvent(ctx, "commented:1", item.ID, "commented", "alice", "here's more info", time.Now()); err != nil {
		t.Fatalf("EnqueueEvent: %v", err)
	}

	if _, err := h.engine.ProcessNext(ctx); err != nil {
		t.Fatalf("ProcessNext() error = %v", err)
	}

	reloaded, ok, err := h.store.GetItemByID(ctx, item.ID)
	if err != nil || !ok {
		t.Fatalf("GetItemByID: ok=%v err=%v", ok, err)
	}
	if reloaded.Phase != store.PhaseInReview {
		t.Fatalf("Phase = %q, want %q (agent should have launched after budget reset)", reloaded.Phase, store.PhaseInReview)
	}
	// リセット後に 1 回分だけ使った状態になっているはず。
	if reloaded.Runs != 1 {
		t.Fatalf("Runs = %d, want 1 (reset then incremented once by this run)", reloaded.Runs)
	}
}

// TestProcessNext_MissingReportFileFallsBackToBlocked は、エージェントが
// NUAGE_REPORT_FILE を書かずに終了した場合、Go が blocked コメントを投稿して
// phase=blocked にすることを検証する（DESIGN.md 8.3 節: 無言終了への保険）。
func TestProcessNext_MissingReportFileFallsBackToBlocked(t *testing.T) {
	claude := writeFakeClaude(t, "", "sess-1", 0.01) // reportBody="" → report ファイルを書かない
	var commentPosted bool
	h := newTestEngine(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/repos/k-wa-wa/pechka/issues/1":
			writeJSON(w, `{"number": 1, "title": "t", "body": "b", "state": "open", "user": {"login": "alice", "type": "User"}}`)
		case r.Method == http.MethodPost && r.URL.Path == "/repos/k-wa-wa/pechka/issues/1/comments":
			commentPosted = true
			w.WriteHeader(http.StatusCreated)
			writeJSON(w, `{}`)
		default:
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
	}, claude)
	ctx := context.Background()

	item, _, err := h.store.UpsertItem(ctx, "k-wa-wa/pechka", 1, store.KindIssue)
	if err != nil {
		t.Fatalf("UpsertItem: %v", err)
	}
	if _, _, err := h.store.EnqueueEvent(ctx, "opened:1", item.ID, "opened", "alice", "x", time.Now()); err != nil {
		t.Fatalf("EnqueueEvent: %v", err)
	}

	if _, err := h.engine.ProcessNext(ctx); err != nil {
		t.Fatalf("ProcessNext() error = %v", err)
	}

	if !commentPosted {
		t.Fatalf("expected a fallback blocked comment to be posted")
	}
	reloaded, ok, err := h.store.GetItemByID(ctx, item.ID)
	if err != nil || !ok {
		t.Fatalf("GetItemByID: ok=%v err=%v", ok, err)
	}
	if reloaded.Phase != store.PhaseBlocked {
		t.Fatalf("Phase = %q, want %q", reloaded.Phase, store.PhaseBlocked)
	}
	// 無言終了でも実行分のコストは計上される。
	if reloaded.Runs != 1 {
		t.Fatalf("Runs = %d, want 1", reloaded.Runs)
	}
}
