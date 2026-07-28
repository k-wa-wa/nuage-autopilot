package store

import (
	"context"
	"database/sql"
	_ "embed"
	"fmt"
	"strings"
)

//go:embed migrations/0001_init.sql
var migration0001 string

// migrations は user_version をバージョン番号として適用する SQL の列である。
// 添字 0 が user_version=1 への移行を表す（0 は「初期状態、テーブル無し」を意味する
// ため、0 番目の移行先を 1 とする）。将来スキーマを変更する場合はこのスライスに
// 追記するだけでよく、Open のたびに未適用の分だけ順に実行される。
var migrations = []string{
	migration0001,
}

// migrate は現在の user_version から migrations の末尾まで順に適用する。
// 各移行は 1 トランザクションで実行し、user_version の更新も同じトランザクション内で行う。
func migrate(ctx context.Context, db *sql.DB) error {
	var current int
	if err := db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&current); err != nil {
		return fmt.Errorf("store: read user_version: %w", err)
	}

	for i := current; i < len(migrations); i++ {
		if err := applyMigration(ctx, db, migrations[i], i+1); err != nil {
			return fmt.Errorf("store: apply migration %d: %w", i+1, err)
		}
	}
	return nil
}

func applyMigration(ctx context.Context, db *sql.DB, script string, version int) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	for _, stmt := range splitStatements(script) {
		if _, err := tx.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("exec statement %q: %w", stmt, err)
		}
	}

	// PRAGMA はプレースホルダを受け付けないため文字列として埋め込む。version は
	// このパッケージ内部の int でありユーザー入力ではないため injection の懸念はない。
	if _, err := tx.ExecContext(ctx, fmt.Sprintf("PRAGMA user_version = %d", version)); err != nil {
		return err
	}

	return tx.Commit()
}

// splitStatements は ";" 区切りの単純な SQL スクリプトをステートメント単位に分割する。
// 移行スクリプトは自前で書く前提であり、文字列リテラル中にセミコロンを含む複雑な
// SQL は使わない（この分割で十分な範囲に留める）。
func splitStatements(script string) []string {
	var out []string
	for _, s := range strings.Split(script, ";") {
		s = strings.TrimSpace(s)
		if s != "" {
			out = append(out, s)
		}
	}
	return out
}
