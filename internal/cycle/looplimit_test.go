package cycle

import (
	"testing"
	"time"

	"autopilot/internal/github"
)

func TestBotCommentsSinceLastHuman(t *testing.T) {
	now := time.Now()
	bot := github.Author{Login: "nuage-autopilot", Type: "User"}
	human := github.Author{Login: "alice", Type: "User"}
	appBot := github.Author{Login: "some-app[bot]", Type: "Bot"}

	tests := []struct {
		name     string
		comments []github.Comment
		want     int
	}{
		{
			name:     "no comments",
			comments: nil,
			want:     0,
		},
		{
			name: "human is the most recent comment",
			comments: []github.Comment{
				{User: bot, CreatedAt: now.Add(-2 * time.Hour)},
				{User: human, CreatedAt: now.Add(-1 * time.Hour)},
			},
			want: 0,
		},
		{
			name: "counts bot comments after the last human comment",
			comments: []github.Comment{
				{User: human, CreatedAt: now.Add(-4 * time.Hour)},
				{User: bot, CreatedAt: now.Add(-3 * time.Hour)},
				{User: bot, CreatedAt: now.Add(-2 * time.Hour)},
				{User: bot, CreatedAt: now.Add(-1 * time.Hour)},
			},
			want: 3,
		},
		{
			name: "other bot (Type=Bot with different login) does not count towards loop limit nor reset it",
			comments: []github.Comment{
				{User: human, CreatedAt: now.Add(-3 * time.Hour)},
				{User: appBot, CreatedAt: now.Add(-2 * time.Hour)},
				{User: bot, CreatedAt: now.Add(-1 * time.Hour)},
			},
			want: 1,
		},
		{
			name: "no human comment at all counts every bot comment",
			comments: []github.Comment{
				{User: bot, CreatedAt: now.Add(-3 * time.Hour)},
				{User: bot, CreatedAt: now.Add(-2 * time.Hour)},
				{User: bot, CreatedAt: now.Add(-1 * time.Hour)},
			},
			want: 3,
		},
		{
			name: "unsorted input is handled correctly",
			comments: []github.Comment{
				{User: bot, CreatedAt: now.Add(-1 * time.Hour)},
				{User: human, CreatedAt: now.Add(-3 * time.Hour)},
				{User: bot, CreatedAt: now.Add(-2 * time.Hour)},
			},
			want: 2,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := botCommentsSinceLastHuman(tt.comments, "nuage-autopilot"); got != tt.want {
				t.Fatalf("botCommentsSinceLastHuman() = %d, want %d", got, tt.want)
			}
		})
	}
}
