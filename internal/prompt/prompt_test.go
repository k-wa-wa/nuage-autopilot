package prompt

import (
	"strings"
	"testing"
	"time"
)

// mustContainAll は got が wants の全ての部分文字列を含むことを検証する。
func mustContainAll(t *testing.T, got string, wants ...string) {
	t.Helper()
	for _, w := range wants {
		if !strings.Contains(got, w) {
			t.Fatalf("prompt does not contain %q\n--- full prompt ---\n%s", w, got)
		}
	}
}

// mustNotContainAny は got が wants のいずれの部分文字列も含まないことを検証する。
func mustNotContainAny(t *testing.T, got string, avoids ...string) {
	t.Helper()
	for _, a := range avoids {
		if strings.Contains(got, a) {
			t.Fatalf("prompt must not contain %q\n--- full prompt ---\n%s", a, got)
		}
	}
}

func testIssueContext() Context {
	return Context{
		RepoName: "k-wa-wa/pechka",
		Kind:     KindIssue,
		Number:   42,
		Title:    "ログイン画面のバグを直す",
		Body:     "ログインボタンを押しても反応しない。",
	}
}

func testPRContext() Context {
	return Context{
		RepoName: "k-wa-wa/pechka",
		Kind:     KindPullRequest,
		Number:   42,
		Title:    "fix: ログイン画面のバグを直す",
		Body:     "Closes #40",
	}
}

// deprecatedReferences は旧 4-worker 構成（spec/dev/review/qa）から廃止された概念への
// 言及である。work/verify のプロンプトはいずれも言及してはならない。
var deprecatedReferences = []string{
	"agent:spec",
	"agent:dev",
	"agent:review-general",
	"agent:review-semantic",
	"agent:qa",
	"agent:wait",
	"agent:triage",
	"SpecAgent",
	"QAAgent",
}

func TestBuildWork_Issue_IncludesBranchAndPRInstructions(t *testing.T) {
	got := BuildWork(testIssueContext())
	mustContainAll(t, got,
		"k-wa-wa/pechka",
		"GitHub Issue #42",
		"ログイン画面のバグを直す",
		"feature/issue-42",
		"git checkout -B feature/issue-42",
		"Closes #42",
		"AGENTS.md",
		"ログインボタンを押しても反応しない。",
		`"status": "<status>"`,
		"done | blocked",
	)
	mustNotContainAny(t, got, deprecatedReferences...)
	if strings.Contains(got, "`") {
		t.Fatalf("BuildWork output must not contain backticks: %q", got)
	}
}

func TestBuildWork_PullRequest_ChecksOutExistingBranch(t *testing.T) {
	got := BuildWork(testPRContext())
	mustContainAll(t, got,
		"GitHub Pull Request #42",
		"gh pr checkout 42",
		"新しい PR を作成してはならない",
		"AGENTS.md",
		"done | blocked",
	)
	mustNotContainAny(t, got, deprecatedReferences...)
}

func TestBuildWork_IncludesVerifyFailureSummaryWhenPresent(t *testing.T) {
	ctx := testPRContext()
	ctx.VerifyFailureSummary = "internal/foo.go:12 の nil チェック漏れでパニックする。"
	got := BuildWork(ctx)
	mustContainAll(t, got, "直近の検証結果", ctx.VerifyFailureSummary)
}

func TestBuildWork_OmitsVerifyFailureSectionWhenAbsent(t *testing.T) {
	got := BuildWork(testPRContext())
	mustNotContainAny(t, got, "## 直近の検証結果")
}

func TestBuildWork_IncludesHumanCommentsWhenPresent(t *testing.T) {
	ctx := testIssueContext()
	ctx.HumanComments = []HumanComment{
		{Author: "k-wa-wa", CreatedAt: time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC), Body: "このエンドポイントも直してほしい"},
	}
	got := BuildWork(ctx)
	mustContainAll(t, got, "直近の人間コメント", "k-wa-wa", "このエンドポイントも直してほしい")
}

func TestBuildWork_AmbiguousRequirementInstructsBlocked(t *testing.T) {
	got := BuildWork(testIssueContext())
	mustContainAll(t, got, "要求が曖昧な場合", `status="blocked"`)
}

func TestBuildVerify_IncludesAllReviewAndExecutionPerspectives(t *testing.T) {
	got := BuildVerify(testPRContext())
	mustContainAll(t, got,
		"GitHub Pull Request #42",
		"gh pr checkout 42",
		"gh pr diff 42",
		// 旧 review-general が持っていた観点。
		"パフォーマンス",
		"セキュリティ",
		// 旧 review-semantic が持っていた観点。
		"設計規約適合度",
		"ドキュメントの同期",
		"影響範囲",
		// 旧 qa が持っていた検証項目。
		"結合・E2Eテスト",
		"完了基準チェックリスト",
		"人間による手動マージを待つ",
		"passed | failed | blocked",
	)
	mustNotContainAny(t, got, deprecatedReferences...)
	// 旧実装の自動マージ分岐（オートマージコマンド）は移植していないことを確認する。
	if strings.Contains(got, "gh pr merge") {
		t.Fatalf("BuildVerify must not include the auto-merge branch: %q", got)
	}
	// verify はコードを変更しないことを明言する。
	mustContainAll(t, got, "コードは一切変更せず")
}

func TestBuildVerify_DoesNotChangeCode(t *testing.T) {
	got := BuildVerify(testPRContext())
	mustContainAll(t, got, "あなた自身は修正しない")
}

func TestReportNote_ListsGivenStatuses(t *testing.T) {
	got := reportNote([]string{"done", "blocked"})
	mustContainAll(t, got, "NUAGE_REPORT_FILE", "done | blocked")
}

func TestContextSection_EmptyBodyIsMarked(t *testing.T) {
	ctx := testIssueContext()
	ctx.Body = ""
	got := contextSection(ctx)
	mustContainAll(t, got, "本文", "(無し)")
}
