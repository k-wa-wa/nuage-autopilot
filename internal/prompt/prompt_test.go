package prompt

import (
	"strconv"
	"strings"
	"testing"
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
// 廃止したラベルへの言及が worker のプロンプトから取り除かれていることを確認するために使う。
func mustNotContainAny(t *testing.T, got string, avoids ...string) {
	t.Helper()
	for _, a := range avoids {
		if strings.Contains(got, a) {
			t.Fatalf("prompt must not contain %q (deprecated label reference)\n--- full prompt ---\n%s", a, got)
		}
	}
}

func testContext() Context {
	return Context{
		RepoName: "k-wa-wa/pechka",
		Kind:     KindIssue,
		Number:   42,
		Title:    "ログイン画面のバグを直す",
	}
}

// deprecatedLabels は DESIGN.md 8章のディスパッチャ方式移行に伴い廃止されたラベルである。
// worker のプロンプトはいずれもこれらへ言及してはならない
// （agent:awaiting_user_review のみ廃止されておらず、別途検証する）。
var deprecatedLabels = []string{
	"agent:spec",
	"agent:dev",
	"agent:review-general",
	"agent:review-semantic",
	"agent:qa",
	"agent:wait",
	"agent:triage",
}

func TestBuildSpec_IncludesIssueContextAndActionSteps(t *testing.T) {
	got := BuildSpec(testContext())
	mustContainAll(t, got,
		"k-wa-wa/pechka",
		"仕様定義エージェント (SpecAgent)",
		"GitHub Issue #42",
		"ログイン画面のバグを直す",
		"gh issue view 42 --comments",
		"AGENTS.md",
		"agent:awaiting_user_review",
		strconv.Itoa(42),
	)
	mustNotContainAny(t, got, deprecatedLabels...)
	// バッククォートを使っていないこと（Go の raw string 制約に基づく置換方針の確認）。
	if strings.Contains(got, "`") {
		t.Fatalf("BuildSpec output must not contain backticks: %q", got)
	}
}

func TestBuildDevIssue_IncludesBranchAndPRInstructions(t *testing.T) {
	ctx := testContext()
	got := BuildDevIssue(ctx)
	mustContainAll(t, got,
		"開発エージェント (DevAgent)",
		"GitHub Issue #42",
		"feature/issue-42",
		"git checkout -B feature/issue-42",
		"AGENTS.md",
		"agent:awaiting_user_review",
	)
	mustNotContainAny(t, got, deprecatedLabels...)
}

func TestBuildDevPR_IncludesReviewFetchAndCheckout(t *testing.T) {
	ctx := testContext()
	ctx.Kind = KindPullRequest
	got := BuildDevPR(ctx)
	mustContainAll(t, got,
		"DevAgent - PR修正担当",
		"GitHub Pull Request #42",
		"gh pr checkout 42",
		"AGENTS.md",
		"agent:awaiting_user_review",
	)
	mustNotContainAny(t, got, deprecatedLabels...)
}

func TestBuildReview_IncludesAllReviewPerspectives(t *testing.T) {
	ctx := testContext()
	ctx.Kind = KindPullRequest
	got := BuildReview(ctx)
	mustContainAll(t, got,
		"Reviewer",
		"gh pr diff 42",
		// 旧 review-general が持っていた観点。
		"パフォーマンス",
		"セキュリティ",
		// 旧 review-semantic が持っていた観点。
		"設計規約適合度",
		"ドキュメントの同期",
		"影響範囲",
		"agent:awaiting_user_review",
	)
	mustNotContainAny(t, got, deprecatedLabels...)
}

func TestBuildQA_IncludesVerificationAndManualMergePath(t *testing.T) {
	ctx := testContext()
	ctx.Kind = KindPullRequest
	got := BuildQA(ctx)
	mustContainAll(t, got,
		"QAAgent",
		"gh pr checkout 42",
		"検証項目",
		"手動でのマージを求める",
		"agent:awaiting_user_review",
	)
	mustNotContainAny(t, got, deprecatedLabels...)
	// 旧実装の自動マージ分岐（オートマージコマンド）は移植していないことを確認する。
	if strings.Contains(got, "gh pr merge") {
		t.Fatalf("BuildQA must not include the auto-merge branch: %q", got)
	}
}

func TestAwaitingUserReviewNote_KindBranching(t *testing.T) {
	issueCtx := Context{Kind: KindIssue, Number: 42}
	issueGot := awaitingUserReviewNote(issueCtx)
	mustContainAll(t, issueGot, `gh issue edit 42 --add-label "agent:awaiting_user_review"`)

	prCtx := Context{Kind: KindPullRequest, Number: 42}
	prGot := awaitingUserReviewNote(prCtx)
	mustContainAll(t, prGot, `gh pr edit 42 --add-label "agent:awaiting_user_review"`)
}
