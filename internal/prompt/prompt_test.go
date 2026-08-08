package prompt

import (
	"strings"
	"testing"
)

func TestBuildAgent_NewSessionIncludesFullContext(t *testing.T) {
	out := BuildAgent(Context{
		RepoName:   "k-wa-wa/pechka",
		Kind:       KindIssue,
		Number:     42,
		Title:      "add dark mode",
		Body:       "please add a dark mode toggle",
		Event:      EventInfo{Type: "opened", Actor: "alice", Body: "please add a dark mode toggle"},
		NewSession: true,
	})

	for _, want := range []string{
		"k-wa-wa/pechka",
		"issue #42",
		"add dark mode",
		"please add a dark mode toggle",
		"イベント種別: opened",
		"投稿者: alice",
		"NUAGE_REPORT_FILE",
		`"outcome"`,
		"outcome=\"asked\"",
		"outcome=\"split\"",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("prompt does not contain %q:\n%s", want, out)
		}
	}

	// 新規セッションは「これまでのセッションの続き」という文言を含まない。
	if strings.Contains(out, "セッションの続きとして") {
		t.Fatalf("new-session prompt should not claim to be a continuation:\n%s", out)
	}
}

func TestBuildAgent_ResumedSessionMentionsContinuity(t *testing.T) {
	out := BuildAgent(Context{
		RepoName:   "k-wa-wa/pechka",
		Kind:       KindPullRequest,
		Number:     7,
		Title:      "fix bug",
		Body:       "the fix",
		Event:      EventInfo{Type: "ci_failure", Actor: ""},
		NewSession: false,
	})

	if !strings.Contains(out, "セッションの続きとして") {
		t.Fatalf("resumed-session prompt should mention continuity:\n%s", out)
	}
	if !strings.Contains(out, "pull_request #7") {
		t.Fatalf("prompt does not contain the kind/number:\n%s", out)
	}
}

// TestBuildVerify_CarriesHeadSHAAndTreatsStaleTargetAsInconclusive は、検証対象の
// コミットがプロンプトに載ること、および検証対象が古い場合を差し戻しにしないことを
// 検証する（DESIGN.md 8.4 節）。
//
// head_sha を渡さないと、verify は古いコードを検証したまま合格を返しうる。
// 一方でその古さは実装の不備ではないため、差し戻すとエージェントが直しようのない
// 修正を繰り返す。
func TestBuildVerify_CarriesHeadSHAAndTreatsStaleTargetAsInconclusive(t *testing.T) {
	out := BuildVerify(Context{
		RepoName: "k-wa-wa/pechka",
		Kind:     KindPullRequest,
		Number:   7,
		Title:    "fix bug",
		HeadSHA:  "deadbeefcafe",
	})

	if !strings.Contains(out, "deadbeefcafe") {
		t.Fatalf("verify prompt does not carry the head sha:\n%s", out)
	}
	for _, want := range []string{
		"それが「対象コミット」に",
		"検証対象が対象コミットより古い場合も差し戻してはならない",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("verify prompt does not mention %q:\n%s", want, out)
		}
	}
}

// TestBuildVerify_DoesNotAssumeAPreviewEnvironment は、汎用プロンプトが特定の検証手段を
// 前提にしないことを検証する。preview を持たないリポジトリ（この autopilot 自身など）で
// 存在しないものを探させ、無用に inconclusive へ倒すのを防ぐためである。
func TestBuildVerify_DoesNotAssumeAPreviewEnvironment(t *testing.T) {
	out := BuildVerify(Context{RepoName: "k-wa-wa/nuage-autopilot", Kind: KindPullRequest, Number: 1, Title: "t"})
	if strings.Contains(out, "プレビュー") {
		t.Fatalf("verify prompt must not assume a preview environment:\n%s", out)
	}
}

// TestBuildAgent_OmitsHeadSHALineForIssues は Issue（head_sha を持たない）で
// 空行だけの項目が出ないことを検証する。
func TestBuildAgent_OmitsHeadSHALineForIssues(t *testing.T) {
	out := BuildAgent(Context{RepoName: "k-wa-wa/pechka", Kind: KindIssue, Number: 1, Title: "t"})
	if strings.Contains(out, "head_sha") {
		t.Fatalf("issue prompt should not mention head_sha:\n%s", out)
	}
}

// TestBuildVerify_ReviewsCodeButOnlyBlocksOnObjectiveDefects は、verify が
// コードレビューも担いつつ（DESIGN.md 8.4 節）、差し戻せる範囲が客観的に示せるものに
// 限定されていることを検証する。
//
// この制約が無いと、LLM は好みの指摘をいくらでも作れてしまい、直しようのない
// 差し戻しでエージェントが修正を繰り返して予算を焼く。
func TestBuildVerify_ReviewsCodeButOnlyBlocksOnObjectiveDefects(t *testing.T) {
	out := BuildVerify(Context{RepoName: "k-wa-wa/pechka", Kind: KindPullRequest, Number: 7, Title: "fix bug"})

	for _, want := range []string{
		"コードレビュー",
		"AGENTS.md",
		"差し戻してよい範囲",
		"客観的に示せるものだけ",
		"それを理由に verify_failed にしてはならない",
		"CI が機械的に判定できるものは CI の責務",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("verify prompt does not mention %q:\n%s", want, out)
		}
	}
}

// TestBuildVerify_ProhibitsImplementationAndPrefersInconclusive は verify の
// 独立性（コードを変更しない）と、迷ったときに不合格へ倒れない指示が
// プロンプトに含まれることを検証する（DESIGN.md 8.4 節）。
func TestBuildVerify_ProhibitsImplementationAndPrefersInconclusive(t *testing.T) {
	out := BuildVerify(Context{RepoName: "k-wa-wa/pechka", Kind: KindPullRequest, Number: 7, Title: "fix bug"})

	for _, want := range []string{
		"コードの変更・commit・push",
		"approve / merge",
		"verify_inconclusive",
		VerifyGuideFile,
		"pull_request #7",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("verify prompt does not mention %q:\n%s", want, out)
		}
	}

	// 実装側の outcome を verify に名乗らせない。
	if strings.Contains(out, `"implemented"`) {
		t.Fatalf("verify prompt must not offer the agent outcomes:\n%s", out)
	}
}

func TestBuildAgent_ProhibitsUnsafeOperations(t *testing.T) {
	out := BuildAgent(Context{RepoName: "k-wa-wa/pechka", Kind: KindIssue, Number: 1, Title: "t"})

	for _, want := range []string{
		"force push",
		"SOPS",
		"Terraform",
		"機密情報",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("prompt does not mention prohibition %q:\n%s", want, out)
		}
	}
}
