// Package web は SQLite（internal/store）の中身を読み取り専用で見せる HTML ダッシュボードである
// （DESIGN.md 14章）。
//
// フロントエンドを別リポジトリ・別プロセスに切り出さず、テンプレートと CSS を go:embed で
// バイナリに埋め込む。単一バイナリ + Nix という既存の配布形態を変えずに済むこと、
// および別ホスト・別ビルドツールチェーン（npm 等）を Nix パッケージングに持ち込まずに済む
// ことが理由である。JS フレームワークは使わず、html/template によるサーバサイド
// レンダリングと <meta http-equiv="refresh"> による定期更新のみで構成する。
//
// このパッケージは store から読むだけで、一切書き込みを行わない。ハンドラはすべて
// GET のみを受け付ける。
package web

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"autopilot/internal/store"
)

// Server は読み取り専用ダッシュボードの HTTP ハンドラをまとめたものである。
// internal/store.Store に対して読み取りしか行わない（DESIGN.md 14章）。
type Server struct {
	store  *store.Store
	logger *slog.Logger
	now    func() time.Time
}

// New はダッシュボードの Server を作る。
func New(st *store.Store, logger *slog.Logger) *Server {
	if logger == nil {
		logger = slog.Default()
	}
	return &Server{store: st, logger: logger, now: time.Now}
}

// Handler は net/http にそのまま渡せるルーティング済みハンドラを返す。
// テストは httptest でこれを直接叩く。
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", s.handleDashboard)
	mux.HandleFunc("GET /items/{owner}/{repo}/{number}", s.handleItem)
	mux.Handle("GET /static/", http.StripPrefix("/static/", http.FileServerFS(staticFS)))
	return s.withLogging(mux)
}

func (s *Server) withLogging(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			// ダッシュボードは読み取り専用である。GET/HEAD 以外は存在しないものとして扱う。
			http.Error(w, "read-only dashboard: only GET is supported", http.StatusMethodNotAllowed)
			return
		}
		next.ServeHTTP(w, r)
		s.logger.Debug("web request", "method", r.Method, "path", r.URL.Path)
	})
}

// ListenAndServe は addr で HTTP サーバを起動し、ctx がキャンセルされたら graceful shutdown する。
// daemon.Run の 4 goroutine とは独立したライフサイクルで動く（DESIGN.md 14章）。
// ダッシュボードの異常は自動化の本体（poll/work/resync）を止める理由にはならないため、
// ListenAndServe が返すエラーは呼び出し側がログに出す程度の扱いでよい設計にしている。
func (s *Server) ListenAndServe(ctx context.Context, addr string) error {
	srv := &http.Server{
		Addr:    addr,
		Handler: s.Handler(),
	}

	errCh := make(chan error, 1)
	go func() {
		errCh <- srv.ListenAndServe()
	}()

	select {
	case err := <-errCh:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return fmt.Errorf("web: listen on %s: %w", addr, err)
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			return fmt.Errorf("web: shutdown: %w", err)
		}
		return nil
	}
}
