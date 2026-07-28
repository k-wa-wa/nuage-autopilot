// Package cycle は 1 サイクル（対象リポジトリの Issue/PR を 1 周見て、処理すべきものが
// あれば処理して終了する処理単位）の制御フローを持つ。
//
// 「次にどの worker (work/verify) を起動すべきか」は、CI 状態・状態行・コミット SHA・
// 関連 PR の有無から機械的に導出できるケースがほとんどである（transition.go の
// 遷移表）。dispatcher (claude haiku) が呼ばれるのは、遷移表が「直近の人間コメントの
// 意図を読む必要がある」と判定した候補が存在するときだけであり、それ以外のサイクルは
// LLM を一切呼ばない。1 サイクルで処理するのは高々 1 件のアイテムのみである。
package cycle

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"autopilot/internal/github"
)

// 1 サイクルで実際に取った行動を表す文字列。ログの action キーおよび
// Result.Action にそのまま使う。
const (
	// ActionNoop は今回のサイクルで worker を起動しなかったことを表す。
	// 対象が 0 件だった場合、遷移表がすべて WorkerNone と判定した場合、
	// dispatcher が worker=none と判断した場合、dispatcher がリトライしても
	// 有効な決定を出せなかった場合のいずれも含む（詳細はログを参照する）。
	ActionNoop = "noop"

	// ActionAwaitingUserReview はループ上限に達したアイテムに
	// agent:awaiting_user_review を付与し、このサイクルは worker を起動しなかった
	// ことを表す。
	ActionAwaitingUserReview = "awaiting_user_review"

	// ActionWorkerExecuted は選ばれた worker (claude) を起動し、正常終了（終了コード 0）
	// かつ有効な報告（internal/report）を残したことを表す。
	ActionWorkerExecuted = "worker_executed"

	// ActionWorkerFailed は worker (claude) の実行を試みたが、claude 自体を
	// 実行できなかった、0 以外の終了コードで終わった、または有効な報告を
	// 残さなかった（report ファイル未生成・不正な JSON・その worker にとって
	// 妥当でない status 等）ことを表す。この場合もラベル状態は変更しない
	// （agent:running のみ外す）ため、次サイクルで再度検討される。
	ActionWorkerFailed = "worker_failed"
)

// Result は 1 サイクルの実行結果を表す。
type Result struct {
	// Repo は処理対象のリポジトリ（owner/name 形式）。
	Repo string

	// StateDir はサイクル実行に使用した作業ディレクトリ。
	StateDir string

	// StartedAt はサイクル開始時刻。
	StartedAt time.Time

	// Action は今回のサイクルで実際に取った行動（Action* 定数のいずれか）。
	Action string

	// ItemKind は Action の対象が "issue" か "pull_request" か。
	// Action が ActionNoop の場合は空文字列。
	ItemKind string

	// ItemNumber は Action の対象の Issue/PR 番号。Action が ActionNoop の場合は 0。
	ItemNumber int

	// Worker は選ばれた worker（WorkerWork/WorkerVerify）。
	// worker を起動していない場合は空文字列。
	Worker string
}

// classifiedCandidate は遷移表評価後の 1 候補である。
type classifiedCandidate struct {
	Item   Item
	Action string
	Reason string
}

// Run は repo に対する 1 サイクルを実行する。
//
// 手順:
//  1. open な Issue/PR を取得し、agent: ラベルが付いているものを除外する。
//  2. 残りが 0 件なら LLM を呼ばずに終了する。
//  3. ループ上限に達しているアイテムがあれば agent:awaiting_user_review を付けて
//     終了する（このサイクルは遷移表も dispatcher も評価しない）。
//  4. 残った候補それぞれについて遷移表 (transition.go) を評価する。
//  5. work/verify が機械的に決まった候補があれば、その中から 1 件選ぶ
//     （PR を優先し、同種であれば updated_at が古いものを優先する）。
//  6. 機械的に決まった候補が無く、"ask" と判定された候補があれば、それらのみを
//     dispatcher (claude haiku) に渡して 1 件選ばせる。
//  7. 選ばれたアイテムに agent:running を付ける。
//  8. 対象リポジトリを clone/更新し、worker (claude) を起動する。
//  9. 終了したら agent:running を外す。
func Run(ctx context.Context, logger *slog.Logger, client *github.Client, dispatcher Dispatcher, executor LLMExecutor, repo, stateDir string, allowedAuthors []string) (Result, error) {
	result := Result{
		Repo:      repo,
		StateDir:  stateDir,
		StartedAt: time.Now(),
		Action:    ActionNoop,
	}

	// サービス利用ラベルをあらかじめ冪等に作成し、gh CLI 等によるラベル付与エラーを防ぐ。
	for _, l := range []string{LabelRunning, LabelAwaitingUserReview} {
		if err := client.CreateLabel(ctx, repo, l); err != nil {
			logger.Warn("failed to ensure label exists", "repo", repo, "label", l, "error", err.Error())
		}
	}

	issues, err := client.ListOpenIssues(ctx, repo)
	if err != nil {
		return result, fmt.Errorf("cycle: list open issues: %w", err)
	}
	prs, err := client.ListOpenPullRequests(ctx, repo)
	if err != nil {
		return result, fmt.Errorf("cycle: list open pull requests: %w", err)
	}

	items := make([]Item, 0, len(issues)+len(prs))
	for _, i := range issues {
		items = append(items, issueToItem(i))
	}
	for _, p := range prs {
		items = append(items, pullRequestToItem(p))
	}

	// 候補の絞り込み前に、全 open アイテムから Issue ↔ PR の紐付けマップを作成しておく。
	// フィルタで除外される PR （awaiting_user_review 付き等）も含めて紐付けを維持するため。
	relatedPRs := buildIssuePRLinks(items)

	// (1) 候補の絞り込み:
	// - agent: ラベルが 1 つでも付いているアイテムは対象外（オプトアウト方式）。
	// - allowedAuthors が指定されている場合、作成者が含まれていないアイテムは対象外。
	var candidates []Item
	for _, it := range items {
		if hasAgentLabel(it.Labels) {
			continue
		}
		if !isAllowedAuthor(it.Author, allowedAuthors) {
			continue
		}
		candidates = append(candidates, it)
	}

	// (2) 対象が 0 件なら LLM を呼ばずに終了する。
	if len(candidates) == 0 {
		logger.Info("no eligible item this cycle (all items are excluded by an agent: label, author filter, or the repository has nothing open)",
			"repo", repo)
		return result, nil
	}

	botLogin, err := client.CurrentUser(ctx)
	if err != nil {
		return result, fmt.Errorf("cycle: resolve current user: %w", err)
	}

	// 各候補のコメントと CI チェックラン状態を取得する。
	commentsByNumber := make(map[int][]github.Comment, len(candidates))
	var validCandidates []Item
	for i := range candidates {
		it := candidates[i]
		comments, err := client.ListComments(ctx, repo, it.Number, it.Kind == kindPullRequest)
		if err != nil {
			logger.Error("failed to list comments for candidate; skipping item for this cycle",
				"repo", repo, "number", it.Number, "error", err.Error())
			continue
		}
		commentsByNumber[it.Number] = comments

		if it.Kind == kindPullRequest && it.HeadSHA != "" {
			st, err := client.GetCheckState(ctx, repo, it.HeadSHA)
			if err != nil {
				logger.Warn("failed to fetch CI check runs; defaulting to none", "repo", repo, "pr", it.Number, "error", err.Error())
				it.CIStatus = "none"
			} else {
				it.CIStatus = st
			}
		}
		validCandidates = append(validCandidates, it)
	}
	candidates = validCandidates

	// (3) ループ上限に達しているアイテムがあれば agent:awaiting_user_review を付与し、
	// 今サイクルの検討対象から除外して他の候補の検討に進む。
	var eligibleCandidates []Item
	for _, it := range candidates {
		if botCommentsSinceLastHuman(commentsByNumber[it.Number], botLogin) >= LoopLimit {
			if err := client.AddLabel(ctx, repo, it.Number, LabelAwaitingUserReview); err != nil {
				logger.Error("failed to add label to over-limit item",
					"repo", repo, "issue_number", it.Number, "error", err.Error())
			} else {
				logger.Warn("loop limit reached; labeling for human review",
					"repo", repo, "issue_number", it.Number, "kind", string(it.Kind), "limit", LoopLimit)
			}
			result.Action = ActionAwaitingUserReview
			result.ItemKind = string(it.Kind)
			result.ItemNumber = it.Number
		} else {
			eligibleCandidates = append(eligibleCandidates, it)
		}
	}
	candidates = eligibleCandidates

	if len(candidates) == 0 {
		logger.Info("no eligible candidate remaining after filtering and loop limit check", "repo", repo)
		return result, nil
	}

	// (4) 遷移表を評価し、機械的に決まる候補（decisive）と、人間コメントの意図を
	// 読む必要がある候補（asking）に分ける。WorkerNone と判定された候補は今回は
	// 何もしないため、どちらにも含めない。
	var decisive []classifiedCandidate
	var asking []classifiedCandidate
	for _, it := range candidates {
		na := nextAction(it, commentsByNumber[it.Number], botLogin, relatedPRs[it.Number])
		switch na.Action {
		case WorkerWork, WorkerVerify:
			decisive = append(decisive, classifiedCandidate{Item: it, Action: na.Action, Reason: na.Reason})
		case ActionAsk:
			asking = append(asking, classifiedCandidate{Item: it, Action: na.Action, Reason: na.Reason})
		default:
			logger.Debug("transition table produced no action for candidate", "repo", repo, "number", it.Number, "kind", string(it.Kind), "reason", na.Reason)
		}
	}

	var chosen *Item
	var chosenWorker string

	if len(decisive) > 0 {
		// (5) 機械的に決まった候補から 1 件選ぶ。
		best := selectDecisiveCandidate(decisive)
		chosen = &best.Item
		chosenWorker = best.Action
		logger.Info("transition table decided the next action", "repo", repo, "issue_number", chosen.Number, "kind", string(chosen.Kind), "worker", chosenWorker, "reason", best.Reason)
	} else if len(asking) > 0 {
		// (6) 機械的に決められる候補が無い場合のみ dispatcher を呼ぶ。
		askItems := make([]Item, 0, len(asking))
		reasons := make(map[int]string, len(asking))
		for _, c := range asking {
			askItems = append(askItems, c.Item)
			reasons[c.Item.Number] = c.Reason
		}

		decision, ok, err := dispatcher.Dispatch(ctx, repo, buildDispatchCandidates(askItems, reasons, commentsByNumber, botLogin, relatedPRs))
		if err != nil {
			return result, fmt.Errorf("cycle: dispatch: %w", err)
		}
		if !ok {
			logger.Info("dispatcher produced no actionable decision this cycle", "repo", repo)
			return result, nil
		}

		for i := range asking {
			if string(asking[i].Item.Kind) == decision.Kind && asking[i].Item.Number == decision.Number {
				chosen = &asking[i].Item
				break
			}
		}
		if chosen == nil {
			// Dispatcher.Dispatch は候補集合との整合性を検証済みのはずだが、実装不備に
			// 備えて防御的にエラーとする。
			return result, fmt.Errorf("cycle: dispatcher chose kind=%s number=%d which is not among this cycle's ask candidates",
				decision.Kind, decision.Number)
		}
		chosenWorker = decision.Worker
		logger.Info("dispatcher decided the next action", "repo", repo, "issue_number", chosen.Number, "kind", string(chosen.Kind), "worker", chosenWorker, "reason", decision.Reason)
	} else {
		logger.Info("no eligible candidate remaining after transition table evaluation", "repo", repo)
		return result, nil
	}

	result.ItemKind = string(chosen.Kind)
	result.ItemNumber = chosen.Number
	result.Worker = chosenWorker

	// (7) 選ばれたアイテムに agent:running を付ける。
	if err := client.AddLabel(ctx, repo, chosen.Number, LabelRunning); err != nil {
		return result, fmt.Errorf("cycle: add %s to %s#%d: %w", LabelRunning, repo, chosen.Number, err)
	}
	logger.Info("dispatched item to worker",
		"repo", repo, "issue_number", chosen.Number, "kind", string(chosen.Kind), "worker", chosenWorker)

	// (8) 対象リポジトリを clone/更新し、worker (claude) を起動する。
	execErr := executor.Execute(ctx, repo, *chosen, chosenWorker)

	// (9) agent:running は成功・失敗にかかわらず外す。
	// worker 実行中にシグナル等で ctx がキャンセルされた場合でも RemoveLabel が
	// context canceled で失敗してラベルが残留しないよう、context.WithoutCancel を使用する。
	cleanupCtx, cleanupCancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cleanupCancel()
	if removeErr := client.RemoveLabel(cleanupCtx, repo, chosen.Number, LabelRunning); removeErr != nil {
		logger.Error("failed to remove agent:running after worker execution; a human must remove it manually",
			"repo", repo, "issue_number", chosen.Number, "error", removeErr.Error())
	}

	if execErr != nil {
		// worker の実行自体が失敗しても nuage-autopilot の異常とはしない。
		// アイテムには agent: ラベルが残らないため、次サイクルで dispatcher が
		// 再度検討する対象になる。
		logger.Error("worker execution failed",
			"repo", repo, "issue_number", chosen.Number, "worker", chosenWorker, "error", execErr.Error())
		result.Action = ActionWorkerFailed
		return result, nil
	}

	logger.Info("worker execution completed",
		"repo", repo, "issue_number", chosen.Number, "worker", chosenWorker)
	result.Action = ActionWorkerExecuted
	return result, nil
}

// selectDecisiveCandidate は機械的に決まった候補（cands）の中から今サイクルで
// 処理する 1 件を選ぶ。PR を Issue より優先し（PR は「仕掛品」であり先に流し切る
// ことで WIP を溜めないため）、同種であれば updated_at が古いものを優先する
// （長く放置されているアイテムを先に処理する）。
func selectDecisiveCandidate(cands []classifiedCandidate) classifiedCandidate {
	best := cands[0]
	for _, c := range cands[1:] {
		if isBetterDecisiveCandidate(c, best) {
			best = c
		}
	}
	return best
}

func isBetterDecisiveCandidate(a, b classifiedCandidate) bool {
	aPR := a.Item.Kind == kindPullRequest
	bPR := b.Item.Kind == kindPullRequest
	if aPR != bPR {
		return aPR
	}
	return a.Item.UpdatedAt.Before(b.Item.UpdatedAt)
}

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
