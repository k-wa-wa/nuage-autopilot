package cycle

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"autopilot/internal/github"
	"autopilot/internal/prompt"
	"autopilot/internal/report"
	"autopilot/internal/runner"
)

// このファイルは DefaultLLMExecutor.Execute の中身のうち、EnsureWorkspace（実際の
// git/gh を要求する）と runner.Run（実際の claude を起動する）を除いた「結果の
// 確定」部分（finish/reportBlocked/refreshHeadSHA）を直接呼んで検証する。
// Execute 自体の end-to-end 検証は、DefaultLLMExecutor が repo.EnsureWorkspace に
// フェイクの git/gh を注入する経路を持たないため、既存の internal/repo と同様に
// ここでは対象としない。

func TestExtractPromptContext_FindsLatestVerifyFailureAndNewerHumanComments(t *testing.T) {
	now := time.Now()
	comments := []github.Comment{
		{Body: report.Render(WorkerVerify, report.StatusFailed, "sha1", "internal/foo.go:12 の nil チェック漏れ"), User: github.Author{Login: testBotLogin, Type: "User"}, CreatedAt: now.Add(-2 * time.Hour)},
		{Body: "あと、ここも直してほしい", User: github.Author{Login: "alice", Type: "User"}, CreatedAt: now.Add(-time.Hour)},
		{Body: "ついでにこっちも", User: github.Author{Login: "bob", Type: "User"}, CreatedAt: now},
	}

	verifyFailureSummary, humanComments := extractPromptContext(comments, testBotLogin)

	if verifyFailureSummary != "internal/foo.go:12 の nil チェック漏れ" {
		t.Fatalf("verifyFailureSummary = %q, want the verify/failed comment's prose", verifyFailureSummary)
	}
	if len(humanComments) != 2 {
		t.Fatalf("humanComments = %+v, want 2 (both newer than the status line)", humanComments)
	}
	// 古い順で返る。
	if humanComments[0].Author != "alice" || humanComments[1].Author != "bob" {
		t.Fatalf("humanComments = %+v, want [alice, bob] in chronological order", humanComments)
	}
}

func TestExtractPromptContext_NoStatusLineTreatsAllCommentsAsNew(t *testing.T) {
	now := time.Now()
	comments := []github.Comment{
		{Body: "最初のコメント", User: github.Author{Login: "alice", Type: "User"}, CreatedAt: now.Add(-time.Hour)},
		{Body: "追加のコメント", User: github.Author{Login: "bob", Type: "User"}, CreatedAt: now},
	}

	verifyFailureSummary, humanComments := extractPromptContext(comments, testBotLogin)
	if verifyFailureSummary != "" {
		t.Fatalf("verifyFailureSummary = %q, want empty (no verify/failed status line)", verifyFailureSummary)
	}
	if len(humanComments) != 2 {
		t.Fatalf("humanComments = %+v, want both comments included", humanComments)
	}
}

func TestExtractPromptContext_HumanCommentsBeforeStatusLineAreExcluded(t *testing.T) {
	now := time.Now()
	comments := []github.Comment{
		{Body: "古い要望", User: github.Author{Login: "alice", Type: "User"}, CreatedAt: now.Add(-2 * time.Hour)},
		{Body: report.Render(WorkerWork, report.StatusDone, "", ""), User: github.Author{Login: testBotLogin, Type: "User"}, CreatedAt: now.Add(-time.Hour)},
	}
	_, humanComments := extractPromptContext(comments, testBotLogin)
	if len(humanComments) != 0 {
		t.Fatalf("humanComments = %+v, want empty (the human comment predates the status line)", humanComments)
	}
}

func TestStripStatusLine(t *testing.T) {
	body := "<!-- nuage-autopilot worker=work status=done -->\n実装した。\n動作確認済み。"
	if got, want := stripStatusLine(body), "実装した。\n動作確認済み。"; got != want {
		t.Fatalf("stripStatusLine() = %q, want %q", got, want)
	}
	if got := stripStatusLine("状態行が無い1行だけの本文"); got != "" {
		t.Fatalf("stripStatusLine() = %q, want empty when there is no second line", got)
	}
}

func TestBuildPromptForWorker(t *testing.T) {
	issueCtx := prompt.Context{Kind: prompt.KindIssue, Number: 1}
	if _, err := buildPromptForWorker(issueCtx, WorkerWork); err != nil {
		t.Fatalf("buildPromptForWorker(work, issue) error = %v", err)
	}

	prCtx := prompt.Context{Kind: prompt.KindPullRequest, Number: 1}
	if _, err := buildPromptForWorker(prCtx, WorkerWork); err != nil {
		t.Fatalf("buildPromptForWorker(work, pr) error = %v", err)
	}
	if _, err := buildPromptForWorker(prCtx, WorkerVerify); err != nil {
		t.Fatalf("buildPromptForWorker(verify, pr) error = %v", err)
	}

	if _, err := buildPromptForWorker(issueCtx, WorkerVerify); err == nil {
		t.Fatalf("buildPromptForWorker(verify, issue) error = nil, want an error (verify is pr-only)")
	}
	if _, err := buildPromptForWorker(issueCtx, "bogus"); err == nil {
		t.Fatalf("buildPromptForWorker(bogus, issue) error = nil, want an error for an unknown worker")
	}
}

func TestRefreshHeadSHA_IssueReturnsEmpty(t *testing.T) {
	m := newMockServer(t)
	e := &DefaultLLMExecutor{Client: m.client(), Logger: testLogger()}
	got := e.refreshHeadSHA(context.Background(), testLogger(), "k-wa-wa/pechka", Item{Kind: kindIssue, Number: 1})
	if got != "" {
		t.Fatalf("refreshHeadSHA(issue) = %q, want empty", got)
	}
}

func TestRefreshHeadSHA_PullRequestFetchesCurrentSHA(t *testing.T) {
	m := newMockServer(t)
	m.singlePR = map[int]map[string]any{5: {"number": 5, "head": map[string]string{"sha": "freshsha"}}}
	e := &DefaultLLMExecutor{Client: m.client(), Logger: testLogger()}
	got := e.refreshHeadSHA(context.Background(), testLogger(), "k-wa-wa/pechka", Item{Kind: kindPullRequest, Number: 5, HeadSHA: "stalesha"})
	if got != "freshsha" {
		t.Fatalf("refreshHeadSHA(pr) = %q, want freshsha (authoritative value from GitHub)", got)
	}
}

func TestFinish_RunErrorProducesSynthesizedBlockedCommentAndLabel(t *testing.T) {
	m := newMockServer(t)
	e := &DefaultLLMExecutor{Client: m.client(), Logger: testLogger()}

	err := e.finish(context.Background(), testLogger(), "k-wa-wa/pechka", Item{Kind: kindIssue, Number: 1}, WorkerWork, "/does/not/matter", runner.Result{}, errors.New("exec: not found"))
	if err == nil {
		t.Fatalf("finish() error = nil, want an error when claude itself could not be launched")
	}

	if got := m.mutatingCallCount(); got != 2 {
		t.Fatalf("mutating call count = %d, want 2 (comment + awaiting_user_review label)", got)
	}
	commentCall, labelCall := m.calls[0], m.calls[1]
	if commentCall.Path != "/repos/k-wa-wa/pechka/issues/1/comments" {
		t.Fatalf("calls[0] = %+v, want a comment post", commentCall)
	}
	if !strings.Contains(commentCall.Body, "status=blocked") {
		t.Fatalf("comment body = %q, want it to contain status=blocked", commentCall.Body)
	}
	if !strings.Contains(commentCall.Body, "worker=work") {
		t.Fatalf("comment body = %q, want it to contain worker=work", commentCall.Body)
	}
	if labelCall.Path != "/repos/k-wa-wa/pechka/issues/1/labels" || !strings.Contains(labelCall.Body, LabelAwaitingUserReview) {
		t.Fatalf("calls[1] = %+v, want an agent:awaiting_user_review label add", labelCall)
	}
}

func TestFinish_MissingReportFileProducesSynthesizedBlocked(t *testing.T) {
	m := newMockServer(t)
	e := &DefaultLLMExecutor{Client: m.client(), Logger: testLogger()}

	err := e.finish(context.Background(), testLogger(), "k-wa-wa/pechka", Item{Kind: kindIssue, Number: 2}, WorkerWork, "/definitely/does/not/exist.json", runner.Result{ExitCode: 0, Success: true}, nil)
	if err == nil {
		t.Fatalf("finish() error = nil, want an error when the worker never wrote a report file")
	}
	if got := m.mutatingCallCount(); got != 2 {
		t.Fatalf("mutating call count = %d, want 2 (comment + label)", got)
	}
}

func TestFinish_InvalidStatusForWorkerProducesSynthesizedBlocked(t *testing.T) {
	m := newMockServer(t)
	e := &DefaultLLMExecutor{Client: m.client(), Logger: testLogger()}

	reportPath := writeReportFile(t, `{"status":"passed","summary":"done"}`) // "passed" is a verify-only status.
	err := e.finish(context.Background(), testLogger(), "k-wa-wa/pechka", Item{Kind: kindIssue, Number: 3}, WorkerWork, reportPath, runner.Result{ExitCode: 0, Success: true}, nil)
	if err == nil {
		t.Fatalf("finish() error = nil, want an error when the reported status is not valid for worker=work")
	}
}

func TestFinish_ValidDoneReportPostsCommentWithoutLabel(t *testing.T) {
	m := newMockServer(t)
	e := &DefaultLLMExecutor{Client: m.client(), Logger: testLogger()}

	reportPath := writeReportFile(t, `{"status":"done","summary":"実装した。テストも通過した。"}`)
	err := e.finish(context.Background(), testLogger(), "k-wa-wa/pechka", Item{Kind: kindIssue, Number: 4}, WorkerWork, reportPath, runner.Result{ExitCode: 0, Success: true}, nil)
	if err != nil {
		t.Fatalf("finish() error = %v, want nil for a valid report", err)
	}

	if got := m.mutatingCallCount(); got != 1 {
		t.Fatalf("mutating call count = %d, want 1 (comment only, no label since status is not blocked)", got)
	}
	if !strings.Contains(m.calls[0].Body, "status=done") || !strings.Contains(m.calls[0].Body, "実装した。テストも通過した。") {
		t.Fatalf("comment body = %q, want the rendered status line and summary", m.calls[0].Body)
	}
}

func TestFinish_ValidBlockedReportPostsCommentAndLabelWithNilError(t *testing.T) {
	// worker 自身が blocked と判断した場合は、nuage-autopilot にとっては正常な
	// 1 サイクルの完了である（合成された blocked とは違い、error を返さない）。
	m := newMockServer(t)
	e := &DefaultLLMExecutor{Client: m.client(), Logger: testLogger()}

	reportPath := writeReportFile(t, `{"status":"blocked","summary":"要件が曖昧なので確認したい。"}`)
	err := e.finish(context.Background(), testLogger(), "k-wa-wa/pechka", Item{Kind: kindIssue, Number: 5}, WorkerWork, reportPath, runner.Result{ExitCode: 0, Success: true}, nil)
	if err != nil {
		t.Fatalf("finish() error = %v, want nil (a worker-reported blocked is a normal outcome)", err)
	}
	if got := m.mutatingCallCount(); got != 2 {
		t.Fatalf("mutating call count = %d, want 2 (comment + awaiting_user_review label)", got)
	}
}

func TestFinish_PullRequestReportIncludesRefreshedSHA(t *testing.T) {
	m := newMockServer(t)
	m.singlePR = map[int]map[string]any{6: {"number": 6, "head": map[string]string{"sha": "newsha"}}}
	e := &DefaultLLMExecutor{Client: m.client(), Logger: testLogger()}

	reportPath := writeReportFile(t, `{"status":"passed","summary":"検証合格"}`)
	err := e.finish(context.Background(), testLogger(), "k-wa-wa/pechka", Item{Kind: kindPullRequest, Number: 6, HeadSHA: "oldsha"}, WorkerVerify, reportPath, runner.Result{ExitCode: 0, Success: true}, nil)
	if err != nil {
		t.Fatalf("finish() error = %v", err)
	}
	if !strings.Contains(m.calls[0].Body, "sha=newsha") {
		t.Fatalf("comment body = %q, want it to contain the freshly-fetched sha", m.calls[0].Body)
	}
}

func TestFinish_CreateCommentFailurePropagatesEvenWithAValidReport(t *testing.T) {
	m := newMockServer(t)
	m.failCreateComment = true
	e := &DefaultLLMExecutor{Client: m.client(), Logger: testLogger()}

	reportPath := writeReportFile(t, `{"status":"done","summary":"done"}`)
	err := e.finish(context.Background(), testLogger(), "k-wa-wa/pechka", Item{Kind: kindIssue, Number: 7}, WorkerWork, reportPath, runner.Result{ExitCode: 0, Success: true}, nil)
	if err == nil {
		t.Fatalf("finish() error = nil, want an error when the result comment could not be posted at all")
	}
}

// writeReportFile は worker が NUAGE_REPORT_FILE に書き出す JSON のテスト用フィクスチャを
// 一時ファイルとして書き出し、そのパスを返す。
func writeReportFile(t *testing.T, content string) string {
	t.Helper()
	path := t.TempDir() + "/report.json"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write report fixture: %v", err)
	}
	return path
}
