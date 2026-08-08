// Package engine は internal/store の未処理イベントを 1 件ずつ取り出し、
// 遷移表（transition.go）に従って LLM CLI を起動する
// （DESIGN.md 8章）。internal/daemon.Worker を実装する。
//
// Go が決めるのは「起こすか否か」だけである。何をするかはエージェント自身が
// 判断し、結果は NUAGE_REPORT_FILE 経由の極小な機械チャネル（outcome とサブ
// Issue 一覧のみ）で受け取る。人間向けの説明は GitHub 側にエージェント自身が
// 直接書き込む（DESIGN.md 8.2 節）。
package engine

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"time"

	"autopilot/internal/agentcli"
	"autopilot/internal/daemon"
	"autopilot/internal/github"
	"autopilot/internal/prompt"
	"autopilot/internal/repo"
	"autopilot/internal/store"
)

const (
	defaultLeaseTTL     = 130 * time.Minute
	defaultAgentTimeout = 120 * time.Minute
	defaultMaxCostUSD   = 5.0
	defaultMaxRuns      = 16

	// verifyTimeout は verify 1 回の実行に許す最大時間である。verify は実装を伴わない
	// 読み取り専用の検証であり、エージェント本体ほどの時間を必要としないため、
	// AgentTimeout とは別に短く固定する。
	verifyTimeout = 30 * time.Minute
)

// errLeaseHeld は、対象アイテムのリースを他の保持者が握っているため今回は
// 処理できなかったことを表す番兵エラーである。
//
// これを通常のエラーと区別するのは、イベントを処理済みにしてよいかが変わるためである。
// プロセスがクラッシュすると解放されなかったリースが TTL 切れまで残る。その間に
// 届いたイベントを処理済みにしてしまうと、作業が恒久的に失われる（DESIGN.md 11章）。
var errLeaseHeld = errors.New("lease is held by another holder")

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
	// （DESIGN.md 5章「ハング検知」の最内層）。
	AgentTimeout time.Duration

	// MaxCostUSD / MaxRuns は 1 アイテムあたりの予算上限である
	// （DESIGN.md 10章。既定 $5 / 16 runs）。
	MaxCostUSD float64
	MaxRuns    int

	// AgentCLI / VerifyCLI は各役割が使う LLM CLI である。実装と検証で別の CLI・
	// 別のモデルを使えるように分けてある（DESIGN.md 4章）。
	// VerifyCLI が nil の場合は AgentCLI をそのまま使う。
	AgentCLI  agentcli.Client
	VerifyCLI agentcli.Client

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
	// verify 用の CLI が指定されていなければ実装側と同じものを使う。
	if c.VerifyCLI == nil {
		c.VerifyCLI = c.AgentCLI
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
// イベントの処理中に発生したエラー（GitHub 呼び出し失敗、CLI の起動失敗等）は
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
		if errors.Is(err, errLeaseHeld) {
			// イベントを未処理のまま残し、次の起床で再試行する。processed=false を
			// 返すことで worker ループはタイマー待ちに入り、忙しくループしない。
			// リースは TTL で必ず失効するため、この待ちは有限である。
			e.logger().Warn("lease is held; leaving the event unprocessed for a later retry",
				"event_id", ev.ID, "repo", item.Repo, "number", item.Number)
			return false, nil
		}
		e.logger().Error("failed to handle event",
			"event_id", ev.ID, "item_id", ev.ItemID, "repo", item.Repo, "number", item.Number, "error", err.Error())
	}

	if err := e.cfg.Store.MarkEventProcessed(ctx, ev.ID); err != nil {
		return false, fmt.Errorf("engine: mark event %d processed: %w", ev.ID, err)
	}
	return true, nil
}

// isHumanEventType は ev.Type が人間の行動に由来するイベントかどうかを返す。
// internal/ingest は commented/reviewed を enqueue する前に自分自身・他 Bot の
// 投稿を除外し、opened については自分自身が起票したものを除外している
// （DESIGN.md 7.3 節）ため、ここでの判定はイベント種別の分類のみでよい。
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

	case actionLaunchVerify:
		return e.launchVerify(ctx, item, ev)

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

// launchAgent は予算とリースを確認した上で実装エージェントを起動する。
func (e *Engine) launchAgent(ctx context.Context, item store.Item, ev store.Event, newSession bool) error {
	return e.withBudgetAndLease(ctx, item, ev, e.cfg.AgentTimeout, func(runCtx context.Context, it store.Item) error {
		return e.runAgent(runCtx, it, ev, newSession)
	})
}

// launchVerify は予算とリースを確認した上で verify を起動する。
//
// verify の実行も予算（CostUSD / Runs）に加算する。「1 アイテムに費やしてよい総額」を
// 一本で管理する DESIGN.md 10章の思想を崩さないためであり、実装と検証が延々と往復した
// 場合の打ち止めもこの一本の上限が担う。
func (e *Engine) launchVerify(ctx context.Context, item store.Item, ev store.Event) error {
	return e.withBudgetAndLease(ctx, item, ev, verifyTimeout, func(runCtx context.Context, it store.Item) error {
		return e.runVerify(runCtx, it, ev)
	})
}

// withBudgetAndLease は予算とリースを確認した上で fn を実行する、agent と verify に
// 共通の前処理である。リースを取得できなかった場合は errLeaseHeld を返す。
func (e *Engine) withBudgetAndLease(ctx context.Context, item store.Item, ev store.Event, timeout time.Duration, fn func(context.Context, store.Item) error) error {
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
		// worker は直列実行のため、同一プロセス内では起こらない。到達するのは
		// 前のプロセスがクラッシュしてリースが残っている場合であり、そのときは
		// イベントを消費せずに再試行させる必要がある（errLeaseHeld の説明を参照）。
		return errLeaseHeld
	}
	defer func() {
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		if err := e.cfg.Store.ReleaseLease(cleanupCtx, item.ID, e.cfg.Holder); err != nil {
			e.logger().Error("failed to release lease", "repo", item.Repo, "number", item.Number, "error", err.Error())
		}
	}()

	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	return fn(runCtx, item)
}

// runAgent は実装エージェントを起動し、結果を適用する。
func (e *Engine) runAgent(ctx context.Context, item store.Item, ev store.Event, newSession bool) error {
	detail, err := fetchDetail(ctx, e.cfg.Client, item.Repo, item.Kind, item.Number)
	if err != nil {
		return e.reportBlocked(ctx, item, fmt.Sprintf("GitHub からの情報取得に失敗した: %s", err.Error()))
	}

	promptText := prompt.BuildAgent(promptContext(item, ev, detail, newSession))

	run, err := e.runCLI(ctx, item, e.cfg.AgentCLI, promptText, item.SessionID)
	if err != nil {
		return e.reportBlocked(ctx, item, fmt.Sprintf("エージェントの実行に失敗した: %s", err.Error()))
	}
	defer run.cleanup()

	if run.result.SessionID != "" && run.result.SessionID != item.SessionID {
		if err := e.cfg.Store.UpdateItemSessionID(ctx, item.ID, run.result.SessionID); err != nil {
			e.logger().Error("failed to persist session id", "repo", item.Repo, "number", item.Number, "error", err.Error())
		}
	}

	res, readErr := readAgentResult(run.reportPath)
	if readErr != nil {
		return e.reportBlocked(ctx, item, fmt.Sprintf("エージェントが有効な結果を報告しなかった（exit_code=%d, 読み取りエラー: %s）",
			run.result.ExitCode, readErr.Error()))
	}

	return e.applyOutcome(ctx, item, res)
}

// runVerify は verify セッションを起動し、判定を適用する。
//
// runAgent との違いは次の 2 点だけであり、いずれも verify を実装エージェントから
// 独立させておくためである。
//   - セッションを --resume しない。実装したエージェントの思考過程を引き継ぐと、
//     自分の実装を自分でレビューする形に退化する
//   - 返ってきた session_id を保存しない。verify は毎回まっさらな状態から検証する
func (e *Engine) runVerify(ctx context.Context, item store.Item, ev store.Event) error {
	detail, err := fetchDetail(ctx, e.cfg.Client, item.Repo, item.Kind, item.Number)
	if err != nil {
		return e.reportVerifyUnavailable(ctx, item, fmt.Sprintf("GitHub からの情報取得に失敗した: %s", err.Error()))
	}

	promptText := prompt.BuildVerify(promptContext(item, ev, detail, true))

	run, err := e.runCLI(ctx, item, e.cfg.VerifyCLI, promptText, "")
	if err != nil {
		return e.reportVerifyUnavailable(ctx, item, fmt.Sprintf("verify を起動できなかった: %s", err.Error()))
	}
	defer run.cleanup()

	res, readErr := readVerifyResult(run.reportPath)
	if readErr != nil {
		return e.reportVerifyUnavailable(ctx, item, fmt.Sprintf("verify が有効な結果を報告しなかった（exit_code=%d, 読み取りエラー: %s）",
			run.result.ExitCode, readErr.Error()))
	}

	return e.applyVerifyOutcome(ctx, item, res)
}

// reportVerifyUnavailable は verify そのものを実行できなかった場合の共通処理である。
//
// agent 側の同種の失敗（reportBlocked）と違い、blocked にせず ready へ進める。
// verify を有効にしたせいで、無効なら ready に進んでいたはずの PR が詰まる状況を
// 作らないためである。エージェントが判定を出せなかった verify_inconclusive と
// 実質的に同じ扱いであり、「判定が無い」を「不合格」に読み替えない
// （DESIGN.md 8.4 節）。
//
// 判定が無いまま ready に進む経路が存在するため、将来 auto-merge を実装する場合は
// `ready` を「検証済み」と解釈してはならない。明示的な verify_passed を根拠にすること。
func (e *Engine) reportVerifyUnavailable(ctx context.Context, item store.Item, message string) error {
	e.logger().Error("verify could not produce a verdict; moving to ready without verification",
		"repo", item.Repo, "number", item.Number, "reason", message)
	if err := e.cfg.Client.CreateComment(ctx, item.Repo, item.Number, "verify を実行できなかったため、検証されないまま次に進む。"+message); err != nil {
		e.logger().Error("failed to post verify-unavailable comment", "repo", item.Repo, "number", item.Number, "error", err.Error())
	}
	return e.cfg.Store.UpdateItemPhase(ctx, item.ID, store.PhaseReady)
}

// cliRun は CLI 1 回の実行結果と、後始末である。
type cliRun struct {
	result     agentcli.Result
	reportPath string
	cleanup    func()
}

// runCLI は agent と verify に共通する起動手順（作業ディレクトリの用意 →
// report ファイルの作成 → CLI の起動 → 実測コストの計上）をまとめたものである。
//
// この関数は役割も CLI の種類も知らない。差はプロンプト・使う Client・resumeID の
// 有無だけであり、それらは呼び出し側が決める。
func (e *Engine) runCLI(ctx context.Context, item store.Item, cli agentcli.Client, promptText, resumeID string) (cliRun, error) {
	workDir, err := repo.EnsureWorkspace(ctx, e.logger(), e.cfg.StateDir, item.Repo, e.cfg.Repos, e.cfg.RepoOptions...)
	if err != nil {
		return cliRun{}, fmt.Errorf("作業ディレクトリの準備: %w", err)
	}

	reportFile, err := os.CreateTemp(e.cfg.StateDir, "nuage-report-*.json")
	if err != nil {
		return cliRun{}, fmt.Errorf("create report file: %w", err)
	}
	reportPath := reportFile.Name()
	_ = reportFile.Close()
	cleanup := func() { os.Remove(reportPath) }

	res, runErr := cli.Run(ctx, agentcli.Request{
		WorkDir:  workDir,
		Prompt:   promptText,
		ResumeID: resumeID,
		Env:      []string{"NUAGE_REPORT_FILE=" + reportPath},
	})
	if runErr != nil {
		cleanup()
		return cliRun{}, fmt.Errorf("%s の起動: %w", cli.Name(), runErr)
	}

	if res.CostUSD == nil {
		// 金額を返さない CLI がある。実行回数（MaxRuns）だけが歯止めになる
		// （DESIGN.md 10章）。
		e.logger().Warn("cli did not report a cost; only the run count limits this item",
			"cli", cli.Name(), "repo", item.Repo, "number", item.Number)
	} else if err := e.cfg.Store.AddItemUsage(ctx, item.ID, *res.CostUSD); err != nil {
		e.logger().Error("failed to record usage", "repo", item.Repo, "number", item.Number, "error", err.Error())
	}

	return cliRun{result: res, reportPath: reportPath, cleanup: cleanup}, nil
}

// promptContext は agent / verify 共通のプロンプト入力を組み立てる。
func promptContext(item store.Item, ev store.Event, detail itemDetail, newSession bool) prompt.Context {
	return prompt.Context{
		RepoName: item.Repo,
		Kind:     prompt.Kind(item.Kind),
		Number:   item.Number,
		Title:    detail.Title,
		Body:     detail.Body,
		HeadSHA:  item.HeadSHA,
		Event: prompt.EventInfo{
			Type:      ev.Type,
			Actor:     ev.Actor,
			Body:      ev.Body,
			CreatedAt: ev.CreatedAt,
		},
		NewSession: newSession,
	}
}

// verifyFailureNote は verify_failure イベントの body である。
//
// 差し戻し理由そのものは機械チャネルに載せず、PR コメント（人間チャネル）に置く
// （DESIGN.md 8.2 節）。イベントはその所在を指すだけに留める。
const verifyFailureNote = `verify が検証を行い、不合格と判断した。PR に投稿された verify のコメントを読み、
指摘に対応すること。対応後に push すれば CI と verify が再度走る。`

// applyVerifyOutcome は verify の判定を適用する（DESIGN.md 8.3 節の outcome 表）。
//
// verify_failed 以外（verify_passed / verify_inconclusive）はいずれも ready に進める。
// inconclusive を止めないのは、検証手段が無い・プレビューに到達できないといった
// 「検証できなかった」を理由に PR を詰まらせないためである。
func (e *Engine) applyVerifyOutcome(ctx context.Context, item store.Item, res agentResult) error {
	if res.Outcome != OutcomeVerifyFailed {
		return e.cfg.Store.UpdateItemPhase(ctx, item.ID, store.PhaseReady)
	}

	// 差し戻しは phase を止めるのではなく、agent を起こし直すイベントとして表現する。
	// これにより「1 イベント = 高々 1 回の LLM 起動」という不変条件が保たれ、予算も
	// 起動ごとに正しく積算される（markDone が child_done を積むのと同じ手法である）。
	//
	// dedup_key に head_sha を含めるのは、プロセスがクラッシュして ci_success が
	// 再処理された場合に verify_failure が二重に積まれないようにするためである。
	// 同じコミットに対する verify の結論は同じであり、2 度目は積む必要が無い。
	dedupKey := fmt.Sprintf("verify_failure:%s#%d:%s", item.Repo, item.Number, item.HeadSHA)
	if _, _, err := e.cfg.Store.EnqueueEvent(ctx, dedupKey, item.ID, "verify_failure", "nuage-autopilot", verifyFailureNote, time.Now()); err != nil {
		return fmt.Errorf("enqueue verify_failure: %w", err)
	}
	// phase は in_review のまま据え置く。次の tick で verify_failure を拾い、
	// agent が --resume で起動される。
	return nil
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

// reportBlocked は CLI 自体を起動できなかった、または有効な結果を確定できな
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
