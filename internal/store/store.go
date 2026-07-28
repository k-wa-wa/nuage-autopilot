// Package store は nuage-autopilot の永続状態（items / events / leases / cursors）を
// SQLite で保持する。DESIGN.md 6章を参照。
//
// DB は真実ではない。GitHub が真実であり、DB はその写像にすぎない。したがって
// このパッケージは GitHub との整合性を自ら保証しない。整合性の保証は
// internal/ingest の低頻度な全走査（resync）の責務である。
//
// SQLite ドライバは pure-Go 実装（github.com/ncruces/go-sqlite3）を使う。cgo が入ると
// buildGoModule でのビルドと vendorHash=null 運用が破綻するためである
// （DESIGN.md 6.4 節）。SQLite 本体は WebAssembly にコンパイルされたバイナリとして
// go:embed で組み込まれており（driver/embed サブパッケージ）、ビルド・実行時とも
// ネットワークアクセスを必要としない。同種の modernc.org/sqlite は GOOS/GOARCH の
// 組み合わせごとに C→Go 変換済みコードを vendor するため vendor/ が 200MB を超えるが、
// この実装は wasm バイナリ 1 つを埋め込むだけなので大幅に小さい。
package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	_ "github.com/ncruces/go-sqlite3/driver"
	_ "github.com/ncruces/go-sqlite3/embed"
)

// Store は SQLite への接続と、items/events/leases/cursors に対する CRUD を提供する。
type Store struct {
	db *sql.DB

	// now は現在時刻を取得する関数である。既定は time.Now だが、lease の TTL や
	// updated_at をテストで決定的に検証するために差し替え可能にしている。
	now func() time.Time
}

// Option は Open の挙動を変更する関数オプションである。
type Option func(*Store)

// WithClock は現在時刻の取得元を差し替える。テスト用。
func WithClock(now func() time.Time) Option {
	return func(s *Store) {
		if now != nil {
			s.now = now
		}
	}
}

// Open は path（ファイルパス、または ":memory:"）の SQLite データベースを開き、
// 未適用のマイグレーションを適用して Store を返す。
//
// path がファイルの場合、WAL モードを有効にする。単一プロセス内の複数 goroutine
// （poller/worker/resyncer）が同じ *sql.DB を共有する設計であるため、書き込みの
// 直列化は database/sql のコネクションプールと busy_timeout に委ねる。
func Open(ctx context.Context, path string, opts ...Option) (*Store, error) {
	// ドライバが PRAGMA 指定などのクエリパラメータを解釈するのは "file:" スキームの
	// DSN のみである（データベースファイルへの生パスや ":memory:" だけでは無視される）。
	// ":memory:" は "file::memory:" という SQLite の標準 URI 形式になる。
	dsn := "file:" + path
	// busy_timeout は「他 goroutine が書き込み中」で即座にエラーにせず、指定ミリ秒
	// まで待ってから再試行させるためのものである。
	dsn += "?_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)"
	if path != ":memory:" {
		dsn += "&_pragma=journal_mode(WAL)"
	}

	db, err := sql.Open("sqlite3", dsn)
	if err != nil {
		return nil, fmt.Errorf("store: open %s: %w", path, err)
	}

	// SQLite は書き込みを直列化するしかないため、複数コネクションを許すと
	// 「database is locked」を busy_timeout の許容範囲外で引き起こしやすくなる。
	// 1 コネクションに固定し、database/sql のプールに直列化を任せる。
	db.SetMaxOpenConns(1)

	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("store: ping %s: %w", path, err)
	}

	if err := migrate(ctx, db); err != nil {
		_ = db.Close()
		return nil, err
	}

	s := &Store{db: db, now: time.Now}
	for _, opt := range opts {
		opt(s)
	}
	return s, nil
}

// Close は基底の DB 接続を閉じる。
func (s *Store) Close() error {
	return s.db.Close()
}

// timeLayout は timestamp を TEXT 列に書き込む際の書式である。
//
// time.RFC3339Nano は末尾の 0 を切り詰めるため、フォーマットのたびに桁数が変わり
// SQL 側の文字列比較（ORDER BY created_at、lease の expires_at < ? 判定）が
// 数値としての大小関係と食い違いうる（詳しくは "0" の並びが小数点以下を固定 9 桁の
// ゼロ埋めにする）。UTC に統一したうえでこのレイアウトを使うことで、常に同じ長さの
// 文字列になり、辞書順比較がそのまま時系列順比較として成立する。
const timeLayout = "2006-01-02T15:04:05.000000000Z07:00"

func nullableTime(t *time.Time) any {
	if t == nil {
		return nil
	}
	return formatTime(*t)
}

func formatTime(t time.Time) string {
	return t.UTC().Format(timeLayout)
}

func parseTime(s string) (time.Time, error) {
	return time.Parse(timeLayout, s)
}

func parseNullableTime(ns sql.NullString) (*time.Time, error) {
	if !ns.Valid || ns.String == "" {
		return nil, nil
	}
	t, err := parseTime(ns.String)
	if err != nil {
		return nil, err
	}
	return &t, nil
}
