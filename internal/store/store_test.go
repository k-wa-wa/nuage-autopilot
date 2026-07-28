package store

import (
	"context"
	"testing"
	"time"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(context.Background(), ":memory:")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func TestOpen_AppliesMigrations(t *testing.T) {
	s := newTestStore(t)

	var version int
	if err := s.db.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		t.Fatalf("read user_version: %v", err)
	}
	if version != len(migrations) {
		t.Fatalf("user_version = %d, want %d", version, len(migrations))
	}
}

func TestOpen_IsIdempotent(t *testing.T) {
	ctx := context.Background()
	s, err := Open(ctx, ":memory:")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()

	// :memory: の場合、Open を 2 回呼んでも新しい DB になるだけで migrate 自体は
	// 冪等であるべきなので、少なくともエラーにならないことを確認する。
	if err := migrate(ctx, s.db); err != nil {
		t.Fatalf("re-running migrate should be a no-op: %v", err)
	}
}

func TestUpsertItem_CreatesOnFirstCall(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	it, created, err := s.UpsertItem(ctx, "k-wa-wa/pechka", 42, KindIssue)
	if err != nil {
		t.Fatalf("UpsertItem: %v", err)
	}
	if !created {
		t.Fatalf("created = false, want true on first call")
	}
	if it.Repo != "k-wa-wa/pechka" || it.Number != 42 || it.Kind != KindIssue {
		t.Fatalf("unexpected item: %+v", it)
	}
	if it.Phase != PhaseNew {
		t.Fatalf("phase = %q, want %q", it.Phase, PhaseNew)
	}
	if it.ID == 0 {
		t.Fatalf("ID should be assigned, got 0")
	}
}

func TestUpsertItem_SecondCallIsNoop(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	first, _, err := s.UpsertItem(ctx, "k-wa-wa/pechka", 42, KindIssue)
	if err != nil {
		t.Fatalf("UpsertItem (1st): %v", err)
	}

	if err := s.UpdateItemPhase(ctx, first.ID, PhaseInReview); err != nil {
		t.Fatalf("UpdateItemPhase: %v", err)
	}

	second, created, err := s.UpsertItem(ctx, "k-wa-wa/pechka", 42, KindIssue)
	if err != nil {
		t.Fatalf("UpsertItem (2nd): %v", err)
	}
	if created {
		t.Fatalf("created = true on 2nd call, want false (upsert must be idempotent)")
	}
	if second.Phase != PhaseInReview {
		t.Fatalf("phase = %q, want %q (2nd upsert must not reset phase)", second.Phase, PhaseInReview)
	}
}

func TestGetItem_NotFound(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	_, ok, err := s.GetItem(ctx, "k-wa-wa/pechka", 999)
	if err != nil {
		t.Fatalf("GetItem: %v", err)
	}
	if ok {
		t.Fatalf("ok = true, want false for a non-existent item")
	}
}

func TestUpdateItemPhase_UnknownIDErrors(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	if err := s.UpdateItemPhase(ctx, 12345, PhaseBlocked); err == nil {
		t.Fatalf("UpdateItemPhase(unknown id) should error")
	}
}

func TestItemParentAndChildren(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	parent, _, err := s.UpsertItem(ctx, "k-wa-wa/pechka", 1, KindIssue)
	if err != nil {
		t.Fatalf("UpsertItem parent: %v", err)
	}
	child1, _, err := s.UpsertItem(ctx, "k-wa-wa/pechka", 2, KindIssue)
	if err != nil {
		t.Fatalf("UpsertItem child1: %v", err)
	}
	child2, _, err := s.UpsertItem(ctx, "k-wa-wa/pechka", 3, KindIssue)
	if err != nil {
		t.Fatalf("UpsertItem child2: %v", err)
	}

	if err := s.SetItemParent(ctx, child1.ID, parent.ID); err != nil {
		t.Fatalf("SetItemParent child1: %v", err)
	}
	if err := s.SetItemParent(ctx, child2.ID, parent.ID); err != nil {
		t.Fatalf("SetItemParent child2: %v", err)
	}
	if err := s.UpdateItemPhase(ctx, parent.ID, PhaseDelegated); err != nil {
		t.Fatalf("UpdateItemPhase parent: %v", err)
	}

	children, err := s.ListChildren(ctx, parent.ID)
	if err != nil {
		t.Fatalf("ListChildren: %v", err)
	}
	if len(children) != 2 {
		t.Fatalf("len(children) = %d, want 2", len(children))
	}
	for _, c := range children {
		if c.ParentID == nil || *c.ParentID != parent.ID {
			t.Fatalf("child %d has parent_id %v, want %d", c.Number, c.ParentID, parent.ID)
		}
	}

	reloadedParent, ok, err := s.GetItemByID(ctx, parent.ID)
	if err != nil || !ok {
		t.Fatalf("GetItemByID(parent): ok=%v err=%v", ok, err)
	}
	if reloadedParent.Phase != PhaseDelegated {
		t.Fatalf("parent phase = %q, want %q", reloadedParent.Phase, PhaseDelegated)
	}
}

func TestUpdateItemLastSeenAt_RoundTrips(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	it, _, err := s.UpsertItem(ctx, "k-wa-wa/pechka", 1, KindIssue)
	if err != nil {
		t.Fatalf("UpsertItem: %v", err)
	}
	if it.LastSeenAt != nil {
		t.Fatalf("LastSeenAt = %v, want nil for a freshly created item", it.LastSeenAt)
	}

	seenAt := time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)
	if err := s.UpdateItemLastSeenAt(ctx, it.ID, seenAt); err != nil {
		t.Fatalf("UpdateItemLastSeenAt: %v", err)
	}

	reloaded, ok, err := s.GetItemByID(ctx, it.ID)
	if err != nil || !ok {
		t.Fatalf("GetItemByID: ok=%v err=%v", ok, err)
	}
	if reloaded.LastSeenAt == nil || !reloaded.LastSeenAt.Equal(seenAt) {
		t.Fatalf("LastSeenAt = %v, want %v", reloaded.LastSeenAt, seenAt)
	}
}

func TestListItemsByPhase(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	a, _, _ := s.UpsertItem(ctx, "k-wa-wa/pechka", 1, KindIssue)
	b, _, _ := s.UpsertItem(ctx, "k-wa-wa/pechka", 2, KindIssue)
	_, _, _ = s.UpsertItem(ctx, "k-wa-wa/pechka", 3, KindIssue)

	if err := s.UpdateItemPhase(ctx, a.ID, PhaseAwaitingAnswer); err != nil {
		t.Fatalf("UpdateItemPhase a: %v", err)
	}
	if err := s.UpdateItemPhase(ctx, b.ID, PhaseAwaitingAnswer); err != nil {
		t.Fatalf("UpdateItemPhase b: %v", err)
	}

	got, err := s.ListItemsByPhase(ctx, PhaseAwaitingAnswer)
	if err != nil {
		t.Fatalf("ListItemsByPhase: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("len(got) = %d, want 2", len(got))
	}

	newItems, err := s.ListItemsByPhase(ctx, PhaseNew)
	if err != nil {
		t.Fatalf("ListItemsByPhase(new): %v", err)
	}
	if len(newItems) != 1 {
		t.Fatalf("len(newItems) = %d, want 1", len(newItems))
	}
}

func TestAddItemUsage_AccumulatesAndResetClearsIt(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	it, _, err := s.UpsertItem(ctx, "k-wa-wa/pechka", 1, KindIssue)
	if err != nil {
		t.Fatalf("UpsertItem: %v", err)
	}

	if err := s.AddItemUsage(ctx, it.ID, 1.5); err != nil {
		t.Fatalf("AddItemUsage: %v", err)
	}
	if err := s.AddItemUsage(ctx, it.ID, 2.0); err != nil {
		t.Fatalf("AddItemUsage: %v", err)
	}

	reloaded, ok, err := s.GetItemByID(ctx, it.ID)
	if err != nil || !ok {
		t.Fatalf("GetItemByID: ok=%v err=%v", ok, err)
	}
	if reloaded.Runs != 2 {
		t.Fatalf("Runs = %d, want 2", reloaded.Runs)
	}
	if reloaded.CostUSD != 3.5 {
		t.Fatalf("CostUSD = %v, want 3.5", reloaded.CostUSD)
	}

	// 人間がコメントしたときに呼ばれる想定（DESIGN.md 10章）。
	if err := s.ResetItemBudget(ctx, it.ID); err != nil {
		t.Fatalf("ResetItemBudget: %v", err)
	}
	reset, ok, err := s.GetItemByID(ctx, it.ID)
	if err != nil || !ok {
		t.Fatalf("GetItemByID after reset: ok=%v err=%v", ok, err)
	}
	if reset.Runs != 0 || reset.CostUSD != 0 {
		t.Fatalf("after reset: Runs=%d CostUSD=%v, want 0/0", reset.Runs, reset.CostUSD)
	}
}

func TestEnqueueEvent_DedupSuppressesDuplicates(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	it, _, err := s.UpsertItem(ctx, "k-wa-wa/pechka", 1, KindIssue)
	if err != nil {
		t.Fatalf("UpsertItem: %v", err)
	}

	now := time.Date(2026, 7, 29, 0, 0, 0, 0, time.UTC)

	_, inserted1, err := s.EnqueueEvent(ctx, "comment:100", it.ID, "commented", "k-wa-wa", "hello", now)
	if err != nil {
		t.Fatalf("EnqueueEvent (1st): %v", err)
	}
	if !inserted1 {
		t.Fatalf("inserted = false on first enqueue, want true")
	}

	_, inserted2, err := s.EnqueueEvent(ctx, "comment:100", it.ID, "commented", "k-wa-wa", "hello again (duplicate delivery)", now)
	if err != nil {
		t.Fatalf("EnqueueEvent (2nd): %v", err)
	}
	if inserted2 {
		t.Fatalf("inserted = true on duplicate dedup_key, want false")
	}

	n, err := s.CountUnprocessedEvents(ctx)
	if err != nil {
		t.Fatalf("CountUnprocessedEvents: %v", err)
	}
	if n != 1 {
		t.Fatalf("CountUnprocessedEvents = %d, want 1 (dedup must prevent a second row)", n)
	}
}

func TestNextUnprocessedEvent_OrdersByCreatedAtThenMarksProcessed(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	it, _, err := s.UpsertItem(ctx, "k-wa-wa/pechka", 1, KindIssue)
	if err != nil {
		t.Fatalf("UpsertItem: %v", err)
	}

	older := time.Date(2026, 7, 29, 0, 0, 0, 0, time.UTC)
	newer := older.Add(time.Hour)

	// わざと新しい方を先に積む。取り出し順は created_at 昇順であるべき。
	if _, _, err := s.EnqueueEvent(ctx, "b", it.ID, "commented", "k-wa-wa", "newer", newer); err != nil {
		t.Fatalf("EnqueueEvent newer: %v", err)
	}
	if _, _, err := s.EnqueueEvent(ctx, "a", it.ID, "commented", "k-wa-wa", "older", older); err != nil {
		t.Fatalf("EnqueueEvent older: %v", err)
	}

	ev, ok, err := s.NextUnprocessedEvent(ctx)
	if err != nil || !ok {
		t.Fatalf("NextUnprocessedEvent: ok=%v err=%v", ok, err)
	}
	if ev.DedupKey != "a" {
		t.Fatalf("DedupKey = %q, want %q (oldest first)", ev.DedupKey, "a")
	}

	if err := s.MarkEventProcessed(ctx, ev.ID); err != nil {
		t.Fatalf("MarkEventProcessed: %v", err)
	}

	next, ok, err := s.NextUnprocessedEvent(ctx)
	if err != nil || !ok {
		t.Fatalf("NextUnprocessedEvent (2nd): ok=%v err=%v", ok, err)
	}
	if next.DedupKey != "b" {
		t.Fatalf("DedupKey = %q, want %q", next.DedupKey, "b")
	}

	if err := s.MarkEventProcessed(ctx, next.ID); err != nil {
		t.Fatalf("MarkEventProcessed: %v", err)
	}
	_, ok, err = s.NextUnprocessedEvent(ctx)
	if err != nil {
		t.Fatalf("NextUnprocessedEvent (3rd): %v", err)
	}
	if ok {
		t.Fatalf("ok = true, want false once all events are processed")
	}
}

func TestAcquireLease_BlocksUntilExpiry(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 7, 29, 10, 0, 0, 0, time.UTC)
	clock := &fakeClock{t: now}

	s, err := Open(ctx, ":memory:", WithClock(clock.Now))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()

	it, _, err := s.UpsertItem(ctx, "k-wa-wa/pechka", 1, KindIssue)
	if err != nil {
		t.Fatalf("UpsertItem: %v", err)
	}

	acquired, err := s.AcquireLease(ctx, it.ID, "host-a:100", 10*time.Minute)
	if err != nil {
		t.Fatalf("AcquireLease (host-a): %v", err)
	}
	if !acquired {
		t.Fatalf("acquired = false, want true for a free item")
	}

	// 期限内に別の holder が奪おうとしても失敗する。
	stillHeld, err := s.AcquireLease(ctx, it.ID, "host-b:200", 10*time.Minute)
	if err != nil {
		t.Fatalf("AcquireLease (host-b, before expiry): %v", err)
	}
	if stillHeld {
		t.Fatalf("acquired = true, want false while host-a's lease is still valid")
	}

	// 期限を過ぎれば別の holder が奪える（クラッシュからの自動回収を模している）。
	clock.t = clock.t.Add(11 * time.Minute)
	stolen, err := s.AcquireLease(ctx, it.ID, "host-b:200", 10*time.Minute)
	if err != nil {
		t.Fatalf("AcquireLease (host-b, after expiry): %v", err)
	}
	if !stolen {
		t.Fatalf("acquired = false, want true once host-a's lease has expired")
	}

	lease, ok, err := s.GetLease(ctx, it.ID)
	if err != nil || !ok {
		t.Fatalf("GetLease: ok=%v err=%v", ok, err)
	}
	if lease.Holder != "host-b:200" {
		t.Fatalf("Holder = %q, want %q", lease.Holder, "host-b:200")
	}
}

func TestReleaseLease_OnlyByOwningHolder(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	it, _, err := s.UpsertItem(ctx, "k-wa-wa/pechka", 1, KindIssue)
	if err != nil {
		t.Fatalf("UpsertItem: %v", err)
	}
	if _, err := s.AcquireLease(ctx, it.ID, "host-a:100", time.Hour); err != nil {
		t.Fatalf("AcquireLease: %v", err)
	}

	// 別 holder 名義での解放は無視される（既に奪取された正当なリースを誤って
	// 消さないための安全策）。
	if err := s.ReleaseLease(ctx, it.ID, "host-b:200"); err != nil {
		t.Fatalf("ReleaseLease (wrong holder): %v", err)
	}
	if _, ok, err := s.GetLease(ctx, it.ID); err != nil || !ok {
		t.Fatalf("lease should still exist after a wrong-holder release: ok=%v err=%v", ok, err)
	}

	if err := s.ReleaseLease(ctx, it.ID, "host-a:100"); err != nil {
		t.Fatalf("ReleaseLease (correct holder): %v", err)
	}
	if _, ok, err := s.GetLease(ctx, it.ID); err != nil || ok {
		t.Fatalf("lease should be gone after the owning holder releases it: ok=%v err=%v", ok, err)
	}
}

func TestReapExpiredLeases(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 7, 29, 10, 0, 0, 0, time.UTC)
	clock := &fakeClock{t: now}

	s, err := Open(ctx, ":memory:", WithClock(clock.Now))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()

	expiring, _, _ := s.UpsertItem(ctx, "k-wa-wa/pechka", 1, KindIssue)
	fresh, _, _ := s.UpsertItem(ctx, "k-wa-wa/pechka", 2, KindIssue)

	if _, err := s.AcquireLease(ctx, expiring.ID, "host-a:100", time.Minute); err != nil {
		t.Fatalf("AcquireLease expiring: %v", err)
	}
	if _, err := s.AcquireLease(ctx, fresh.ID, "host-a:100", time.Hour); err != nil {
		t.Fatalf("AcquireLease fresh: %v", err)
	}

	clock.t = clock.t.Add(2 * time.Minute)

	n, err := s.ReapExpiredLeases(ctx)
	if err != nil {
		t.Fatalf("ReapExpiredLeases: %v", err)
	}
	if n != 1 {
		t.Fatalf("reaped = %d, want 1", n)
	}

	if _, ok, err := s.GetLease(ctx, expiring.ID); err != nil || ok {
		t.Fatalf("expiring lease should be gone: ok=%v err=%v", ok, err)
	}
	if _, ok, err := s.GetLease(ctx, fresh.ID); err != nil || !ok {
		t.Fatalf("fresh lease should remain: ok=%v err=%v", ok, err)
	}
}

func TestCursor_RoundTrip(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	if _, ok, err := s.GetCursor(ctx, "notifications"); err != nil || ok {
		t.Fatalf("cursor should not exist yet: ok=%v err=%v", ok, err)
	}

	if err := s.SaveCursor(ctx, "notifications", `"etag-1"`, "", "2026-07-29T00:00:00Z"); err != nil {
		t.Fatalf("SaveCursor: %v", err)
	}

	c, ok, err := s.GetCursor(ctx, "notifications")
	if err != nil || !ok {
		t.Fatalf("GetCursor: ok=%v err=%v", ok, err)
	}
	if c.ETag != `"etag-1"` || c.Since != "2026-07-29T00:00:00Z" {
		t.Fatalf("unexpected cursor: %+v", c)
	}
	if c.PolledAt == nil {
		t.Fatalf("PolledAt should be set by SaveCursor")
	}

	// 2 回目の保存は上書きする。
	if err := s.SaveCursor(ctx, "notifications", `"etag-2"`, "", "2026-07-29T01:00:00Z"); err != nil {
		t.Fatalf("SaveCursor (2nd): %v", err)
	}
	c2, ok, err := s.GetCursor(ctx, "notifications")
	if err != nil || !ok {
		t.Fatalf("GetCursor (2nd): ok=%v err=%v", ok, err)
	}
	if c2.ETag != `"etag-2"` {
		t.Fatalf("ETag = %q, want %q", c2.ETag, `"etag-2"`)
	}
}

type fakeClock struct {
	t time.Time
}

func (c *fakeClock) Now() time.Time { return c.t }
