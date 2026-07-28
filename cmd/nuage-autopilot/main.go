// Command nuage-autopilot は GitHub Issue/PR を起点にアプリ開発を自動化する
// nuage-autopilot の実行バイナリである。詳細は autopilot/DESIGN.md を参照。
//
// 単一の常駐プロセスとして、poll/work/resync/watchdog の 4 goroutine を
// internal/daemon 上で動かす（DESIGN.md 5章）。状態は SQLite（internal/store）に持ち、
// プロセス自体は無状態である。
//
// Phase 2（本実装）の時点で Poller/Resyncer は internal/ingest による実装に
// 差し替わり、GitHub の変化を実際に events として取り込む（DESIGN.md 7章）。
// internal/engine（遷移表・エージェント起動）はまだ無いため、Worker は
// プレースホルダのままである（DESIGN.md 18章 Phase 3 で置き換える）。
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
	"autopilot/internal/github"
	"autopilot/internal/ingest"
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

	client := github.NewClient(githubClientOptions()...)

	poller := &ingest.Poller{
		Client:         client,
		Store:          st,
		Repos:          cfg.Repos,
		AllowedAuthors: cfg.AllowedAuthors,
		Logger:         logger,
	}
	resyncer := &ingest.Resyncer{
		Client:         client,
		Store:          st,
		Repos:          cfg.Repos,
		AllowedAuthors: cfg.AllowedAuthors,
		Logger:         logger,
	}

	if err := daemon.Run(ctx, daemon.Config{
		Logger:   logger,
		Poller:   poller,
		Worker:   newPlaceholderWorker(logger, st),
		Resyncer: resyncer,
	}); err != nil {
		logger.Error("daemon exited with error", "error", err.Error())
		return 1
	}

	return 0
}

// githubClientOptions は internal/github.Client の生成オプションを組み立てる。
//
// NUAGE_GITHUB_API_BASE_URL は本番運用では未設定のままでよい内部フックである。
// 実際の GitHub API に到達させたくない結合テストや、GitHub Enterprise 運用での
// ベース URL 差し替えのために用意している。
func githubClientOptions() []github.Option {
	var opts []github.Option
	if baseURL := os.Getenv("NUAGE_GITHUB_API_BASE_URL"); baseURL != "" {
		opts = append(opts, github.WithBaseURL(baseURL))
	}
	return opts
}

// newPlaceholderWorker は Phase 2 時点でのプレースホルダである。実際のイベント処理
// （DESIGN.md 8章）は internal/engine として Phase 3 で実装する。
//
// internal/ingest が events を enqueue するようになった（Phase 2）ため、この
// 関数はもう到達しうる。未処理イベントを見つけても処理せず滞留させ、ログに
// 残すだけに留める（engine が実装されるまで、イベントは失われずキューに残る）。
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
