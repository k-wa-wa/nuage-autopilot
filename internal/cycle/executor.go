package cycle

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"sort"
	"strings"

	"autopilot/internal/github"
	"autopilot/internal/prompt"
	"autopilot/internal/repo"
	"autopilot/internal/report"
	"autopilot/internal/runner"
)

// reportFileEnvVar は worker (claude) に結果の書き出し先を伝える環境変数名である。
// internal/prompt の reportNote がこの変数名を worker への指示文に埋め込む前提とは
// せず、worker は単に「環境変数 NUAGE_REPORT_FILE」という指示を読んで書き出す
// （プロンプト側の文言と実装側の定数がズレないよう、値そのものはここでのみ定義する）。
const reportFileEnvVar = "NUAGE_REPORT_FILE"

// LLMExecutor は、agent:running を付与された 1 件の Issue/PR に対して、選ばれた
// worker (work/verify) を実際に起動する処理の抽象である。Run はこのインターフェース
// 越しにのみ worker を実行するため、cycle パッケージのテストでは実際の
// git/gh/claude を起動しないフェイク実装に差し替えられる。
type LLMExecutor interface {
	// Execute は worker（WorkerWork/WorkerVerify）に応じたプロンプトを組み立て、
	// 対象リポジトリの clone 内で claude を起動し、その結果を GitHub コメントとして
	// 残す。
	//
	// 戻り値の error は「claude 自体を実行できなかった」「実行はしたが有効な結果を
	// 確定できなかった（report ファイル未生成・不正な JSON・その worker にとって
	// 妥当でない status・結果コメントの投稿失敗）」ことを表す。呼び出し元
	// （cycle.Run）はこれを ActionWorkerFailed として扱い、次サイクルでの
	// 再試行対象とする。worker が正常に起動し、有効な結果（done/passed/failed/blocked
	// のいずれか）を報告してそれをコメントとして投稿できた場合は、その結果の
	// 中身が failed/blocked であっても nil を返す
	// （nuage-autopilot 自体は正常に 1 サイクルを完了しているため）。
	Execute(ctx context.Context, repoName string, item Item, worker string) error
}

// DefaultLLMExecutor は本番で使用する LLMExecutor の実装である。
// 対象リポジトリの clone/更新（internal/repo）、プロンプトの組み立て
// （internal/prompt）、claude CLI の起動（internal/runner）、worker が残した
// report ファイル（internal/report）の読み取りと結果コメントの投稿までを
// 一貫して行う。
type DefaultLLMExecutor struct {
	// StateDir はリポジトリの clone を置くディレクトリである（NUAGE_STATE_DIR）。
	// report ファイルの一時置き場としても使う（対象リポジトリの clone 内に
	// 置くと、worker が誤ってコミットしたり、次サイクルの git clean -fd で
	// 消えたりする可能性があるため、あえて clone の外に置く）。
	StateDir string

	// Repos は StateDir 配下に事前 clone / 最新化しておくべき全リポジトリ一覧である。
	Repos []string

	// Client は結果コメントの投稿、agent:awaiting_user_review の付与、
	// worker 実行後の PR head SHA の再取得に使う。
	Client *github.Client

	// Logger は clone/claude 実行のログ出力先。nil の場合 slog.Default() を使う。
	Logger *slog.Logger
}

// Execute は DefaultLLMExecutor の LLMExecutor 実装である。
func (e *DefaultLLMExecutor) Execute(ctx context.Context, repoName string, item Item, worker string) error {
	logger := e.Logger
	if logger == nil {
		logger = slog.Default()
	}

	workDir, err := repo.EnsureWorkspace(ctx, logger, e.StateDir, repoName, e.Repos)
	if err != nil {
		return fmt.Errorf("executor: ensure workspace for %s: %w", repoName, err)
	}

	comments, err := e.Client.ListComments(ctx, repoName, item.Number, item.Kind == kindPullRequest)
	if err != nil {
		logger.Warn("failed to list comments for prompt context; proceeding without it",
			"repo", repoName, "number", item.Number, "error", err.Error())
		comments = nil
	}
	botLogin, err := e.Client.CurrentUser(ctx)
	if err != nil {
		logger.Warn("failed to resolve current user for prompt context; proceeding without it",
			"repo", repoName, "error", err.Error())
		botLogin = ""
	}
	verifyFailureSummary, humanComments := extractPromptContext(comments, botLogin)

	promptCtx := prompt.Context{
		RepoName:             repoName,
		Kind:                 prompt.Kind(item.Kind),
		Number:               item.Number,
		Title:                item.Title,
		Body:                 item.Body,
		VerifyFailureSummary: verifyFailureSummary,
		HumanComments:        humanComments,
	}
	promptText, err := buildPromptForWorker(promptCtx, worker)
	if err != nil {
		return fmt.Errorf("executor: build prompt: %w", err)
	}

	reportFile, err := os.CreateTemp(e.StateDir, "nuage-report-*.json")
	if err != nil {
		return fmt.Errorf("executor: create report file: %w", err)
	}
	reportPath := reportFile.Name()
	_ = reportFile.Close()
	defer os.Remove(reportPath)

	result, runErr := runner.Run(ctx, runner.Options{
		WorkDir:  workDir,
		Prompt:   promptText,
		ExtraEnv: []string{reportFileEnvVar + "=" + reportPath},
		Logger:   logger,
	})

	return e.finish(ctx, logger, repoName, item, worker, reportPath, result, runErr)
}

// finish は claude の実行結果（またはその起動失敗）を受けて、worker が残した
// report ファイルを読み、結果コメントの投稿と（必要なら）
// agent:awaiting_user_review の付与を行う。
func (e *DefaultLLMExecutor) finish(ctx context.Context, logger *slog.Logger, repoName string, item Item, worker, reportPath string, result runner.Result, runErr error) error {
	if runErr != nil {
		return e.reportBlocked(ctx, logger, repoName, item, worker,
			fmt.Sprintf("claude の起動に失敗した: %s", runErr.Error()))
	}

	res, readErr := report.ReadResultFile(reportPath)
	switch {
	case readErr != nil:
		return e.reportBlocked(ctx, logger, repoName, item, worker,
			fmt.Sprintf("worker が有効な結果を報告しなかった（exit_code=%d, report読み取りエラー: %s）", result.ExitCode, readErr.Error()))
	case !report.ValidStatus(worker, res.Status):
		return e.reportBlocked(ctx, logger, repoName, item, worker,
			fmt.Sprintf("worker=%s にとって不正な status %q が報告された（exit_code=%d）", worker, res.Status, result.ExitCode))
	}

	sha := e.refreshHeadSHA(ctx, logger, repoName, item)
	body := report.Render(worker, res.Status, sha, res.Summary)
	if err := e.Client.CreateComment(ctx, repoName, item.Number, body); err != nil {
		return fmt.Errorf("executor: post result comment: %w", err)
	}

	if res.Status == report.StatusBlocked {
		if err := e.Client.AddLabel(ctx, repoName, item.Number, LabelAwaitingUserReview); err != nil {
			logger.Error("failed to add agent:awaiting_user_review after a blocked report",
				"repo", repoName, "number", item.Number, "error", err.Error())
		}
	}

	return nil
}

// reportBlocked は「claude 自体を実行できなかった」「有効な結果を確定できなかった」
// 場合の共通処理である。Go 自身が status="blocked" の状態行コメントを合成して
// 投稿し、agent:awaiting_user_review を付与したうえでエラーを返す。
//
// summary はここで既にエラーとして確定しているため、コメント投稿自体が失敗しても
// ログに残すのみで（呼び出し元へは既存のエラーをそのまま返す）、二重にエラーを
// 上書きしない。
func (e *DefaultLLMExecutor) reportBlocked(ctx context.Context, logger *slog.Logger, repoName string, item Item, worker, summary string) error {
	sha := e.refreshHeadSHA(ctx, logger, repoName, item)
	body := report.Render(worker, report.StatusBlocked, sha, summary)
	if err := e.Client.CreateComment(ctx, repoName, item.Number, body); err != nil {
		logger.Error("failed to post fallback blocked comment", "repo", repoName, "number", item.Number, "error", err.Error())
	}
	if err := e.Client.AddLabel(ctx, repoName, item.Number, LabelAwaitingUserReview); err != nil {
		logger.Error("failed to add agent:awaiting_user_review after worker failure", "repo", repoName, "number", item.Number, "error", err.Error())
	}
	return errors.New("executor: " + summary)
}

// refreshHeadSHA は item が PR の場合、worker 実行後の権威ある head SHA を GitHub から
// 再取得する。Issue の場合は状態行に sha を含めないため常に空文字列を返す。
// 取得に失敗した場合は、実行開始時点で分かっていた item.HeadSHA にフォールバックする
// （空文字列よりは、多少古くても値がある方が stale 判定を機能させられる）。
func (e *DefaultLLMExecutor) refreshHeadSHA(ctx context.Context, logger *slog.Logger, repoName string, item Item) string {
	if item.Kind != kindPullRequest {
		return ""
	}
	pr, err := e.Client.GetPullRequest(ctx, repoName, item.Number)
	if err != nil {
		logger.Warn("failed to refresh pr head sha; falling back to the pre-run value",
			"repo", repoName, "number", item.Number, "error", err.Error())
		return item.HeadSHA
	}
	return pr.HeadSHA
}

// buildPromptForWorker は worker（WorkerWork/WorkerVerify）に応じた internal/prompt の
// Build 関数を呼び出す。
func buildPromptForWorker(ctx prompt.Context, worker string) (string, error) {
	switch worker {
	case WorkerWork:
		return prompt.BuildWork(ctx), nil
	case WorkerVerify:
		if ctx.Kind != prompt.KindPullRequest {
			return "", fmt.Errorf("worker %q is only valid for pull requests, got kind %q", worker, ctx.Kind)
		}
		return prompt.BuildVerify(ctx), nil
	default:
		return "", fmt.Errorf("unknown worker %q", worker)
	}
}

// extractPromptContext は comments から、work worker に注入する
// 「直近の verify 不合格の詳細」と「最後の状態行より新しい人間コメント」を抽出する。
//
// 状態行の投稿者は botLogin と一致するものだけを信頼する（人間が偶然似た文面を
// 書いた場合と区別するため。internal/cycle/transition.go の deriveState と同じ考え方）。
func extractPromptContext(comments []github.Comment, botLogin string) (verifyFailureSummary string, humanComments []prompt.HumanComment) {
	sorted := make([]github.Comment, len(comments))
	copy(sorted, comments)
	sort.Slice(sorted, func(i, j int) bool {
		return sorted[i].CreatedAt.After(sorted[j].CreatedAt) // 新しい順
	})

	latestStatusLineIdx := -1
	for i, c := range sorted {
		if !isOwnComment(c, botLogin) {
			continue
		}
		sl, ok := report.Parse(c.Body)
		if !ok {
			continue
		}
		if latestStatusLineIdx == -1 {
			latestStatusLineIdx = i
		}
		if verifyFailureSummary == "" && sl.Worker == WorkerVerify && sl.Status == report.StatusFailed {
			verifyFailureSummary = stripStatusLine(c.Body)
		}
	}

	// 状態行が 1 件も無ければ、履歴中のすべてのコメントが「状態行より新しい」扱いになる。
	cutoff := len(sorted)
	if latestStatusLineIdx != -1 {
		cutoff = latestStatusLineIdx
	}

	newestFirst := make([]prompt.HumanComment, 0, cutoff)
	for i := 0; i < cutoff; i++ {
		if isHumanComment(sorted[i], botLogin) {
			newestFirst = append(newestFirst, prompt.HumanComment{
				Author:    sorted[i].User.Login,
				CreatedAt: sorted[i].CreatedAt,
				Body:      sorted[i].Body,
			})
		}
	}
	for i, j := 0, len(newestFirst)-1; i < j; i, j = i+1, j-1 {
		newestFirst[i], newestFirst[j] = newestFirst[j], newestFirst[i]
	}
	return verifyFailureSummary, newestFirst
}

// stripStatusLine は状態行付きコメントの本文から状態行そのものを取り除き、
// 散文部分（summary）だけを返す。
func stripStatusLine(body string) string {
	if i := strings.IndexByte(body, '\n'); i >= 0 {
		return strings.TrimSpace(body[i+1:])
	}
	return ""
}
