package web

import (
	"embed"
	"fmt"
	"html/template"
	"io/fs"
	"time"

	"autopilot/internal/store"
)

//go:embed static
var staticFSRaw embed.FS

// staticFS は "/static/style.css" のような絶対パスで配信するため、embed 側の
// "static" プレフィックスを剥がしたサブツリーを使う。
var staticFS, _ = fs.Sub(staticFSRaw, "static")

//go:embed templates/layout.gohtml
var layoutTmplSrc string

//go:embed templates/dashboard.gohtml
var dashboardTmplSrc string

//go:embed templates/item.gohtml
var itemTmplSrc string

var templateFuncs = template.FuncMap{
	"phaseLabel": phaseLabel,
	"kindLabel":  kindLabel,
	"eventLabel": eventLabel,
	"formatTime": func(t time.Time) string { return t.Local().Format("2006-01-02 15:04:05") },
	"ago":        ago,
	"money":      func(v float64) string { return fmt.Sprintf("$%.2f", v) },
	"itemHref":   itemHref,
	"githubHref": githubHref,
	"mul":        func(a, b float64) float64 { return a * b },
}

var dashboardTmpl = template.Must(
	template.New("layout").Funcs(templateFuncs).Parse(layoutTmplSrc + dashboardTmplSrc),
)

var itemTmpl = template.Must(
	template.New("layout").Funcs(templateFuncs).Parse(layoutTmplSrc + itemTmplSrc),
)

// phaseLabel は store.Phase を画面表示用の日本語ラベルに変換する（DESIGN.md 6.2 節の語彙）。
func phaseLabel(p store.Phase) string {
	switch p {
	case store.PhaseNew:
		return "未着手"
	case store.PhaseAwaitingAnswer:
		return "質問中"
	case store.PhaseInReview:
		return "レビュー中"
	case store.PhaseReady:
		return "マージ待ち"
	case store.PhaseBlocked:
		return "要対応"
	case store.PhaseDelegated:
		return "分割済み"
	case store.PhaseDone:
		return "完了"
	default:
		return string(p)
	}
}

// kindLabel は store.Kind を画面表示用のラベルに変換する。
func kindLabel(k store.Kind) string {
	switch k {
	case store.KindIssue:
		return "Issue"
	case store.KindPullRequest:
		return "PR"
	default:
		return string(k)
	}
}

// eventLabel は events.type を画面表示用の日本語ラベルに変換する
// （型そのものは internal/ingest が enqueue する文字列と一致させる必要があるため、
// ここでは表示専用の変換に留め、未知の type はそのまま表示する）。
func eventLabel(t string) string {
	switch t {
	case "opened":
		return "起票"
	case "commented":
		return "コメント"
	case "reviewed":
		return "レビュー"
	case "ci_success":
		return "CI 成功"
	case "ci_failure":
		return "CI 失敗"
	case "child_done":
		return "子アイテム完了"
	default:
		return t
	}
}

// ago は t から now までの経過時間を「3分前」のような相対表記にする。
// 監視画面として「どれだけ動きが無いか」を一目で分かるようにするための表示専用ヘルパーである。
func ago(now, t time.Time) string {
	d := now.Sub(t)
	switch {
	case d < 0:
		return "たった今"
	case d < time.Minute:
		return "たった今"
	case d < time.Hour:
		return fmt.Sprintf("%d分前", int(d/time.Minute))
	case d < 24*time.Hour:
		return fmt.Sprintf("%d時間前", int(d/time.Hour))
	default:
		return fmt.Sprintf("%d日前", int(d/(24*time.Hour)))
	}
}

// itemHref はアイテム詳細ページへのパスを作る。repo は "owner/name" 形式である前提。
func itemHref(repo string, number int) string {
	return fmt.Sprintf("/items/%s/%d", repo, number)
}

// githubHref はアイテムに対応する GitHub 上の URL を作る。閲覧専用画面から実物へ
// 飛べるようにするためのリンクであり、この画面自体からは何も操作できない。
func githubHref(repo string, kind store.Kind, number int) string {
	path := "issues"
	if kind == store.KindPullRequest {
		path = "pull"
	}
	return fmt.Sprintf("https://github.com/%s/%s/%d", repo, path, number)
}
