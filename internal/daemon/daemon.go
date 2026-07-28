// Package daemon は nuage-autopilot の常駐プロセスとしての骨格を持つ。
//
// DESIGN.md 5章の決定に従い、poll/work/resync/watchdog の 4 goroutine を単一プロセス内で
// 動かす。状態は SQLite（internal/store）に持たせるため、常駐してもプロセス自体は
// 無状態のままである。何を poll/process/resync するかはこのパッケージの関心事ではなく、
// Poller/Worker/Resyncer インターフェース経由で外部から注入する
// （Phase 1 の時点では internal/ingest・internal/engine がまだ無いため、
// cmd/nuage-autopilot は何もしない実装を渡し、デーモンが空回りすることを確認する）。
package daemon

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"autopilot/internal/sdnotify"
)

// Poller は GitHub 側の変化を検知し、events に enqueue する処理の抽象である
// （実装は internal/ingest。DESIGN.md 7章）。
type Poller interface {
	// Poll は 1 回分の取り込みを行い、新たに enqueue したイベント数を返す。
	// 0 は「変化が無かった」ことを表す（notifications が 304 を返した場合等）。
	Poll(ctx context.Context) (enqueued int, err error)
}

// PollerFunc は関数を Poller として使うためのアダプタである。
type PollerFunc func(ctx context.Context) (int, error)

func (f PollerFunc) Poll(ctx context.Context) (int, error) { return f(ctx) }

// Worker は未処理イベントを 1 件処理する処理の抽象である（実装は internal/engine）。
type Worker interface {
	// ProcessNext は未処理イベントを高々 1 件処理する。処理すべきものが無かった
	// 場合は processed=false を返す（エラーではない）。
	ProcessNext(ctx context.Context) (processed bool, err error)
}

// WorkerFunc は関数を Worker として使うためのアダプタである。
type WorkerFunc func(ctx context.Context) (bool, error)

func (f WorkerFunc) ProcessNext(ctx context.Context) (bool, error) { return f(ctx) }

// Resyncer は全 open Issue/PR を走査して DB を修復する処理の抽象である
// （実装は internal/ingest。DESIGN.md 7.5 節）。
type Resyncer interface {
	Resync(ctx context.Context) error
}

// ResyncerFunc は関数を Resyncer として使うためのアダプタである。
type ResyncerFunc func(ctx context.Context) error

func (f ResyncerFunc) Resync(ctx context.Context) error { return f(ctx) }

// Config は Run の入力である。
type Config struct {
	Logger *slog.Logger

	Poller   Poller
	Worker   Worker
	Resyncer Resyncer

	// PollInterval は poller の定期起床間隔である。既定 1 分。
	PollInterval time.Duration

	// WorkInterval は worker が wake 通知を受け取れなかった場合に保険として
	// 起きる間隔である。既定 1 分。poller が events を enqueue した直後は
	// この間隔を待たずに wake 通知で即座に起こされる。
	WorkInterval time.Duration

	// ResyncInterval は resyncer の定期起床間隔である。既定 1 時間。
	ResyncInterval time.Duration

	// WatchdogInterval は systemd へ WATCHDOG=1 を送る頻度の上限を決める確認間隔で
	// ある。未指定の場合、$WATCHDOG_USEC（systemd の WatchdogSec= から算出される）が
	// あればその半分、無ければ 30 秒を使う。
	WatchdogInterval time.Duration

	// PollStaleAfter/WorkStaleAfter/ResyncStaleAfter は、各ループが 1 回の処理に
	// これ以上の時間をかけている場合に「ハングしている」とみなし、watchdog による
	// WATCHDOG=1 の送信を止めるための閾値である。
	//
	// WorkStaleAfter は特に注意が必要である。worker は claude の 1 回の実行
	// （既定タイムアウト 120 分。internal/runner が管理する）を待つため、
	// この値は必ずそれより十分大きく取る。既定値は 150 分。
	PollStaleAfter   time.Duration
	WorkStaleAfter   time.Duration
	ResyncStaleAfter time.Duration
}

const (
	defaultPollInterval     = time.Minute
	defaultWorkInterval     = time.Minute
	defaultResyncInterval   = time.Hour
	defaultWatchdogInterval = 30 * time.Second
	defaultWorkStaleAfter   = 150 * time.Minute
)

func (c Config) withDefaults() Config {
	if c.Logger == nil {
		c.Logger = slog.Default()
	}
	if c.PollInterval <= 0 {
		c.PollInterval = defaultPollInterval
	}
	if c.WorkInterval <= 0 {
		c.WorkInterval = defaultWorkInterval
	}
	if c.ResyncInterval <= 0 {
		c.ResyncInterval = defaultResyncInterval
	}
	if c.WatchdogInterval <= 0 {
		if iv, ok := sdnotify.WatchdogInterval(); ok {
			c.WatchdogInterval = iv
		} else {
			c.WatchdogInterval = defaultWatchdogInterval
		}
	}
	if c.PollStaleAfter <= 0 {
		c.PollStaleAfter = 5 * c.PollInterval
		if c.PollStaleAfter < 5*time.Minute {
			c.PollStaleAfter = 5 * time.Minute
		}
	}
	if c.WorkStaleAfter <= 0 {
		c.WorkStaleAfter = defaultWorkStaleAfter
	}
	if c.ResyncStaleAfter <= 0 {
		c.ResyncStaleAfter = 3 * c.ResyncInterval
	}
	return c
}

// Run は poll/work/resync/watchdog の 4 goroutine を起動し、ctx がキャンセルされる
// まで動かし続ける。呼び出し元は signal.NotifyContext 等で ctx を作り、
// SIGTERM/SIGINT を ctx のキャンセルに変換する責務を持つ（daemon パッケージは
// シグナルを直接扱わない）。
//
// ctx がキャンセルされると、Run は全 goroutine の終了を待ってから戻る。実行中の
// Worker.ProcessNext（claude の実行を含みうる）をどう打ち切るかは Worker の実装
// 自身が ctx を見て判断する責務であり、Run はそれを強制しない。
func Run(ctx context.Context, cfg Config) error {
	cfg = cfg.withDefaults()
	logger := cfg.Logger

	if cfg.Poller == nil || cfg.Worker == nil || cfg.Resyncer == nil {
		return errors.New("daemon: Poller, Worker, and Resyncer are all required")
	}

	now := time.Now()
	pollBeat := newHeartbeat(now)
	workBeat := newHeartbeat(now)
	resyncBeat := newHeartbeat(now)

	wake := make(chan struct{}, 1)

	var wg sync.WaitGroup
	wg.Add(4)

	go func() {
		defer wg.Done()
		runPoller(ctx, logger, cfg, wake, pollBeat)
	}()
	go func() {
		defer wg.Done()
		runWorker(ctx, logger, cfg, wake, workBeat)
	}()
	go func() {
		defer wg.Done()
		runResyncer(ctx, logger, cfg, resyncBeat)
	}()
	go func() {
		defer wg.Done()
		runWatchdog(ctx, logger, cfg, pollBeat, workBeat, resyncBeat)
	}()

	if ok, err := sdnotify.Ready(); err != nil {
		logger.Warn("sd_notify READY failed", "error", err.Error())
	} else if ok {
		logger.Info("sd_notify READY sent")
	}

	<-ctx.Done()
	logger.Info("shutdown signal received; waiting for loops to stop", "reason", ctx.Err())
	if _, err := sdnotify.Stopping(); err != nil {
		logger.Warn("sd_notify STOPPING failed", "error", err.Error())
	}

	wg.Wait()
	logger.Info("all loops stopped")
	return nil
}

func trigger(wake chan<- struct{}) {
	select {
	case wake <- struct{}{}:
	default:
	}
}

// runPoller は cfg.Poller.Poll を起動直後に 1 回、以後 cfg.PollInterval ごとに呼ぶ。
// enqueue が発生した場合は worker を wake で起こす。
func runPoller(ctx context.Context, logger *slog.Logger, cfg Config, wake chan<- struct{}, beat *heartbeat) {
	ticker := time.NewTicker(cfg.PollInterval)
	defer ticker.Stop()

	for {
		n, err := cfg.Poller.Poll(ctx)
		beat.mark(time.Now())
		switch {
		case err != nil:
			logger.Error("poll failed", "error", err.Error())
		case n > 0:
			logger.Info("poll enqueued events", "count", n)
			trigger(wake)
		}

		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// runWorker は cfg.Worker.ProcessNext を起動直後に 1 回、以後 wake 通知または
// cfg.WorkInterval ごとに呼ぶ。処理すべきイベントがまだ残っている
// （processed=true かつエラー無し）場合は待たずに次を試し、バックログを
// ポーリング間隔で律速しないようにする。並行実行はしない
// （clone を共有するためワーキングツリーが衝突する。DESIGN.md 12章）。
func runWorker(ctx context.Context, logger *slog.Logger, cfg Config, wake <-chan struct{}, beat *heartbeat) {
	ticker := time.NewTicker(cfg.WorkInterval)
	defer ticker.Stop()

	for {
		processed, err := cfg.Worker.ProcessNext(ctx)
		beat.mark(time.Now())
		if err != nil {
			logger.Error("worker failed to process an event", "error", err.Error())
		}

		if err == nil && processed {
			continue
		}

		select {
		case <-ctx.Done():
			return
		case <-wake:
		case <-ticker.C:
		}
	}
}

// runResyncer は cfg.Resyncer.Resync を起動直後に 1 回、以後 cfg.ResyncInterval
// ごとに呼ぶ。
func runResyncer(ctx context.Context, logger *slog.Logger, cfg Config, beat *heartbeat) {
	ticker := time.NewTicker(cfg.ResyncInterval)
	defer ticker.Stop()

	for {
		if err := cfg.Resyncer.Resync(ctx); err != nil {
			logger.Error("resync failed", "error", err.Error())
		}
		beat.mark(time.Now())

		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// runWatchdog は cfg.WatchdogInterval ごとに他 3 ループの生存を確認し、すべてが
// それぞれの許容時間内に動いている場合に限り systemd へ WATCHDOG=1 を送る。
// いずれかが停止しているとみなされる間は送信を止め、systemd の WatchdogSec が
// プロセスを再起動する（DESIGN.md 5章「ハング検知」）。
func runWatchdog(ctx context.Context, logger *slog.Logger, cfg Config, pollBeat, workBeat, resyncBeat *heartbeat) {
	ticker := time.NewTicker(cfg.WatchdogInterval)
	defer ticker.Stop()

	checks := []struct {
		name       string
		beat       *heartbeat
		staleAfter time.Duration
	}{
		{"poller", pollBeat, cfg.PollStaleAfter},
		{"worker", workBeat, cfg.WorkStaleAfter},
		{"resyncer", resyncBeat, cfg.ResyncStaleAfter},
	}

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}

		now := time.Now()
		healthy := true
		for _, c := range checks {
			if since := c.beat.since(now); since > c.staleAfter {
				logger.Warn("loop appears stalled; withholding watchdog notification",
					"loop", c.name, "since", since.String(), "stale_after", c.staleAfter.String())
				healthy = false
			}
		}
		if !healthy {
			continue
		}

		if _, err := sdnotify.Watchdog(); err != nil {
			logger.Warn("sd_notify WATCHDOG failed", "error", err.Error())
		}
	}
}
