// Package cycle は 1 サイクル（対象リポジトリの Issue/PR を 1 周見て、処理すべきものが
// あれば処理して終了する処理単位）の制御フローを持つ。
//
// DESIGN.md 7章にある通り、プロセスは状態を持たない。状態は GitHub のラベルのみが
// 保持する。1 回の Run 呼び出しで実行するのは高々 1 件の遷移のみである。
//
// Phase 2 の時点では GitHub 連携（Issue/PR 取得・ラベル判定・LLM を要しない遷移）
// のみを実装する。LLM CLI (claude/agy) の起動は Phase 3 で executeLLMPhase の中身を
// 差し替えることで行う。
package cycle

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/k-wa-wa/nuage-workspace/autopilot/internal/github"
)

// 1 サイクルで実際に取った行動を表す文字列。ログの action キーおよび
// Result.Action にそのまま使う。
const (
	// ActionNoop は今回のサイクルで何も遷移させるものが無かったことを表す。
	ActionNoop = "noop"

	// ActionAssignSpec はラベル無し Issue に agent:spec を付与したことを表す。
	ActionAssignSpec = "assign_spec"

	// ActionClearWait は agent:wait を解除したことを表す。
	ActionClearWait = "clear_wait"

	// ActionEscalateTriage はアクティブフェーズのタイムアウトを検知し、
	// agent:triage へ遷移させたことを表す。
	ActionEscalateTriage = "escalate_triage"

	// ActionLLMPhasePending は LLM の実行を要するフェーズに到達し、
	// その対象を特定してログに出力したことを表す（Phase 2 では実行しない）。
	ActionLLMPhasePending = "llm_phase_pending"
)

// DefaultActivePhaseTimeout は、アクティブフェーズラベル（spec/dev/review-*/qa）が
// 付いたまま進捗がないと判断し agent:triage へ強制遷移させるまでの経過時間である。
//
// DESIGN.md 8章はこの遷移の存在のみを定めており具体的な閾値は指定していないため、
// 本実装で決定した値である。1 サイクルの既定間隔（5分）および 1 回の実行の
// TimeoutStartSec（既定30分、nix/modules/nuage-autopilot.nix）よりも十分大きく
// 取ることで、正常な処理中のサイクルを誤って triage 送りにしないようにしている。
// Phase 3 で実際の LLM 実行時間の分布が分かった段階で見直す前提の暫定値である。
const DefaultActivePhaseTimeout = 2 * time.Hour

// options は Run の挙動を調整するための内部設定である。
type options struct {
	activePhaseTimeout time.Duration
}

// Option は Run の挙動を変更する関数オプションである。
type Option func(*options)

// WithActivePhaseTimeout は DefaultActivePhaseTimeout を上書きする。
// 主にテストで「タイムアウトした」状態を決定的に再現するために用意する。
func WithActivePhaseTimeout(d time.Duration) Option {
	return func(o *options) {
		if d > 0 {
			o.activePhaseTimeout = d
		}
	}
}

// Result は 1 サイクルの実行結果を表す。
type Result struct {
	// Repo は処理対象のリポジトリ（owner/name 形式）。
	Repo string

	// StateDir はサイクル実行に使用した作業ディレクトリ。
	// Phase 2 の時点では git clone を行わないため未使用だが、呼び出し元
	// （main.go）のログに残すために引き続き保持する。
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

	// Label は Action に関連するラベル名。Action が ActionNoop の場合は空文字列。
	Label string
}

// Run は repo に対する 1 サイクルを実行する。
//
// 手順（DESIGN.md 7章・8章、および本タスクで指示された遷移ルールに従う）:
//  1. 対象リポジトリの open な Issue と PR を取得する。
//  2. 各アイテムのラベルからフェーズを判定する。
//  3. LLM を要しない遷移（ラベル無し Issue への agent:spec 付与、
//     agent:wait の解除、アクティブフェーズのタイムアウトによる agent:triage 遷移）
//     を、最初に見つかったもの 1 件だけ実行して即座に return する。
//  4. LLM を要するフェーズ（spec/dev/review-*/qa）に到達した場合は実行せず、
//     対象を特定したことをログに出力して return する（Phase 3 で差し替える）。
//  5. どのアイテムにも行動の必要が無ければ ActionNoop を返す。
func Run(ctx context.Context, logger *slog.Logger, client *github.Client, repo, stateDir string, opts ...Option) (Result, error) {
	cfg := options{activePhaseTimeout: DefaultActivePhaseTimeout}
	for _, opt := range opts {
		opt(&cfg)
	}

	result := Result{
		Repo:      repo,
		StateDir:  stateDir,
		StartedAt: time.Now(),
		Action:    ActionNoop,
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

	now := time.Now()
	var botLogin string
	var botLoginResolved bool

	for _, item := range items {
		state := classifyLabels(item.Labels)

		if state.Ambiguous {
			logger.Warn("multiple active phase labels on a single item, using the first match",
				"repo", repo, "issue_number", item.Number, "kind", string(item.Kind), "labels", item.Labels)
		}

		// (a) ラベルが 1 つも付いていない Issue → agent:spec を付与する。
		// PR には適用しない（PR は agent:dev フェーズが既にラベル付きで作成する想定のため）。
		if item.Kind == kindIssue && !state.HasAnyAgentLabel {
			if err := client.AddLabel(ctx, repo, item.Number, LabelSpec); err != nil {
				return result, fmt.Errorf("cycle: assign %s to %s#%d: %w", LabelSpec, repo, item.Number, err)
			}
			logger.Info("assigned initial phase label",
				"repo", repo, "issue_number", item.Number, "label", LabelSpec, "action", ActionAssignSpec)
			result.Action = ActionAssignSpec
			result.ItemKind = string(item.Kind)
			result.ItemNumber = item.Number
			result.Label = LabelSpec
			return result, nil
		}

		// (b) agent:wait が付いている → 人間の新しいコメントがあれば解除する。
		if state.Waiting {
			if !botLoginResolved {
				login, err := client.CurrentUser(ctx)
				if err != nil {
					return result, fmt.Errorf("cycle: resolve current user: %w", err)
				}
				botLogin = login
				botLoginResolved = true
			}

			comments, err := client.ListComments(ctx, repo, item.Number)
			if err != nil {
				return result, fmt.Errorf("cycle: list comments for %s#%d: %w", repo, item.Number, err)
			}

			if shouldClearWait(comments, botLogin) {
				if err := client.RemoveLabel(ctx, repo, item.Number, LabelWait); err != nil {
					return result, fmt.Errorf("cycle: clear %s on %s#%d: %w", LabelWait, repo, item.Number, err)
				}
				logger.Info("cleared wait label after human response",
					"repo", repo, "issue_number", item.Number, "label", LabelWait, "action", ActionClearWait)
				result.Action = ActionClearWait
				result.ItemKind = string(item.Kind)
				result.ItemNumber = item.Number
				result.Label = LabelWait
				return result, nil
			}

			logger.Debug("still waiting for a human response",
				"repo", repo, "issue_number", item.Number, "label", LabelWait)
			continue
		}

		// (c) agent:triage は Phase 2 時点で自動遷移を持たない例外状態。
		// 人間の対応を待つのみで、次のアイテムへ進む。
		if state.Triage {
			logger.Debug("skipping item under triage",
				"repo", repo, "issue_number", item.Number, "label", LabelTriage)
			continue
		}

		// (d) アクティブフェーズにある場合、タイムアウトを確認する。
		if state.Phase != "" {
			elapsed := now.Sub(item.UpdatedAt)
			if elapsed > cfg.activePhaseTimeout {
				if err := client.RemoveLabel(ctx, repo, item.Number, state.Phase); err != nil {
					return result, fmt.Errorf("cycle: remove timed out label %s on %s#%d: %w", state.Phase, repo, item.Number, err)
				}
				if err := client.AddLabel(ctx, repo, item.Number, LabelTriage); err != nil {
					return result, fmt.Errorf("cycle: escalate %s#%d to triage: %w", repo, item.Number, err)
				}
				logger.Warn("active phase timed out, escalated to triage",
					"repo", repo, "issue_number", item.Number, "label", state.Phase,
					"elapsed", elapsed.String(), "action", ActionEscalateTriage)
				result.Action = ActionEscalateTriage
				result.ItemKind = string(item.Kind)
				result.ItemNumber = item.Number
				result.Label = state.Phase
				return result, nil
			}

			// LLM を要するフェーズに到達した。Phase 3 で executeLLMPhase の中身を
			// internal/runner の呼び出しに差し替えるまでは、対象を特定してログに
			// 出力するのみで実行はしない。
			executeLLMPhase(logger, repo, item, state.Phase)
			result.Action = ActionLLMPhasePending
			result.ItemKind = string(item.Kind)
			result.ItemNumber = item.Number
			result.Label = state.Phase
			return result, nil
		}

		// ここに到達するのは、例えば agent: ラベルの無い PR など、
		// このサイクルでは行動の必要が無いアイテムである。
		logger.Debug("no actionable state for item",
			"repo", repo, "issue_number", item.Number, "kind", string(item.Kind))
	}

	logger.Info("no item required action this cycle", "repo", repo, "action", ActionNoop)
	return result, nil
}

// shouldClearWait は comments の中で最も新しいものが、autopilot 自身
// （botLogin と一致する、または type が Bot）以外による投稿であれば true を返す。
// コメントが 1 件も無い場合は false（解除しない）。
func shouldClearWait(comments []github.Comment, botLogin string) bool {
	var latest *github.Comment
	for i := range comments {
		c := &comments[i]
		if latest == nil || c.CreatedAt.After(latest.CreatedAt) {
			latest = c
		}
	}
	if latest == nil {
		return false
	}
	if latest.User.Login == botLogin || latest.User.Type == "Bot" {
		return false
	}
	return true
}

// executeLLMPhase は LLM CLI (claude/agy) の起動を担う関数境界である。
//
// Phase 2 の時点では何も実行せず、「このフェーズを実行する対象である」ことを
// 構造化ログに出力するのみとする。Phase 3 でこの関数の中身を
// internal/prompt・internal/runner の呼び出しに差し替える。
func executeLLMPhase(logger *slog.Logger, repo string, item Item, phase string) {
	logger.Info("llm phase target identified (not executed: phase 3 not implemented yet)",
		"repo", repo,
		"issue_number", item.Number,
		"kind", string(item.Kind),
		"label", phase,
		"action", ActionLLMPhasePending,
	)
}
