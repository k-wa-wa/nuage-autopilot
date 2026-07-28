package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

const itemColumns = `id, repo, number, kind, phase, parent_id, session_id, head_sha, cost_usd, runs, last_seen_at, updated_at`

// UpsertItem は repo/number のアイテムを取得し、無ければ phase=new で新規作成する。
//
// 冪等である。resync や poller が同じアイテムを何度観測しても 1 行しか作られない
// （DESIGN.md 7.6 節: 初めて認識したアイテムは記録のみで着火しない。着火するか
// どうかは呼び出し側が戻り値の created を見て判断する）。
func (s *Store) UpsertItem(ctx context.Context, repo string, number int, kind Kind) (item Item, created bool, err error) {
	now := formatTime(s.now())

	res, err := s.db.ExecContext(ctx, `
		INSERT INTO items (repo, number, kind, phase, updated_at)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(repo, number) DO NOTHING`,
		repo, number, string(kind), string(PhaseNew), now)
	if err != nil {
		return Item{}, false, fmt.Errorf("store: upsert item %s#%d: %w", repo, number, err)
	}

	if n, _ := res.RowsAffected(); n > 0 {
		it, ok, err := s.GetItem(ctx, repo, number)
		if err != nil {
			return Item{}, false, err
		}
		if !ok {
			return Item{}, false, fmt.Errorf("store: item %s#%d disappeared immediately after insert", repo, number)
		}
		return it, true, nil
	}

	it, ok, err := s.GetItem(ctx, repo, number)
	if err != nil {
		return Item{}, false, err
	}
	if !ok {
		return Item{}, false, fmt.Errorf("store: item %s#%d not found after no-op upsert", repo, number)
	}
	return it, false, nil
}

// GetItem は repo/number のアイテムを取得する。存在しない場合は ok=false を返す。
func (s *Store) GetItem(ctx context.Context, repo string, number int) (Item, bool, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+itemColumns+` FROM items WHERE repo = ? AND number = ?`, repo, number)
	return scanItem(row)
}

// GetItemByID は id でアイテムを取得する。存在しない場合は ok=false を返す。
func (s *Store) GetItemByID(ctx context.Context, id int64) (Item, bool, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+itemColumns+` FROM items WHERE id = ?`, id)
	return scanItem(row)
}

// ListItemsByPhase は phase に一致するアイテムを updated_at 昇順で返す。
func (s *Store) ListItemsByPhase(ctx context.Context, phase Phase) ([]Item, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+itemColumns+` FROM items WHERE phase = ? ORDER BY updated_at ASC`, string(phase))
	if err != nil {
		return nil, fmt.Errorf("store: list items by phase %s: %w", phase, err)
	}
	defer rows.Close()
	return scanItems(rows)
}

// ListChildren は parentID を親とするアイテムを返す。
func (s *Store) ListChildren(ctx context.Context, parentID int64) ([]Item, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+itemColumns+` FROM items WHERE parent_id = ? ORDER BY id ASC`, parentID)
	if err != nil {
		return nil, fmt.Errorf("store: list children of %d: %w", parentID, err)
	}
	defer rows.Close()
	return scanItems(rows)
}

// UpdateItemPhase は id の phase を更新する。
func (s *Store) UpdateItemPhase(ctx context.Context, id int64, phase Phase) error {
	return s.updateItem(ctx, id, "phase", string(phase))
}

// UpdateItemSessionID は claude --resume 用のセッション ID を更新する。
func (s *Store) UpdateItemSessionID(ctx context.Context, id int64, sessionID string) error {
	return s.updateItem(ctx, id, "session_id", sessionID)
}

// UpdateItemHeadSHA は id の head_sha を更新する。
func (s *Store) UpdateItemHeadSHA(ctx context.Context, id int64, sha string) error {
	return s.updateItem(ctx, id, "head_sha", sha)
}

// UpdateItemLastSeenAt は取り込み済みコメントの最新時刻を更新する。
func (s *Store) UpdateItemLastSeenAt(ctx context.Context, id int64, seenAt string) error {
	return s.updateItem(ctx, id, "last_seen_at", seenAt)
}

// SetItemParent は id の親を parentID に設定する。
func (s *Store) SetItemParent(ctx context.Context, id, parentID int64) error {
	return s.updateItem(ctx, id, "parent_id", parentID)
}

func (s *Store) updateItem(ctx context.Context, id int64, column string, value any) error {
	// column はこのパッケージ内の定数のみが渡る呼び出し元限定の内部ヘルパーであり、
	// 外部入力を SQL に埋め込むものではない。
	query := fmt.Sprintf(`UPDATE items SET %s = ?, updated_at = ? WHERE id = ?`, column)
	res, err := s.db.ExecContext(ctx, query, value, formatTime(s.now()), id)
	if err != nil {
		return fmt.Errorf("store: update item %d.%s: %w", id, column, err)
	}
	return requireRowsAffected(res, "item", id)
}

// AddItemUsage は cost_usd に costUSD を加算し、runs を 1 増やす。
// claude 1 回の実行が終わるたびに呼ぶ（DESIGN.md 10章）。
func (s *Store) AddItemUsage(ctx context.Context, id int64, costUSD float64) error {
	res, err := s.db.ExecContext(ctx, `
		UPDATE items SET cost_usd = cost_usd + ?, runs = runs + 1, updated_at = ? WHERE id = ?`,
		costUSD, formatTime(s.now()), id)
	if err != nil {
		return fmt.Errorf("store: add usage to item %d: %w", id, err)
	}
	return requireRowsAffected(res, "item", id)
}

// ResetItemBudget は cost_usd と runs をゼロに戻す。人間がコメントしたときに呼ぶ
// （DESIGN.md 10章: 人間の関与が唯一の脱出口である、というモデルを反映する）。
func (s *Store) ResetItemBudget(ctx context.Context, id int64) error {
	res, err := s.db.ExecContext(ctx, `
		UPDATE items SET cost_usd = 0, runs = 0, updated_at = ? WHERE id = ?`,
		formatTime(s.now()), id)
	if err != nil {
		return fmt.Errorf("store: reset budget for item %d: %w", id, err)
	}
	return requireRowsAffected(res, "item", id)
}

func requireRowsAffected(res sql.Result, kind string, id int64) error {
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("store: rows affected: %w", err)
	}
	if n == 0 {
		return fmt.Errorf("store: %s %d not found", kind, id)
	}
	return nil
}

// rowScanner は *sql.Row と *sql.Rows の両方が満たすインターフェースである。
type rowScanner interface {
	Scan(dest ...any) error
}

func scanItem(row rowScanner) (Item, bool, error) {
	var (
		it         Item
		parentID   sql.NullInt64
		sessionID  sql.NullString
		headSHA    sql.NullString
		lastSeenAt sql.NullString
		updatedAt  string
	)

	err := row.Scan(&it.ID, &it.Repo, &it.Number, &it.Kind, &it.Phase, &parentID, &sessionID, &headSHA, &it.CostUSD, &it.Runs, &lastSeenAt, &updatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Item{}, false, nil
	}
	if err != nil {
		return Item{}, false, fmt.Errorf("store: scan item: %w", err)
	}

	if parentID.Valid {
		v := parentID.Int64
		it.ParentID = &v
	}
	it.SessionID = sessionID.String
	it.HeadSHA = headSHA.String

	seenAt, err := parseNullableTime(lastSeenAt)
	if err != nil {
		return Item{}, false, fmt.Errorf("store: parse last_seen_at: %w", err)
	}
	it.LastSeenAt = seenAt

	t, err := parseTime(updatedAt)
	if err != nil {
		return Item{}, false, fmt.Errorf("store: parse updated_at: %w", err)
	}
	it.UpdatedAt = t

	return it, true, nil
}

func scanItems(rows *sql.Rows) ([]Item, error) {
	var out []Item
	for rows.Next() {
		it, ok, err := scanItem(rows)
		if err != nil {
			return nil, err
		}
		if ok {
			out = append(out, it)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate items: %w", err)
	}
	return out, nil
}
