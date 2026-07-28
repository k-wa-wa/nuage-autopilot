// Command nuage-autopilot は GitHub Issue/PR を起点にアプリ開発を自動化する
// nuage-autopilot の実行バイナリである。詳細は autopilot/DESIGN.md を参照。
//
// 単一の常駐プロセスとして、poll/work/resync/watchdog の 4 goroutine を
// internal/daemon 上で動かす（DESIGN.md 5章）。状態は SQLite（internal/store）に持ち、
// プロセス自体は無状態である。
//
// Phase 1（本実装）の時点では internal/ingest・internal/engine がまだ無いため、
// Poller/Worker には最小限のプレースホルダを渡し、「LLM も GitHub も呼ばず空回りする
// デーモンとして systemd 上で安定稼働する」ことを確認する段階に留める
// （DESIGN.md 18章 Phase 1）。Resyncer だけは、GitHub を必要としない
// 期限切れリースの回収を実際に行う。
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"autopilot/internal/config"
	"autopilot/internal/daemon"
	"autopilot/internal/store"
)

// version はビルド時に -ldflags "-X main.version=..." で上書きされる想定の値である。
var version = "dev"

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

// run は main のロジック本体をテスト可能な形に切り出したものである。
func run(args []string, stdout, stderr io.Writer) int {
	cfg, err := config.Parse(args)
	if err != nil {
		if errors.Is(err, config.ErrRepoRequired) {
			fmt.Fprintln(stderr, err)
			return 2
		}
		// flag パッケージは -h/--help やパースエラー時に独自のメッセージを
		// 既に stderr へ出力済みなので、ここでは終了コードのみ返す。
		return 2
	}

	if cfg.ShowVersion {
		fmt.Fprintln(stdout, version)
		return 0
	}

	logger := slog.New(slog.NewJSONHandler(stdout, nil))

	// secrets.env は手作業で配置する運用のため、起動直後は存在しないことがある。
	// 常駐プロセスであるため、旧設計（oneshot）のように「警告して正常終了」はしない。
	// GitHub を必要としないループは動き続け、GitHub API を呼ぶ段になって初めて
	// エラーとしてログに現れる（DESIGN.md 15章）。
	if missing := config.MissingEnv(); len(missing) > 0 {
		logger.Warn("required environment variables are not set; GitHub-dependent operations will fail until they are configured",
			"missing", missing,
			"hint", "/var/lib/nuage-autopilot/secrets.env に値を配置する",
		)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	dbPath := filepath.Join(cfg.StateDir, "state.db")
	st, err := store.Open(ctx, dbPath)
	if err != nil {
		logger.Error("failed to open state store", "path", dbPath, "error", err.Error())
		return 1
	}
	defer func() {
		if err := st.Close(); err != nil {
			logger.Warn("failed to close state store cleanly", "error", err.Error())
		}
	}()

	logger.Info("nuage-autopilot starting",
		"version", version, "repos", cfg.Repos, "state_dir", cfg.StateDir, "db_path", dbPath)

	if err := daemon.Run(ctx, daemon.Config{
		Logger:   logger,
		Poller:   newPlaceholderPoller(logger),
		Worker:   newPlaceholderWorker(logger, st),
		Resyncer: newLeaseReapingResyncer(logger, st),
	}); err != nil {
		logger.Error("daemon exited with error", "error", err.Error())
		return 1
	}

	return 0
}

// newPlaceholderPoller は Phase 1 のプレースホルダである。GitHub への通知取得
// （DESIGN.md 7章）は internal/ingest として Phase 2 で実装する。
func newPlaceholderPoller(logger *slog.Logger) daemon.Poller {
	return daemon.PollerFunc(func(ctx context.Context) (int, error) {
		logger.Debug("poll tick (Phase 1 placeholder: internal/ingest is not wired yet)")
		return 0, nil
	})
}

// newPlaceholderWorker は Phase 1 のプレースホルダである。実際のイベント処理
// （DESIGN.md 8章）は internal/engine として Phase 3 で実装する。
//
// 現時点では events を enqueue する経路が無い（poller が常に 0 件を返す）ため
// この関数の本体が実行されることは無いが、将来 enqueue 経路だけが先行してできた
// 場合に備え、未処理イベントを検出したら「処理せず滞留させ、ログに残す」防御的な
// 実装にしている。
func newPlaceholderWorker(logger *slog.Logger, st *store.Store) daemon.Worker {
	return daemon.WorkerFunc(func(ctx context.Context) (bool, error) {
		ev, ok, err := st.NextUnprocessedEvent(ctx)
		if err != nil {
			return false, fmt.Errorf("check for unprocessed events: %w", err)
		}
		if !ok {
			return false, nil
		}
		logger.Warn("found an unprocessed event but internal/engine is not wired yet (Phase 3); leaving it queued",
			"event_id", ev.ID, "item_id", ev.ItemID, "type", ev.Type)
		return false, nil
	})
}

// newLeaseReapingResyncer は resync のうち GitHub を必要としない部分
// （期限切れリースの回収。DESIGN.md 7.5 節）だけを Phase 1 から行う。
// GitHub 側との全走査による突き合わせは internal/ingest として Phase 2 で追加する。
func newLeaseReapingResyncer(logger *slog.Logger, st *store.Store) daemon.Resyncer {
	return daemon.ResyncerFunc(func(ctx context.Context) error {
		n, err := st.ReapExpiredLeases(ctx)
		if err != nil {
			return fmt.Errorf("reap expired leases: %w", err)
		}
		if n > 0 {
			logger.Info("resync reaped expired leases", "count", n)
		}
		return nil
	})
}
