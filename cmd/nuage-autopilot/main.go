// Command nuage-autopilot は GitHub Issue/PR を起点にアプリ開発を自動化する
// nuage-autopilot の実行バイナリである。詳細は DESIGN.md を参照。
//
// 単一の常駐プロセスとして、poll/work/resync/watchdog の 4 goroutine を
// internal/daemon 上で動かす（DESIGN.md 5章）。状態は SQLite（internal/store）に持ち、
// プロセス自体は無状態である。
//
// Poller/Resyncer は internal/ingest（DESIGN.md 7章）、Worker は internal/engine
// （DESIGN.md 8章）がそれぞれ実装する。このファイルの責務は設定の解決と依存の
// 組み立てだけであり、遷移やイベント取り込みの判断は一切持たない。
//
// 加えて、SQLite の中身を読み取り専用で見せる Web ダッシュボード（internal/web、
// DESIGN.md 14章）を、上記 4 goroutine とは独立したライフサイクルで起動する。
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
	"sync"
	"syscall"

	"autopilot/internal/config"
	"autopilot/internal/daemon"
	"autopilot/internal/engine"
	"autopilot/internal/github"
	"autopilot/internal/ingest"
	"autopilot/internal/skills"
	"autopilot/internal/store"
	"autopilot/internal/web"
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

	if err := skills.Ensure(); err != nil {
		logger.Warn("failed to ensure autopilot skills", "error", err.Error())
	}

	// 環境変数は起動後に設定・更新される場合もあるため、未設定時も警告を出力して起動を継続する。
	// GitHub API 呼び出し時に環境変数を直接読み出すことで、動的な環境変数設定に対応する。
	if missing := config.MissingEnv(); len(missing) > 0 {
		logger.Warn("required environment variables are not set; GitHub-dependent operations will fail until they are configured",
			"missing", missing,
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
	poller.EnsureSubscriptions(ctx)

	resyncer := &ingest.Resyncer{
		Client:         client,
		Store:          st,
		Repos:          cfg.Repos,
		AllowedAuthors: cfg.AllowedAuthors,
		Logger:         logger,
	}

	worker := engine.New(engine.Config{
		Store:    st,
		Client:   client,
		StateDir: cfg.StateDir,
		Repos:    cfg.Repos,
		Logger:   logger,
	})

	// 読み取り専用ダッシュボード（DESIGN.md 14章）は poll/work/resync/watchdog の
	// 4 goroutine とは独立したライフサイクルで動かす。閲覧機能が落ちても自動化の
	// 本体を止める理由にはならないため、daemon.Run とは別に起動・待機する。
	var webWG sync.WaitGroup
	if cfg.WebAddr != "" {
		dashboard := web.New(st, logger)
		webWG.Add(1)
		go func() {
			defer webWG.Done()
			logger.Info("starting read-only web dashboard", "addr", cfg.WebAddr)
			if err := dashboard.ListenAndServe(ctx, cfg.WebAddr); err != nil {
				logger.Error("web dashboard exited with error", "error", err.Error())
			}
		}()
	} else {
		logger.Info("read-only web dashboard disabled")
	}

	exitCode := 0
	if err := daemon.Run(ctx, daemon.Config{
		Logger:   logger,
		Poller:   poller,
		Worker:   worker,
		Resyncer: resyncer,
	}); err != nil {
		logger.Error("daemon exited with error", "error", err.Error())
		exitCode = 1
	}

	webWG.Wait()

	return exitCode
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
