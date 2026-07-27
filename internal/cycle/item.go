package cycle

import (
	"time"

	"github.com/k-wa-wa/nuage-workspace/autopilot/internal/github"
)

// itemKind は Item が Issue か PR かを表す。ログの "kind" キーにそのまま出力する。
type itemKind string

const (
	kindIssue       itemKind = "issue"
	kindPullRequest itemKind = "pull_request"
)

// Item は Issue と PullRequest を cycle パッケージの状態機械が扱う上で共通に
// 見るための型である。GitHub 上は明確に別物であり、取得も internal/github 側で
// 別エンドポイント（/issues と /pulls）から行っているが、ラベル状態機械としての
// 遷移ロジックは両者で共通なため、ここでまとめて扱う。
type Item struct {
	Kind      itemKind
	Number    int
	Title     string
	Labels    []string
	UpdatedAt time.Time
}

func issueToItem(i github.Issue) Item {
	return Item{
		Kind:      kindIssue,
		Number:    i.Number,
		Title:     i.Title,
		Labels:    i.Labels,
		UpdatedAt: i.UpdatedAt,
	}
}

func pullRequestToItem(p github.PullRequest) Item {
	return Item{
		Kind:      kindPullRequest,
		Number:    p.Number,
		Title:     p.Title,
		Labels:    p.Labels,
		UpdatedAt: p.UpdatedAt,
	}
}
