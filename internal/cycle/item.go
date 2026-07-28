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

// Item は Issue と PullRequest を cycle パッケージが扱う上で共通に見るための型である。
// GitHub 上は明確に別物であり、取得も internal/github 側で別エンドポイント
// （/issues と /pulls）から行っているが、dispatcher への候補提示・ループ上限判定・
// worker への引き渡しといった処理は両者で共通なため、ここでまとめて扱う。
type Item struct {
	Kind   itemKind
	Number int
	Title  string
	Author string

	// Body は Issue/PR の本文全文（未切り詰め）である。dispatcher に渡す際は
	// internal/cycle/dispatcher.go の buildDispatchCandidates が切り詰める。
	// worker はここから clone 後に必要なら全文を読み直せるため、Item の時点では
	// 切り詰めない。
	Body      string
	Labels    []string
	UpdatedAt time.Time
}

func issueToItem(i github.Issue) Item {
	return Item{
		Kind:      kindIssue,
		Number:    i.Number,
		Title:     i.Title,
		Author:    i.User.Login,
		Body:      i.Body,
		Labels:    i.Labels,
		UpdatedAt: i.UpdatedAt,
	}
}

func pullRequestToItem(p github.PullRequest) Item {
	return Item{
		Kind:      kindPullRequest,
		Number:    p.Number,
		Title:     p.Title,
		Author:    p.User.Login,
		Body:      p.Body,
		Labels:    p.Labels,
		UpdatedAt: p.UpdatedAt,
	}
}
