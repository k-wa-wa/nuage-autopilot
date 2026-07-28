package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// GetCursor は source（例: "notifications"）の読み取り位置を返す。
// 一度も保存されていない場合は ok=false を返す。
func (s *Store) GetCursor(ctx context.Context, source string) (Cursor, bool, error) {
	row := s.db.QueryRowContext(ctx, `SELECT source, etag, last_modified, since, polled_at FROM cursors WHERE source = ?`, source)

	var (
		c            Cursor
		etag         sql.NullString
		lastModified sql.NullString
		since        sql.NullString
		polledAt     sql.NullString
	)
	err := row.Scan(&c.Source, &etag, &lastModified, &since, &polledAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Cursor{}, false, nil
	}
	if err != nil {
		return Cursor{}, false, fmt.Errorf("store: get cursor %s: %w", source, err)
	}

	c.ETag = etag.String
	c.LastModified = lastModified.String
	c.Since = since.String

	t, err := parseNullableTime(polledAt)
	if err != nil {
		return Cursor{}, false, fmt.Errorf("store: parse polled_at: %w", err)
	}
	c.PolledAt = t

	return c, true, nil
}

// SaveCursor は source の読み取り位置を保存する（既存の行があれば置き換える）。
// polled_at は自動的に現在時刻へ更新する。
func (s *Store) SaveCursor(ctx context.Context, source, etag, lastModified, since string) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO cursors (source, etag, last_modified, since, polled_at)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(source) DO UPDATE SET
			etag = excluded.etag,
			last_modified = excluded.last_modified,
			since = excluded.since,
			polled_at = excluded.polled_at`,
		source, etag, lastModified, since, formatTime(s.now()))
	if err != nil {
		return fmt.Errorf("store: save cursor %s: %w", source, err)
	}
	return nil
}
