package cycle

import (
	"testing"
	"time"

	"autopilot/internal/github"
	"autopilot/internal/report"
)

const testBotLogin = "nuage-autopilot"

func humanComment(body string, at time.Time) github.Comment {
	return github.Comment{Body: body, User: github.Author{Login: "alice", Type: "User"}, CreatedAt: at}
}

func botComment(worker, status, sha string, at time.Time) github.Comment {
	return github.Comment{
		Body:      report.Render(worker, status, sha, "summary"),
		User:      github.Author{Login: testBotLogin, Type: "User"},
		CreatedAt: at,
	}
}

func TestDeriveState_NoComments(t *testing.T) {
	ds := deriveState(nil, testBotLogin)
	if ds.HasStatusLine {
		t.Fatalf("ds.HasStatusLine = true, want false")
	}
}

func TestDeriveState_FindsLatestOwnStatusLine(t *testing.T) {
	now := time.Now()
	comments := []github.Comment{
		botComment(WorkerWork, report.StatusDone, "", now.Add(-2*time.Hour)),
		botComment(WorkerVerify, report.StatusFailed, "sha1", now.Add(-1*time.Hour)),
	}
	ds := deriveState(comments, testBotLogin)
	if !ds.HasStatusLine || ds.StatusLine.Worker != WorkerVerify || ds.StatusLine.Status != report.StatusFailed {
		t.Fatalf("ds = %+v, want the most recent status line (verify/failed)", ds)
	}
}

func TestDeriveState_IgnoresHumanCommentsThatLookLikeStatusLines(t *testing.T) {
	now := time.Now()
	comments := []github.Comment{
		{Body: report.Render(WorkerWork, report.StatusDone, "", ""), User: github.Author{Login: "alice", Type: "User"}, CreatedAt: now},
	}
	ds := deriveState(comments, testBotLogin)
	if ds.HasStatusLine {
		t.Fatalf("ds.HasStatusLine = true, want false (a human's comment must not be trusted as our own status line)")
	}
}

func TestDeriveState_NewerHumanComment(t *testing.T) {
	now := time.Now()
	tests := []struct {
		name     string
		comments []github.Comment
		want     bool
	}{
		{
			name: "human commented after the status line",
			comments: []github.Comment{
				botComment(WorkerWork, report.StatusDone, "", now.Add(-time.Hour)),
				humanComment("please also fix Y", now),
			},
			want: true,
		},
		{
			name: "status line is the most recent comment",
			comments: []github.Comment{
				humanComment("please fix X", now.Add(-time.Hour)),
				botComment(WorkerWork, report.StatusDone, "", now),
			},
			want: false,
		},
		{
			name:     "no status line at all",
			comments: []github.Comment{humanComment("hello", now)},
			want:     false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := deriveState(tt.comments, testBotLogin).NewerHumanComment; got != tt.want {
				t.Fatalf("NewerHumanComment = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestNextForPullRequest(t *testing.T) {
	now := time.Now()

	tests := []struct {
		name       string
		item       Item
		comments   []github.Comment
		wantAction string
	}{
		{
			name:       "draft is skipped regardless of everything else",
			item:       Item{Kind: kindPullRequest, Draft: true, CIStatus: "failure"},
			wantAction: WorkerNone,
		},
		{
			name:       "human comment after status line always asks, even if ci is failing",
			item:       Item{Kind: kindPullRequest, CIStatus: "failure"},
			comments:   []github.Comment{botComment(WorkerWork, report.StatusDone, "sha1", now.Add(-time.Hour)), humanComment("wait, don't touch this yet", now)},
			wantAction: ActionAsk,
		},
		{
			name:       "ci pending blocks regardless of status line",
			item:       Item{Kind: kindPullRequest, CIStatus: "pending"},
			wantAction: WorkerNone,
		},
		{
			name:       "ci failure routes to work even with no status line",
			item:       Item{Kind: kindPullRequest, CIStatus: "failure"},
			wantAction: WorkerWork,
		},
		{
			name:       "new pr with no status line and passing ci goes to verify",
			item:       Item{Kind: kindPullRequest, CIStatus: "success", HeadSHA: "sha1"},
			wantAction: WorkerVerify,
		},
		{
			name:       "new commits since the last status line re-triggers verify",
			item:       Item{Kind: kindPullRequest, CIStatus: "success", HeadSHA: "sha2"},
			comments:   []github.Comment{botComment(WorkerVerify, report.StatusPassed, "sha1", now)},
			wantAction: WorkerVerify,
		},
		{
			name:       "verify failed routes to work",
			item:       Item{Kind: kindPullRequest, CIStatus: "success", HeadSHA: "sha1"},
			comments:   []github.Comment{botComment(WorkerVerify, report.StatusFailed, "sha1", now)},
			wantAction: WorkerWork,
		},
		{
			name:       "work done routes to verify",
			item:       Item{Kind: kindPullRequest, CIStatus: "success", HeadSHA: "sha1"},
			comments:   []github.Comment{botComment(WorkerWork, report.StatusDone, "sha1", now)},
			wantAction: WorkerVerify,
		},
		{
			name:       "verify passed with no new commits and no human comment is a terminal none",
			item:       Item{Kind: kindPullRequest, CIStatus: "success", HeadSHA: "sha1"},
			comments:   []github.Comment{botComment(WorkerVerify, report.StatusPassed, "sha1", now)},
			wantAction: WorkerNone,
		},
		{
			name:       "blocked work resumes work after the label is cleared (no new human comment)",
			item:       Item{Kind: kindPullRequest, CIStatus: "success", HeadSHA: "sha1"},
			comments:   []github.Comment{botComment(WorkerWork, report.StatusBlocked, "sha1", now)},
			wantAction: WorkerWork,
		},
		{
			name:       "blocked verify resumes verify after the label is cleared",
			item:       Item{Kind: kindPullRequest, CIStatus: "success", HeadSHA: "sha1"},
			comments:   []github.Comment{botComment(WorkerVerify, report.StatusBlocked, "sha1", now)},
			wantAction: WorkerVerify,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := nextForPullRequest(tt.item, deriveState(tt.comments, testBotLogin))
			if got.Action != tt.wantAction {
				t.Fatalf("nextForPullRequest() = %+v, want Action=%q", got, tt.wantAction)
			}
		})
	}
}

func TestNextForIssue(t *testing.T) {
	now := time.Now()

	tests := []struct {
		name       string
		relatedPRs []int
		comments   []github.Comment
		wantAction string
	}{
		{
			name:       "has a related open pr defers to the pr",
			relatedPRs: []int{43},
			wantAction: WorkerNone,
		},
		{
			name:       "not started goes to work",
			wantAction: WorkerWork,
		},
		{
			name:       "human comment after status line asks",
			comments:   []github.Comment{botComment(WorkerWork, report.StatusDone, "", now.Add(-time.Hour)), humanComment("also handle the edge case", now)},
			wantAction: ActionAsk,
		},
		{
			name:       "work done without a linked pr awaits human",
			comments:   []github.Comment{botComment(WorkerWork, report.StatusDone, "", now)},
			wantAction: WorkerNone,
		},
		{
			name:       "blocked resumes work after the label is cleared",
			comments:   []github.Comment{botComment(WorkerWork, report.StatusBlocked, "", now)},
			wantAction: WorkerWork,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			item := Item{Kind: kindIssue}
			got := nextForIssue(item, deriveState(tt.comments, testBotLogin), tt.relatedPRs)
			if got.Action != tt.wantAction {
				t.Fatalf("nextForIssue() = %+v, want Action=%q", got, tt.wantAction)
			}
		})
	}
}

func TestNextAction_DispatchesByKind(t *testing.T) {
	prAction := nextAction(Item{Kind: kindPullRequest, CIStatus: "success", HeadSHA: "sha1"}, nil, testBotLogin, nil)
	if prAction.Action != WorkerVerify {
		t.Fatalf("nextAction(pr) = %+v, want WorkerVerify", prAction)
	}

	issueAction := nextAction(Item{Kind: kindIssue}, nil, testBotLogin, nil)
	if issueAction.Action != WorkerWork {
		t.Fatalf("nextAction(issue) = %+v, want WorkerWork", issueAction)
	}
}
