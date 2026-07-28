// Package ingest は GitHub 側の変化を検知し、internal/store の events テーブルへ
// 取り込む（DESIGN.md 7章）。取り込み経路は 2 つある。
//
//   - Poller: GET /notifications の条件付きポーリングにより、コメント・レビュー・
//     新規 Issue/PR・CI チェックランの変化をイベントとして enqueue する。
//     internal/daemon.Poller を実装する。
//   - Resyncer: 全 open Issue/PR を走査し、DB を GitHub の現状に合わせて修復する。
//     イベントは一切 enqueue しない（着火はしない。DESIGN.md 7.5 節）。
//     internal/daemon.Resyncer を実装する。
//
// 両者に共通する「対象アイテムの選別」（DESIGN.md 15章）と、通知取り込みで
// 自分自身の投稿を無限ループの引き金にしないためのフィルタ（DESIGN.md 7.3 節）を
// このファイルに置く。
package ingest

import (
	"regexp"
	"strconv"
	"strings"

	"autopilot/internal/github"
	"autopilot/internal/store"
)

// IgnoreLabel が付いている Issue/PR は対象から除外する（DESIGN.md 8.5 節・15章）。
// このチェックは、アイテムを初めて DB に登録する時点でのみ行う（登録後に人間が
// 付け足した場合は継続監視の対象外にはならない。Phase 2 のスコープ外とする）。
const IgnoreLabel = "agent:ignore"

// isAllowedAuthor は author が allowed に含まれるかどうかを判定する。
// allowed が空の場合は制限なし（誰でも許可）とする。
func isAllowedAuthor(author string, allowed []string) bool {
	if len(allowed) == 0 {
		return true
	}
	for _, a := range allowed {
		if strings.EqualFold(a, author) {
			return true
		}
	}
	return false
}

// hasIgnoreLabel は labels に IgnoreLabel が含まれるかどうかを判定する。
func hasIgnoreLabel(labels []string) bool {
	for _, l := range labels {
		if l == IgnoreLabel {
			return true
		}
	}
	return false
}

// isSelfOrBot は c が自分自身（botLogin と一致）、または他の Bot による投稿かどうかを
// 判定する。真の場合、イベントを enqueue しない（DESIGN.md 7.3 節）。
//
// botLogin との一致に加えて type == "Bot" も見るのは、GitHub Actions bot や
// dependabot など、他の自動化による投稿も人間の意図ではないため同様に無視すべき
// だからである（旧設計の isHumanComment と同じ考え方）。
func isSelfOrBot(c github.Comment, botLogin string) bool {
	return c.User.Login == botLogin || c.User.Type == "Bot"
}

// subjectNumberRe は通知の subject.url（例:
// "https://api.github.com/repos/k-wa-wa/pechka/issues/42"）の末尾から Issue/PR
// 番号を取り出す。PullRequest 種別の subject でも url は /issues/{n} 形式である
// ことがあるため、パス末尾の数字のみに依存する。
var subjectNumberRe = regexp.MustCompile(`/(\d+)$`)

func parseSubjectNumber(rawURL string) (int, bool) {
	m := subjectNumberRe.FindStringSubmatch(rawURL)
	if m == nil {
		return 0, false
	}
	n, err := strconv.Atoi(m[1])
	if err != nil {
		return 0, false
	}
	return n, true
}

// subjectKind は通知の subject.type から store.Kind を導出する。
// Issue/PullRequest 以外（Commit, Discussion, Release 等）は ok=false を返し、
// 呼び出し側はそのスレッドを無視する。
func subjectKind(subjectType string) (store.Kind, bool) {
	switch subjectType {
	case "Issue":
		return store.KindIssue, true
	case "PullRequest":
		return store.KindPullRequest, true
	default:
		return "", false
	}
}
