package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// AcquireLease は itemID に対する排他制御を holder 名義で取得する。
//
// 未取得、または既存のリースが期限切れの場合に取得できる（acquired=true）。
// 既存のリースが有効な場合は取得できない（acquired=false, err=nil。これはエラーでは
// なく「今は他が処理中」という通常の結果である）。
//
// SQLite の UPSERT に WHERE 句を付けることで、この判定と更新を 1 ステートメントの
// アトミックな操作として行う。旧設計が自動回収を諦めた理由（「他プロセスが起動して
// いる可能性を Go 側から判別できない」）は、holder と expires_at を持つこの方式で
// 解消される（DESIGN.md 11章）。
func (s *Store) AcquireLease(ctx context.Context, itemID int64, holder string, ttl time.Duration) (acquired bool, err error) {
	now := s.now()
	expiresAt := formatTime(now.Add(ttl))

	res, err := s.db.ExecContext(ctx, `
		INSERT INTO leases (item_id, holder, expires_at)
		VALUES (?, ?, ?)
		ON CONFLICT(item_id) DO UPDATE SET
			holder = excluded.holder,
			expires_at = excluded.expires_at
		WHERE leases.expires_at < ?`,
		itemID, holder, expiresAt, formatTime(now))
	if err != nil {
		return false, fmt.Errorf("store: acquire lease for item %d: %w", itemID, err)
	}

	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("store: acquire lease for item %d: rows affected: %w", itemID, err)
	}
	return n > 0, nil
}

// ReleaseLease は holder が保持する itemID のリースを解放する。
//
// holder が一致しない場合（既に TTL で失効し、別の holder が奪取済みの場合など）は
// 何もしない。エラーにはしない。これは古い保持者が「自分のリースだと思って」解放
// しようとした結果として起こりうる正常系であり、他者が取得した新しいリースを
// 誤って消さないための安全策である。
func (s *Store) ReleaseLease(ctx context.Context, itemID int64, holder string) error {
	if _, err := s.db.ExecContext(ctx, `DELETE FROM leases WHERE item_id = ? AND holder = ?`, itemID, holder); err != nil {
		return fmt.Errorf("store: release lease for item %d: %w", itemID, err)
	}
	return nil
}

// GetLease は itemID のリースを返す。存在しない場合は ok=false を返す
// （期限切れかどうかは呼び出し側が ExpiresAt を見て判断する）。
func (s *Store) GetLease(ctx context.Context, itemID int64) (Lease, bool, error) {
	row := s.db.QueryRowContext(ctx, `SELECT item_id, holder, expires_at FROM leases WHERE item_id = ?`, itemID)

	var (
		l         Lease
		expiresAt string
	)
	err := row.Scan(&l.ItemID, &l.Holder, &expiresAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Lease{}, false, nil
	}
	if err != nil {
		return Lease{}, false, fmt.Errorf("store: get lease for item %d: %w", itemID, err)
	}

	t, err := parseTime(expiresAt)
	if err != nil {
		return Lease{}, false, fmt.Errorf("store: parse expires_at: %w", err)
	}
	l.ExpiresAt = t
	return l, true, nil
}

// ReapExpiredLeases は expires_at が現在時刻より過去のリースをすべて削除し、削除件数を返す。
// AcquireLease は個別のアイテムに対して同等の回収を自律的に行うため、この関数は
// resync による一括清掃や観測用に使う（DESIGN.md 7.5 節）。
func (s *Store) ReapExpiredLeases(ctx context.Context) (int64, error) {
	res, err := s.db.ExecContext(ctx, `DELETE FROM leases WHERE expires_at < ?`, formatTime(s.now()))
	if err != nil {
		return 0, fmt.Errorf("store: reap expired leases: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("store: reap expired leases: rows affected: %w", err)
	}
	return n, nil
}
