// このファイルは 1 サイクルの中核である遷移表を持つ。
//
// 「次にどの worker を起動すべきか」は、CI 状態・状態行・コミット SHA・関連 PR の
// 有無から機械的に導出できるケースがほとんどであり、それを LLM (dispatcher) の
// 判断に委ねる必要はない。nextAction はこの機械的な判断を行い、判断がつかない
// （直近の人間コメントの意図を読む必要がある）場合にのみ ActionAsk を返す。
// dispatcher が呼ばれるのは ActionAsk になった候補が存在するときだけである
// （cycle.go 参照）。
package cycle

import (
	"sort"

	"autopilot/internal/github"
	"autopilot/internal/report"
)

// ActionAsk は、遷移表だけでは次のアクションを決められず、dispatcher (LLM) に
// 直近の人間コメントの意図を判断させる必要があることを表す。
// これ以外の 3 値（WorkerWork/WorkerVerify/WorkerNone）は dispatcher.go で
// 定義されている worker 識別子をそのまま「次のアクション」として再利用する。
const ActionAsk = "ask"

// NextAction は 1 候補アイテムに対する遷移表の評価結果である。
type NextAction struct {
	// Action は WorkerWork / WorkerVerify / WorkerNone / ActionAsk のいずれか。
	Action string

	// Reason は判定理由（ログおよび dispatcher へのヒントに使う）。
	Reason string
}

// derivedState は、あるアイテムのコメント履歴から機械的に導出できる状態である。
type derivedState struct {
	// HasStatusLine は、autopilot 自身が投稿した状態行付きコメントが
	// 履歴の中に 1 件でも存在するかどうかを表す。
	HasStatusLine bool

	// StatusLine は履歴の中で最も新しい状態行である（HasStatusLine が true の場合のみ有効）。
	StatusLine report.StatusLine

	// NewerHumanComment は、StatusLine のコメントより新しい人間のコメントが
	// 存在するかどうかを表す。HasStatusLine が false の場合は常に false
	// （比較対象となる状態行自体が無いため、その場合は「未着手」として扱う）。
	NewerHumanComment bool
}

// deriveState は comments（順不同でよい）と botLogin から derivedState を計算する。
func deriveState(comments []github.Comment, botLogin string) derivedState {
	sorted := make([]github.Comment, len(comments))
	copy(sorted, comments)
	sort.Slice(sorted, func(i, j int) bool {
		return sorted[i].CreatedAt.After(sorted[j].CreatedAt) // 新しい順
	})

	var ds derivedState
	statusLineIdx := -1
	for i, c := range sorted {
		if !isOwnComment(c, botLogin) {
			continue
		}
		if sl, ok := report.Parse(c.Body); ok {
			ds.StatusLine = sl
			ds.HasStatusLine = true
			statusLineIdx = i
			break
		}
	}
	if statusLineIdx == -1 {
		return ds
	}

	for i := 0; i < statusLineIdx; i++ {
		if isHumanComment(sorted[i], botLogin) {
			ds.NewerHumanComment = true
			break
		}
	}
	return ds
}

// nextAction は it の遷移表評価結果を返す。relatedPRs は it が Issue の場合にのみ
// 参照される、その Issue に紐づくオープンな PR 番号の一覧である。
func nextAction(it Item, comments []github.Comment, botLogin string, relatedPRs []int) NextAction {
	ds := deriveState(comments, botLogin)
	if it.Kind == kindPullRequest {
		return nextForPullRequest(it, ds)
	}
	return nextForIssue(it, ds, relatedPRs)
}

// nextForPullRequest は PR に対する遷移表である。上から順に評価し、最初に
// 一致した条件の結果を返す。
func nextForPullRequest(it Item, ds derivedState) NextAction {
	if it.Draft {
		return NextAction{Action: WorkerNone, Reason: "pr is draft"}
	}

	// 人間の最新の意図はどんな機械的なルールよりも優先する。
	// CI が落ちていても、人間が「まだ触らないで」と言っていればそれに従うべきであり、
	// 機械的な ci_status=failure -> work のルールで上書きしてはならない。
	if ds.HasStatusLine && ds.NewerHumanComment {
		return NextAction{Action: ActionAsk, Reason: "human commented after the last status line"}
	}

	switch it.CIStatus {
	case "pending":
		return NextAction{Action: WorkerNone, Reason: "ci pending"}
	case "failure":
		return NextAction{Action: WorkerWork, Reason: "ci failure"}
	}

	if !ds.HasStatusLine {
		return NextAction{Action: WorkerVerify, Reason: "new pr, ci is not failing"}
	}

	// 状態行より新しいコミットが積まれている（work による修正、または人間による
	// 無言の push）場合、その状態行はもはや現在のコードを表していない。
	// sha は PR に対する実行でのみ埋まる（Issue では省略）ため、Issue のケースは
	// ここに来ない。
	if ds.StatusLine.SHA != "" && ds.StatusLine.SHA != it.HeadSHA {
		return NextAction{Action: WorkerVerify, Reason: "new commits since the last status line"}
	}

	switch {
	case ds.StatusLine.Status == report.StatusBlocked:
		// 人間が agent:awaiting_user_review を外したことで再びここに来ている
		// （ラベルが付いている間は候補にすら上がらない）。何が blocked にしたかは
		// 状態行の worker が知っているので、そこへ差し戻す。
		return NextAction{Action: ds.StatusLine.Worker, Reason: "resuming " + ds.StatusLine.Worker + " after blocked status was cleared"}
	case ds.StatusLine.Worker == WorkerVerify && ds.StatusLine.Status == report.StatusFailed:
		return NextAction{Action: WorkerWork, Reason: "verify failed"}
	case ds.StatusLine.Worker == WorkerWork && ds.StatusLine.Status == report.StatusDone:
		return NextAction{Action: WorkerVerify, Reason: "work done"}
	case ds.StatusLine.Worker == WorkerVerify && ds.StatusLine.Status == report.StatusPassed:
		return NextAction{Action: WorkerNone, Reason: "verify passed; awaiting human merge"}
	default:
		return NextAction{Action: WorkerNone, Reason: "unrecognized status line"}
	}
}

// nextForIssue は Issue に対する遷移表である。
func nextForIssue(it Item, ds derivedState, relatedPRs []int) NextAction {
	if len(relatedPRs) > 0 {
		return NextAction{Action: WorkerNone, Reason: "has a related open pr"}
	}

	if ds.HasStatusLine && ds.NewerHumanComment {
		return NextAction{Action: ActionAsk, Reason: "human commented after the last status line"}
	}

	if !ds.HasStatusLine {
		return NextAction{Action: WorkerWork, Reason: "not started"}
	}

	switch {
	case ds.StatusLine.Status == report.StatusBlocked:
		return NextAction{Action: ds.StatusLine.Worker, Reason: "resuming " + ds.StatusLine.Worker + " after blocked status was cleared"}
	case ds.StatusLine.Worker == WorkerWork && ds.StatusLine.Status == report.StatusDone:
		// work は完了したが PR が（まだ）紐付いていない。relatedPRs が空のため
		// ここに来ているが、work 自身が PR を作らなかった可能性が高い。
		return NextAction{Action: WorkerNone, Reason: "work done without a linked pr; awaiting human"}
	default:
		return NextAction{Action: WorkerNone, Reason: "unrecognized status line"}
	}
}
