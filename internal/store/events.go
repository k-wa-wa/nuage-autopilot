package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

const eventColumns = `id, dedup_key, item_id, type, actor, body, created_at, processed_at`

// EnqueueEvent は itemID に対する 1 件の出来事を events に積む。
//
// dedupKey が既存の行と衝突する場合、新しい行は作られず inserted=false を返す
// （同じ通知・コメントを何度取り込んでもイベントは 1 件しか生まれないことを保証する。
// DESIGN.md 6.3 節）。
func (s *Store) EnqueueEvent(ctx context.Context, dedupKey string, itemID int64, eventType, actor, body string, createdAt time.Time) (id int64, inserted bool, err error) {
	res, err := s.db.ExecContext(ctx, `
		INSERT INTO events (dedup_key, item_id, type, actor, body, created_at, processed_at)
		VALUES (?, ?, ?, ?, ?, ?, NULL)
		ON CONFLICT(dedup_key) DO NOTHING`,
		dedupKey, itemID, eventType, actor, body, formatTime(createdAt))
	if err != nil {
		return 0, false, fmt.Errorf("store: enqueue event %s: %w", dedupKey, err)
	}

	n, err := res.RowsAffected()
	if err != nil {
		return 0, false, fmt.Errorf("store: enqueue event %s: rows affected: %w", dedupKey, err)
	}
	if n == 0 {
		return 0, false, nil
	}

	insertedID, err := res.LastInsertId()
	if err != nil {
		return 0, false, fmt.Errorf("store: enqueue event %s: last insert id: %w", dedupKey, err)
	}
	return insertedID, true, nil
}

// NextUnprocessedEvent は processed_at が NULL の中で最も古いイベントを 1 件返す。
// events.processed_at IS NULL がそのまま処理待ちキューを成す（別途キュー機構は持たない）。
// 未処理イベントが無い場合は ok=false を返す。
func (s *Store) NextUnprocessedEvent(ctx context.Context) (Event, bool, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT `+eventColumns+` FROM events
		WHERE processed_at IS NULL
		ORDER BY created_at ASC, id ASC
		LIMIT 1`)
	return scanEvent(row)
}

// MarkEventProcessed は id のイベントを処理済みにする。
func (s *Store) MarkEventProcessed(ctx context.Context, id int64) error {
	res, err := s.db.ExecContext(ctx, `UPDATE events SET processed_at = ? WHERE id = ?`, formatTime(s.now()), id)
	if err != nil {
		return fmt.Errorf("store: mark event %d processed: %w", id, err)
	}
	return requireRowsAffected(res, "event", id)
}

// CountUnprocessedEvents は未処理イベントの件数を返す。デーモンの生存確認・ログ用。
func (s *Store) CountUnprocessedEvents(ctx context.Context) (int, error) {
	var n int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM events WHERE processed_at IS NULL`).Scan(&n); err != nil {
		return 0, fmt.Errorf("store: count unprocessed events: %w", err)
	}
	return n, nil
}

func scanEvent(row rowScanner) (Event, bool, error) {
	var (
		ev          Event
		body        sql.NullString
		createdAt   string
		processedAt sql.NullString
	)

	err := row.Scan(&ev.ID, &ev.DedupKey, &ev.ItemID, &ev.Type, &ev.Actor, &body, &createdAt, &processedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Event{}, false, nil
	}
	if err != nil {
		return Event{}, false, fmt.Errorf("store: scan event: %w", err)
	}

	ev.Body = body.String

	t, err := parseTime(createdAt)
	if err != nil {
		return Event{}, false, fmt.Errorf("store: parse created_at: %w", err)
	}
	ev.CreatedAt = t

	pt, err := parseNullableTime(processedAt)
	if err != nil {
		return Event{}, false, fmt.Errorf("store: parse processed_at: %w", err)
	}
	ev.ProcessedAt = pt

	return ev, true, nil
}
