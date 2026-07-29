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

func TestBuildAgent_ProhibitsUnsafeOperations(t *testing.T) {
	out := BuildAgent(Context{RepoName: "k-wa-wa/pechka", Kind: KindIssue, Number: 1, Title: "t"})

	for _, want := range []string{
		"force push",
		"SOPS",
		"Terraform",
		"secrets.env",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("prompt does not mention prohibition %q:\n%s", want, out)
		}
	}
}
