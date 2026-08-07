package engine

import "autopilot/internal/store"

// action は 1 イベントに対する遷移表の評価結果である。
type action string

const (
	// actionIgnore は何もしないことを表す（phase=done、または該当行が無い組み合わせ）。
	actionIgnore action = "ignore"

	// actionToDone は close/merged イベントを受けて done へ遷移することを表す。
	// エージェントは起動しない。
	actionToDone action = "to_done"

	// actionLaunchVerify は in_review + ci_success を受けて verify を起動することを
	// 表す（DESIGN.md 8.4 節）。ready へ進めるかどうかは verify の判定が決めるため、
	// この時点では phase を動かさない。
	actionLaunchVerify action = "launch_verify"

	// actionLaunchNew は新規セッションでエージェントを起動することを表す。
	actionLaunchNew action = "launch_new"

	// actionLaunchResume は既存セッションを --resume で継続してエージェントを
	// 起動することを表す。
	actionLaunchResume action = "launch_resume"

	// actionToInReviewAndLaunch は ready 状態で人間が追加 push した（ci_failure）
	// ことを受けて in_review に戻し、エージェントを起動することを表す。
	actionToInReviewAndLaunch action = "to_in_review_and_launch"
)

// nextAction は DESIGN.md 8.1 節の遷移表をそのまま実装したものである。
// Go が決めるのは「起こすか否か」だけであり、何をするかはエージェント自身が
// 判断する（LLM はここでは一切呼ばない）。
func nextAction(phase store.Phase, eventType string) action {
	// closed/merged は phase を問わず優先する（DESIGN.md 8.1 節「任意」）。
	if eventType == "closed" || eventType == "merged" {
		return actionToDone
	}

	switch phase {
	case store.PhaseDone:
		return actionIgnore

	case store.PhaseNew:
		if eventType == "opened" || eventType == "commented" {
			return actionLaunchNew
		}

	case store.PhaseAwaitingAnswer, store.PhaseBlocked:
		if eventType == "commented" {
			return actionLaunchResume
		}

	case store.PhaseInReview:
		switch eventType {
		// verify_failure は verify の差し戻しを表す合成イベントである。人間の指摘
		// （commented）とまったく同じ扱いで agent を起こす。差し戻し理由は PR
		// コメントとして GitHub 側に置かれている（DESIGN.md 8.4 節）。
		case "ci_failure", "commented", "reviewed", "verify_failure":
			return actionLaunchResume
		case "ci_success":
			return actionLaunchVerify
		}

	case store.PhaseReady:
		switch eventType {
		case "commented", "reviewed":
			return actionLaunchResume
		case "ci_failure":
			return actionToInReviewAndLaunch
		}

	case store.PhaseDelegated:
		if eventType == "child_done" {
			return actionLaunchResume
		}
	}

	return actionIgnore
}
