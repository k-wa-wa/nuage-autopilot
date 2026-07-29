// Package engine は internal/store の未処理イベントを 1 件ずつ取り出し、
// 遷移表（transition.go）に従ってエージェント（claude）を起動する
// （DESIGN.md 8章）。internal/daemon.Worker を実装する。
//
// Go が決めるのは「起こすか否か」だけである。何をするかはエージェント自身が
// 判断し、結果は NUAGE_REPORT_FILE 経由の極小な機械チャネル（outcome とサブ
// Issue 一覧のみ）で受け取る。人間向けの説明は GitHub 側にエージェント自身が
// 直接書き込む（DESIGN.md 8.2 節）。
package engine

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"time"

	"autopilot/internal/daemon"
	"autopilot/internal/github"
	"autopilot/internal/prompt"
	"autopilot/internal/repo"
	"autopilot/internal/runner"
	"autopilot/internal/store"
)

const (
	defaultLeaseTTL     = 130 * time.Minute
	defaultAgentTimeout = 120 * time.Minute
	defaultMaxCostUSD   = 5.0
	defaultMaxRuns      = 10
)

// Config は Engine の生成に必要な設定である。
type Config struct {
	Store  *store.Store
	Client *github.Client

	// StateDir はリポジトリの clone、および NUAGE_REPORT_FILE の一時置き場である。
	// report ファイルを clone の外に置くのは、エージェントが誤ってコミットしたり、
	// 次回の clone 更新で消えたりすることを避けるためである。
	StateDir string

	// Repos は StateDir 配下に事前 clone / 最新化しておくべき全リポジトリ一覧である
	// （internal/repo.EnsureWorkspace の allRepos）。
	Repos []string

	// Holder はリースの保持者を識別する文字列（"host:pid" 形式を想定）。
	// 空の場合は自動的に決定する。
	Holder string

	// LeaseTTL はリースの有効期間である。AgentTimeout より長く取る必要がある
	// （DESIGN.md 11章）。既定 130 分。
	LeaseTTL time.Duration

	// AgentTimeout はエージェント 1 回の実行に許す最大時間である。既定 120 分
	// （DESIGN.md 5章 レイヤ 1）。
	AgentTimeout time.Duration

	// MaxCostUSD / MaxRuns は 1 アイテムあたりの予算上限である
	// （DESIGN.md 10章。既定 $5 / 10 runs）。
	MaxCostUSD float64
	MaxRuns    int

	// AgentCommand は claude の実行ファイル名/パスの上書きである。空の場合
	// internal/runner の既定（"claude"）を使う。テスト用。
	AgentCommand string

	// RepoOptions は internal/repo.EnsureWorkspace にそのまま渡す追加オプションである。
	// 本番では空でよい（既定の GitHub リモート・git/gh コマンドを使う）。テストで
	// ローカルの bare リポジトリやフェイクの git/gh 実行系に差し替えるために公開している。
	RepoOptions []repo.Option

	Logger *slog.Logger
}

func (c Config) withDefaults() Config {
	if c.LeaseTTL <= 0 {
		c.LeaseTTL = defaultLeaseTTL
	}
	if c.AgentTimeout <= 0 {
		c.AgentTimeout = defaultAgentTimeout
	}
	if c.MaxCostUSD <= 0 {
		c.MaxCostUSD = defaultMaxCostUSD
	}
	if c.MaxRuns <= 0 {
		c.MaxRuns = defaultMaxRuns
	}
	if c.Holder == "" {
		host, err := os.Hostname()
		if err != nil {
			host = "unknown-host"
		}
		c.Holder = fmt.Sprintf("%s:%d", host, os.Getpid())
	}
	if c.Logger == nil {
		c.Logger = slog.Default()
	}
	return c
}

// Engine は internal/daemon.Worker の実装である。
type Engine struct {
	cfg Config
}

var _ daemon.Worker = (*Engine)(nil)

// New は Engine を生成する。
func New(cfg Config) *Engine {
	return &Engine{cfg: cfg.withDefaults()}
}

func (e *Engine) logger() *slog.Logger { return e.cfg.Logger }

// ProcessNext は internal/daemon.Worker の実装である。未処理イベントを高々 1 件
// 処理する。
//
// イベントの処理中に発生したエラー（GitHub 呼び出し失敗、claude の起動失敗等）は
// 可能な限り handle 内部で吸収し、"blocked" コメントの投稿と phase=blocked への
// 遷移という形で GitHub 側に記録する。ここで拾うのは、その吸収すら失敗した
// 場合のログ用途であり、いずれにせよイベントは処理済みにする。1 件のイベントに
// 固執して他アイテムの処理を止めないためである。
func (e *Engine) ProcessNext(ctx context.Context) (bool, error) {
	ev, ok, err := e.cfg.Store.NextUnprocessedEvent(ctx)
	if err != nil {
		return false, fmt.Errorf("engine: get next unprocessed event: %w", err)
	}
	if !ok {
		return false, nil
	}

	item, ok, err := e.cfg.Store.GetItemByID(ctx, ev.ItemID)
	if err != nil {
		return false, fmt.Errorf("engine: get item %d: %w", ev.ItemID, err)
	}
	if !ok {
		e.logger().Error("event references a missing item; discarding", "event_id", ev.ID, "item_id", ev.ItemID)
	} else if err := e.handle(ctx, item, ev); err != nil {
		e.logger().Error("failed to handle event",
			"event_id", ev.ID, "item_id", ev.ItemID, "repo", item.Repo, "number", item.Number, "error", err.Error())
	}

	if err := e.cfg.Store.MarkEventProcessed(ctx, ev.ID); err != nil {
		return false, fmt.Errorf("engine: mark event %d processed: %w", ev.ID, err)
	}
	return true, nil
}

// isHumanEventType は ev.Type が人間の行動に由来するイベントかどうかを返す。
// internal/ingest は commented/reviewed/opened を enqueue する前に既に
// 自分自身・他 Bot の投稿を除外している（DESIGN.md 7.3 節）ため、ここでの
// 判定はイベント種別の分類のみでよい。
func isHumanEventType(eventType string) bool {
	switch eventType {
	case "opened", "commented", "reviewed":
		return true
	default:
		return false
	}
}

func (e *Engine) handle(ctx context.Context, item store.Item, ev store.Event) error {
	act := nextAction(item.Phase, ev.Type)

	switch act {
	case actionIgnore:
		return nil

	case actionToDone:
		return e.markDone(ctx, item)

	case actionToReady:
		return e.cfg.Store.UpdateItemPhase(ctx, item.ID, store.PhaseReady)

	case actionToInReviewAndLaunch:
		if err := e.cfg.Store.UpdateItemPhase(ctx, item.ID, store.PhaseInReview); err != nil {
			return fmt.Errorf("transition to in_review: %w", err)
		}
		item.Phase = store.PhaseInReview
		return e.launchAgent(ctx, item, ev, false)

	case actionLaunchNew:
		return e.launchAgent(ctx, item, ev, true)

	case actionLaunchResume:
		return e.launchAgent(ctx, item, ev, false)
	}

	return nil
}

// markDone は item を done にし、親が居ればサブ Issue 分割の完了判定を行う
// （DESIGN.md 9章）。
func (e *Engine) markDone(ctx context.Context, item store.Item) error {
	if err := e.cfg.Store.UpdateItemPhase(ctx, item.ID, store.PhaseDone); err != nil {
		return fmt.Errorf("mark done: %w", err)
	}
	if item.ParentID == nil {
		return nil
	}

	siblings, err := e.cfg.Store.ListChildren(ctx, *item.ParentID)
	if err != nil {
		return fmt.Errorf("list children of parent %d: %w", *item.ParentID, err)
	}
	for _, s := range siblings {
		if s.Phase != store.PhaseDone {
			return nil
		}
	}

	// 全子が done になった。親を起こす。dedup_key に「今回完了した子の ID」を
	// 含めることで、親が将来再び分割し、子が再び全完了した場合にも新しい
	// child_done イベントが起こせるようにする（親 ID だけをキーにすると
	// 2 回目以降の分割ラウンドが dedup で握りつぶされてしまう）。
	dedupKey := fmt.Sprintf("child_done:%d:%d", *item.ParentID, item.ID)
	if _, _, err := e.cfg.Store.EnqueueEvent(ctx, dedupKey, *item.ParentID, "child_done", "nuage-autopilot", "", time.Now()); err != nil {
		return fmt.Errorf("enqueue child_done for parent %d: %w", *item.ParentID, err)
	}
	return nil
}

// launchAgent は予算とリースを確認した上でエージェントを起動する。
func (e *Engine) launchAgent(ctx context.Context, item store.Item, ev store.Event, newSession bool) error {
	if isHumanEventType(ev.Type) {
		// 人間の関与が唯一の脱出口である、というモデルを反映する（DESIGN.md 10章）。
		if err := e.cfg.Store.ResetItemBudget(ctx, item.ID); err != nil {
			return fmt.Errorf("reset budget: %w", err)
		}
		item.CostUSD = 0
		item.Runs = 0
	}

	if item.CostUSD >= e.cfg.MaxCostUSD || item.Runs >= e.cfg.MaxRuns {
		e.logger().Warn("budget exceeded; blocking item without launching the agent",
			"repo", item.Repo, "number", item.Number, "cost_usd", item.CostUSD, "runs", item.Runs)
		msg := fmt.Sprintf("予算上限（$%.2f または %d 回）に達したため、これ以上自動実行しない。コメントすると予算がリセットされ再開する。",
			e.cfg.MaxCostUSD, e.cfg.MaxRuns)
		if err := e.cfg.Client.CreateComment(ctx, item.Repo, item.Number, msg); err != nil {
			e.logger().Error("failed to post budget-exceeded comment", "repo", item.Repo, "number", item.Number, "error", err.Error())
		}
		return e.cfg.Store.UpdateItemPhase(ctx, item.ID, store.PhaseBlocked)
	}

	acquired, err := e.cfg.Store.AcquireLease(ctx, item.ID, e.cfg.Holder, e.cfg.LeaseTTL)
	if err != nil {
		return fmt.Errorf("acquire lease: %w", err)
	}
	if !acquired {
		// worker は直列実行のため通常は起こらないが、防御的に扱う。
		e.logger().Warn("lease already held by another holder; skipping this launch",
			"repo", item.Repo, "number", item.Number)
		return nil
	}
	defer func() {
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		if err := e.cfg.Store.ReleaseLease(cleanupCtx, item.ID, e.cfg.Holder); err != nil {
			e.logger().Error("failed to release lease", "repo", item.Repo, "number", item.Number, "error", err.Error())
		}
	}()

	runCtx, cancel := context.WithTimeout(ctx, e.cfg.AgentTimeout)
	defer cancel()

	return e.runAgent(runCtx, item, ev, newSession)
}

// runAgent は実際に claude を起動し、結果を適用する。
func (e *Engine) runAgent(ctx context.Context, item store.Item, ev store.Event, newSession bool) error {
	detail, err := fetchDetail(ctx, e.cfg.Client, item.Repo, item.Kind, item.Number)
	if err != nil {
		return e.reportBlocked(ctx, item, fmt.Sprintf("GitHub からの情報取得に失敗した: %s", err.Error()))
	}

	workDir, err := repo.EnsureWorkspace(ctx, e.logger(), e.cfg.StateDir, item.Repo, e.cfg.Repos, e.cfg.RepoOptions...)
	if err != nil {
		return e.reportBlocked(ctx, item, fmt.Sprintf("作業ディレクトリの準備に失敗した: %s", err.Error()))
	}

	promptText := prompt.BuildAgent(prompt.Context{
		RepoName: item.Repo,
		Kind:     prompt.Kind(item.Kind),
		Number:   item.Number,
		Title:    detail.Title,
		Body:     detail.Body,
		Event: prompt.EventInfo{
			Type:      ev.Type,
			Actor:     ev.Actor,
			Body:      ev.Body,
			CreatedAt: ev.CreatedAt,
		},
		NewSession: newSession,
	})

	reportFile, err := os.CreateTemp(e.cfg.StateDir, "nuage-report-*.json")
	if err != nil {
		return fmt.Errorf("create report file: %w", err)
	}
	reportPath := reportFile.Name()
	_ = reportFile.Close()
	defer os.Remove(reportPath)

	opts := runner.Options{
		Command:   e.cfg.AgentCommand,
		WorkDir:   workDir,
		Prompt:    promptText,
		ExtraArgs: []string{"--output-format", "json"},
		ExtraEnv:  []string{"NUAGE_REPORT_FILE=" + reportPath},
		Logger:    e.logger(),
	}
	if item.SessionID != "" {
		opts.ExtraArgs = append(opts.ExtraArgs, "--resume", item.SessionID)
	}

	result, runErr := runner.Run(ctx, opts)
	if runErr != nil {
		return e.reportBlocked(ctx, item, fmt.Sprintf("claude の起動に失敗した: %s", runErr.Error()))
	}

	meta, metaErr := parseClaudeMeta(result.Stdout)
	if metaErr != nil {
		// --output-format json を指定している以上、通常はここに来ない。
		// コスト計上・セッション継続ができないだけで実行自体は成功しているため、
		// ログに残して処理は続ける。
		e.logger().Warn("failed to parse claude's json output; cost and session id were not recorded",
			"repo", item.Repo, "number", item.Number, "error", metaErr.Error())
	}
	if meta.SessionID != "" && meta.SessionID != item.SessionID {
		if err := e.cfg.Store.UpdateItemSessionID(ctx, item.ID, meta.SessionID); err != nil {
			e.logger().Error("failed to persist session id", "repo", item.Repo, "number", item.Number, "error", err.Error())
		}
	}
	if err := e.cfg.Store.AddItemUsage(ctx, item.ID, meta.TotalCostUSD); err != nil {
		e.logger().Error("failed to record usage", "repo", item.Repo, "number", item.Number, "error", err.Error())
	}

	res, readErr := readAgentResult(reportPath)
	if readErr != nil {
		return e.reportBlocked(ctx, item, fmt.Sprintf("エージェントが有効な結果を報告しなかった（exit_code=%d, 読み取りエラー: %s）",
			result.ExitCode, readErr.Error()))
	}

	return e.applyOutcome(ctx, item, res)
}

// applyOutcome はエージェントの報告（outcome）に応じて phase を遷移させる
// （DESIGN.md 8.3 節）。
func (e *Engine) applyOutcome(ctx context.Context, item store.Item, res agentResult) error {
	switch res.Outcome {
	case OutcomeAsked:
		return e.cfg.Store.UpdateItemPhase(ctx, item.ID, store.PhaseAwaitingAnswer)

	case OutcomeImplemented:
		return e.cfg.Store.UpdateItemPhase(ctx, item.ID, store.PhaseInReview)

	case OutcomeBlocked:
		return e.cfg.Store.UpdateItemPhase(ctx, item.ID, store.PhaseBlocked)

	case OutcomeIdle:
		return nil

	case OutcomeSplit:
		return e.applySplit(ctx, item, res.Children)
	}

	return fmt.Errorf("unreachable: unvalidated outcome %q", res.Outcome)
}

// applySplit は子 Issue を登録し、親を delegated にする（DESIGN.md 9章）。
//
// 現時点では children はすべて親と同じリポジトリの Issue であることを前提とする
// （NUAGE_REPORT_FILE のスキーマが番号のみでリポジトリを含まないため）。
// リポジトリを跨ぐ分割は将来 children のスキーマを拡張したときに対応する。
func (e *Engine) applySplit(ctx context.Context, item store.Item, children []int) error {
	for _, num := range children {
		child, _, err := e.cfg.Store.UpsertItem(ctx, item.Repo, num, store.KindIssue)
		if err != nil {
			return fmt.Errorf("register child #%d: %w", num, err)
		}
		if err := e.cfg.Store.SetItemParent(ctx, child.ID, item.ID); err != nil {
			return fmt.Errorf("set parent for child #%d: %w", num, err)
		}
	}
	return e.cfg.Store.UpdateItemPhase(ctx, item.ID, store.PhaseDelegated)
}

// reportBlocked は claude 自体を起動できなかった、または有効な結果を確定できな
// かった場合の共通処理である。GitHub にその旨を投稿し、phase=blocked にする。
// これはエージェントの無言終了に対する唯一の保険であり、通常運転で Go が
// GitHub に書き込むことはない（DESIGN.md 8.3 節）。
func (e *Engine) reportBlocked(ctx context.Context, item store.Item, message string) error {
	if err := e.cfg.Client.CreateComment(ctx, item.Repo, item.Number, message); err != nil {
		e.logger().Error("failed to post fallback blocked comment", "repo", item.Repo, "number", item.Number, "error", err.Error())
	}
	if err := e.cfg.Store.UpdateItemPhase(ctx, item.ID, store.PhaseBlocked); err != nil {
		return fmt.Errorf("set blocked phase: %w", err)
	}
	return nil
}

// itemDetail は現在の Title/Body（GitHub 上の最新状態）である。
type itemDetail struct {
	Title string
	Body  string
}

func fetchDetail(ctx context.Context, client *github.Client, repoName string, kind store.Kind, number int) (itemDetail, error) {
	if kind == store.KindPullRequest {
		pr, err := client.GetPullRequest(ctx, repoName, number)
		if err != nil {
			return itemDetail{}, err
		}
		return itemDetail{Title: pr.Title, Body: pr.Body}, nil
	}

	issue, err := client.GetIssue(ctx, repoName, number)
	if err != nil {
		return itemDetail{}, err
	}
	return itemDetail{Title: issue.Title, Body: issue.Body}, nil
}
