// Package cycle は 1 サイクル（対象リポジトリの Issue/PR を 1 周見て、処理すべきものが
// あれば処理して終了する処理単位）の制御フローを持つ。
//
// DESIGN.md 8章「ディスパッチャ方式」に従い、ラベルはプログラムカウンタとして使わない。
// 毎サイクル、open な Issue/PR の現実の状態（agent: ラベルが付いているか、ループ上限に
// 達しているか）から、dispatcher (claude haiku) が「どのアイテムをどの worker に渡すか」
// を判断し直す。1 サイクルで処理するのは高々 1 件のアイテムのみである
// （DESIGN.md 8章「1 サイクルの流れ」）。
package cycle

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/k-wa-wa/nuage-workspace/autopilot/internal/github"
)

// 1 サイクルで実際に取った行動を表す文字列。ログの action キーおよび
// Result.Action にそのまま使う。
const (
	// ActionNoop は今回のサイクルで worker を起動しなかったことを表す。
	// 対象が 0 件だった場合と、dispatcher が worker=none と判断した場合、
	// dispatcher がリトライしても有効な決定を出せなかった場合のいずれも含む
	// （詳細はログを参照する）。
	ActionNoop = "noop"

	// ActionAwaitingUserReview はループ上限に達したアイテムに
	// agent:awaiting_user_review を付与し、このサイクルは worker を起動しなかった
	// ことを表す（DESIGN.md 8章「ループ上限（Go 側の硬い制限）」）。
	ActionAwaitingUserReview = "awaiting_user_review"

	// ActionWorkerExecuted は dispatcher が選んだ worker (claude) を起動し、
	// 正常終了（終了コード 0）したことを表す。
	ActionWorkerExecuted = "worker_executed"

	// ActionWorkerFailed は worker (claude) の実行を試みたが、claude 自体を
	// 実行できなかった、または 0 以外の終了コードで終わったことを表す。
	// この場合もラベル状態は変更しない（agent:running のみ外す）ため、
	// 次サイクルで dispatcher が再度検討する対象になる。
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

	// Worker は dispatcher が選んだ worker（WorkerSpec 等）。
	// worker を起動していない場合は空文字列。
	Worker string
}

// Run は repo に対する 1 サイクルを実行する。
//
// 手順（DESIGN.md 8章「1 サイクルの流れ」に従う）:
//  1. open な Issue/PR を取得し、agent: ラベルが付いているものを除外する。
//  2. 残りが 0 件なら LLM を呼ばずに終了する。
//  3. ループ上限に達しているアイテムがあれば agent:awaiting_user_review を付けて
//     終了する（このサイクルは dispatcher を呼ばない）。
//  4. dispatcher (claude haiku) を 1 回だけ呼び、どのアイテムをどの worker に渡すかを
//     決めさせる。
//  5. 選ばれたアイテムに agent:running を付ける。
//  6. 対象リポジトリを clone/更新し、worker (claude) を起動する。
//  7. 終了したら agent:running を外す。
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
	// 今サイクルの dispatch 候補から除外して他の候補の検討に進む。
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

	// (4) dispatcher を 1 サイクル 1 コールだけ呼ぶ。
	decision, ok, err := dispatcher.Dispatch(ctx, repo, buildDispatchCandidates(candidates, commentsByNumber, botLogin, relatedPRs))
	if err != nil {
		return result, fmt.Errorf("cycle: dispatch: %w", err)
	}
	if !ok {
		logger.Info("dispatcher produced no actionable decision this cycle", "repo", repo)
		return result, nil
	}

	var chosen *Item
	for i := range candidates {
		if string(candidates[i].Kind) == decision.Kind && candidates[i].Number == decision.Number {
			chosen = &candidates[i]
			break
		}
	}
	if chosen == nil {
		// Dispatcher.Dispatch は候補集合との整合性を検証済みのはずだが、実装不備に
		// 備えて防御的にエラーとする。
		return result, fmt.Errorf("cycle: dispatcher chose kind=%s number=%d which is not among this cycle's candidates",
			decision.Kind, decision.Number)
	}

	result.ItemKind = string(chosen.Kind)
	result.ItemNumber = chosen.Number
	result.Worker = decision.Worker

	// (5) 選ばれたアイテムに agent:running を付ける。
	if err := client.AddLabel(ctx, repo, chosen.Number, LabelRunning); err != nil {
		return result, fmt.Errorf("cycle: add %s to %s#%d: %w", LabelRunning, repo, chosen.Number, err)
	}
	logger.Info("dispatched item to worker",
		"repo", repo, "issue_number", chosen.Number, "kind", string(chosen.Kind),
		"worker", decision.Worker, "reason", decision.Reason)

	// (6) 対象リポジトリを clone/更新し、worker (claude) を起動する。
	execErr := executor.Execute(ctx, repo, *chosen, decision.Worker)

	// (7) agent:running は成功・失敗にかかわらず外す（DESIGN.md 8章）。
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
			"repo", repo, "issue_number", chosen.Number, "worker", decision.Worker, "error", execErr.Error())
		result.Action = ActionWorkerFailed
		return result, nil
	}

	logger.Info("worker execution completed",
		"repo", repo, "issue_number", chosen.Number, "worker", decision.Worker)
	result.Action = ActionWorkerExecuted
	return result, nil
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
