package cycle

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/k-wa-wa/nuage-workspace/autopilot/internal/prompt"
	"github.com/k-wa-wa/nuage-workspace/autopilot/internal/repo"
	"github.com/k-wa-wa/nuage-workspace/autopilot/internal/runner"
)

// LLMExecutor は、agent:running を付与された 1 件の Issue/PR に対して、dispatcher が
// 選んだ worker を実際に起動する処理の抽象である。Run はこのインターフェース越しにのみ
// worker を実行するため、cycle パッケージのテストでは実際の git/gh/claude を起動しない
// フェイク実装に差し替えられる。
type LLMExecutor interface {
	// Execute は worker（WorkerSpec 等）に応じたプロンプトを組み立て、対象リポジトリの
	// clone 内で claude を起動する。戻り値の error は「claude 自体を実行できなかった」
	// （clone 失敗、プロセス起動失敗等）ことを表す。claude が起動できた上でタスクに
	// 失敗した（0 以外の終了コード）場合も、Run 側でリトライ可能な結果として扱うため
	// error として区別せず nil を返してよい実装が標準（DefaultLLMExecutor 参照）。
	Execute(ctx context.Context, repoName string, item Item, worker string) error
}

// DefaultLLMExecutor は本番で使用する LLMExecutor の実装である。
// 対象リポジトリの clone/更新（internal/repo）、プロンプトの組み立て（internal/prompt）、
// claude CLI の起動（internal/runner）を順に行う。
//
// worker は既定モデルで起動する（dispatcher とは異なり --model を指定しない）。
// DESIGN.md 8章「dispatcher の契約」の「モデルは claude-haiku-4-5-20251001 を
// 明示指定する（判断のみで実装を伴わないため）」は dispatcher にのみ適用される。
type DefaultLLMExecutor struct {
	// StateDir はリポジトリの clone を置くディレクトリである（NUAGE_STATE_DIR）。
	StateDir string

	// Logger は clone/claude 実行のログ出力先。nil の場合 slog.Default() を使う。
	Logger *slog.Logger
}

// Execute は DefaultLLMExecutor の LLMExecutor 実装である。
func (e *DefaultLLMExecutor) Execute(ctx context.Context, repoName string, item Item, worker string) error {
	logger := e.Logger
	if logger == nil {
		logger = slog.Default()
	}

	workDir, err := repo.EnsureClone(ctx, logger, e.StateDir, repoName)
	if err != nil {
		return fmt.Errorf("executor: ensure clone of %s: %w", repoName, err)
	}

	promptText, err := buildPromptForWorker(repoName, item, worker)
	if err != nil {
		return fmt.Errorf("executor: build prompt: %w", err)
	}

	result, err := runner.Run(ctx, runner.Options{
		WorkDir: workDir,
		Prompt:  promptText,
		Logger:  logger,
	})
	if err != nil {
		return fmt.Errorf("executor: run claude: %w", err)
	}
	if !result.Success {
		return fmt.Errorf("executor: claude exited with non-zero status %d (duration %s)", result.ExitCode, result.Duration)
	}
	return nil
}

// buildPromptForWorker は worker と item の種類（Issue/PR）に応じて internal/prompt の
// 適切な Build 関数を呼び出す。
func buildPromptForWorker(repoName string, item Item, worker string) (string, error) {
	ctx := prompt.Context{
		RepoName: repoName,
		Kind:     prompt.Kind(item.Kind),
		Number:   item.Number,
		Title:    item.Title,
	}

	switch worker {
	case WorkerSpec:
		if item.Kind != kindIssue {
			return "", fmt.Errorf("worker %q is only valid for issues, got kind %q", worker, item.Kind)
		}
		return prompt.BuildSpec(ctx), nil

	case WorkerDev:
		switch item.Kind {
		case kindIssue:
			return prompt.BuildDevIssue(ctx), nil
		case kindPullRequest:
			return prompt.BuildDevPR(ctx), nil
		default:
			return "", fmt.Errorf("worker %q: unsupported item kind %q", worker, item.Kind)
		}

	case WorkerReview:
		if item.Kind != kindPullRequest {
			return "", fmt.Errorf("worker %q is only valid for pull requests, got kind %q", worker, item.Kind)
		}
		return prompt.BuildReview(ctx), nil

	case WorkerQA:
		if item.Kind != kindPullRequest {
			return "", fmt.Errorf("worker %q is only valid for pull requests, got kind %q", worker, item.Kind)
		}
		return prompt.BuildQA(ctx), nil

	default:
		return "", fmt.Errorf("unknown worker %q", worker)
	}
}
