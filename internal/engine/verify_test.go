package engine

import (
	"context"
	"net/http"
	"testing"
	"time"

	"autopilot/internal/store"
)

// prDetailHandler は verify が対象とする PR の詳細を返す。verify は
// fetchDetail 経由で GetPullRequest を呼ぶため、Issue とは別の経路になる。
func prDetailHandler(t *testing.T) http.HandlerFunc {
	t.Helper()
	return func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/k-wa-wa/pechka/pulls/1":
			writeJSON(w, `{"number": 1, "title": "add dark mode", "body": "closes #9", "state": "open", "user": {"login": "bot-wa-wa", "type": "Bot"}}`)
		default:
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
	}
}

// newVerifyItem は ci_success を受け取る直前の状態（PR が存在し、phase=in_review、
// head_sha が判明している）のアイテムを作る。
func newVerifyItem(t *testing.T, h *testHarness, sha string) store.Item {
	t.Helper()
	ctx := context.Background()

	item, _, err := h.store.UpsertItem(ctx, "k-wa-wa/pechka", 1, store.KindPullRequest)
	if err != nil {
		t.Fatalf("UpsertItem: %v", err)
	}
	if err := h.store.UpdateItemPhase(ctx, item.ID, store.PhaseInReview); err != nil {
		t.Fatalf("UpdateItemPhase: %v", err)
	}
	if err := h.store.UpdateItemHeadSHA(ctx, item.ID, sha); err != nil {
		t.Fatalf("UpdateItemHeadSHA: %v", err)
	}
	reloaded, _, err := h.store.GetItemByID(ctx, item.ID)
	if err != nil {
		t.Fatalf("GetItemByID: %v", err)
	}
	return reloaded
}

func enqueueCiSuccess(t *testing.T, h *testHarness, item store.Item, sha string) {
	t.Helper()
	dedup := "checkrun:k-wa-wa/pechka#1:" + sha + ":success"
	if _, _, err := h.store.EnqueueEvent(context.Background(), dedup, item.ID, "ci_success", "github", "", time.Now()); err != nil {
		t.Fatalf("EnqueueEvent: %v", err)
	}
}

func mustPhase(t *testing.T, h *testHarness, id int64) store.Item {
	t.Helper()
	item, ok, err := h.store.GetItemByID(context.Background(), id)
	if err != nil || !ok {
		t.Fatalf("GetItemByID: ok=%v err=%v", ok, err)
	}
	return item
}

// TestProcessNext_VerifyPassedMovesToReady は verify_passed を受け取ったとき
// ready に進むことを検証する。
func TestProcessNext_VerifyPassedMovesToReady(t *testing.T) {
	claude := writeFakeClaude(t, `{"outcome": "verify_passed"}`, "verify-sess", 0.1)
	h := newTestEngine(t, prDetailHandler(t), claude)
	ctx := context.Background()

	item := newVerifyItem(t, h, "sha-1")
	enqueueCiSuccess(t, h, item, "sha-1")

	if _, err := h.engine.ProcessNext(ctx); err != nil {
		t.Fatalf("ProcessNext() error = %v", err)
	}

	if got := mustPhase(t, h, item.ID).Phase; got != store.PhaseReady {
		t.Fatalf("Phase = %q, want %q", got, store.PhaseReady)
	}
}

// TestProcessNext_VerifyInconclusiveMovesToReady は「検証できなかった」場合に
// PR を詰まらせず ready に進めることを検証する。この性質があるため、verify を
// 検証できない事情で PR が止まることが無い（DESIGN.md 8.4 節）。
func TestProcessNext_VerifyInconclusiveMovesToReady(t *testing.T) {
	claude := writeFakeClaude(t, `{"outcome": "verify_inconclusive"}`, "verify-sess", 0.1)
	h := newTestEngine(t, prDetailHandler(t), claude)
	ctx := context.Background()

	item := newVerifyItem(t, h, "sha-1")
	enqueueCiSuccess(t, h, item, "sha-1")

	if _, err := h.engine.ProcessNext(ctx); err != nil {
		t.Fatalf("ProcessNext() error = %v", err)
	}

	if got := mustPhase(t, h, item.ID).Phase; got != store.PhaseReady {
		t.Fatalf("Phase = %q, want %q", got, store.PhaseReady)
	}
	if n, err := h.store.CountUnprocessedEvents(ctx); err != nil || n != 0 {
		t.Fatalf("CountUnprocessedEvents = %d err=%v, want 0 (差し戻しは起きない)", n, err)
	}
}

// TestProcessNext_VerifyFailedKeepsInReviewAndEnqueuesVerifyFailure は
// 不合格が phase を止めるのではなく、agent を起こし直すイベントとして表現される
// ことを検証する（DESIGN.md 8.1 / 8.3 節）。
func TestProcessNext_VerifyFailedKeepsInReviewAndEnqueuesVerifyFailure(t *testing.T) {
	claude := writeFakeClaude(t, `{"outcome": "verify_failed"}`, "verify-sess", 0.1)
	h := newTestEngine(t, prDetailHandler(t), claude)
	ctx := context.Background()

	item := newVerifyItem(t, h, "sha-1")
	enqueueCiSuccess(t, h, item, "sha-1")

	if _, err := h.engine.ProcessNext(ctx); err != nil {
		t.Fatalf("ProcessNext() error = %v", err)
	}

	if got := mustPhase(t, h, item.ID).Phase; got != store.PhaseInReview {
		t.Fatalf("Phase = %q, want %q (差し戻し中は in_review のまま)", got, store.PhaseInReview)
	}

	ev, ok, err := h.store.NextUnprocessedEvent(ctx)
	if err != nil || !ok {
		t.Fatalf("NextUnprocessedEvent: ok=%v err=%v, want a verify_failure event", ok, err)
	}
	if ev.Type != "verify_failure" {
		t.Fatalf("event type = %q, want verify_failure", ev.Type)
	}
	if ev.Body == "" {
		t.Fatal("verify_failure event body is empty; agent は差し戻し理由の所在を知れない")
	}
	// 差し戻しイベントは agent を起こす。
	if got := nextAction(store.PhaseInReview, ev.Type); got != actionLaunchResume {
		t.Fatalf("nextAction(in_review, verify_failure) = %q, want %q", got, actionLaunchResume)
	}
}

// TestProcessNext_VerifyFailureIsNotEnqueuedTwiceForSameSHA は、プロセスが落ちて
// ci_success が再処理された場合でも差し戻しが二重に積まれないことを検証する。
// dedup_key に head_sha を含めていることがこの性質を支えている（DESIGN.md 11章）。
func TestProcessNext_VerifyFailureIsNotEnqueuedTwiceForSameSHA(t *testing.T) {
	claude := writeFakeClaude(t, `{"outcome": "verify_failed"}`, "verify-sess", 0.1)
	h := newTestEngine(t, prDetailHandler(t), claude)
	ctx := context.Background()

	item := newVerifyItem(t, h, "sha-1")

	// 同じ head_sha に対する verify を 2 回走らせる（クラッシュ後の再処理を模す）。
	for i := 0; i < 2; i++ {
		if err := h.engine.applyVerifyOutcome(ctx, item, agentResult{Outcome: OutcomeVerifyFailed}); err != nil {
			t.Fatalf("applyVerifyOutcome(%d): %v", i, err)
		}
	}

	if n, err := h.store.CountUnprocessedEvents(ctx); err != nil || n != 1 {
		t.Fatalf("CountUnprocessedEvents = %d err=%v, want 1", n, err)
	}
}

// TestProcessNext_VerifyDoesNotPersistSessionID は verify のセッションが
// アイテムに保存されないことを検証する。保存してしまうと次回の実装エージェントが
// verify のセッションを --resume してしまい、第三者性が失われる。
func TestProcessNext_VerifyDoesNotPersistSessionID(t *testing.T) {
	claude := writeFakeClaude(t, `{"outcome": "verify_passed"}`, "verify-sess", 0.25)
	h := newTestEngine(t, prDetailHandler(t), claude)
	ctx := context.Background()

	item := newVerifyItem(t, h, "sha-1")
	enqueueCiSuccess(t, h, item, "sha-1")

	if _, err := h.engine.ProcessNext(ctx); err != nil {
		t.Fatalf("ProcessNext() error = %v", err)
	}

	reloaded := mustPhase(t, h, item.ID)
	if reloaded.SessionID != "" {
		t.Fatalf("SessionID = %q, want empty (verify のセッションは保存しない)", reloaded.SessionID)
	}
	// コストと実行回数は 1 本の予算として計上する（DESIGN.md 10章）。
	if reloaded.CostUSD != 0.25 {
		t.Fatalf("CostUSD = %v, want 0.25", reloaded.CostUSD)
	}
	if reloaded.Runs != 1 {
		t.Fatalf("Runs = %d, want 1", reloaded.Runs)
	}
}

// TestProcessNext_VerifyUnavailableMovesToReadyInsteadOfBlocking は、verify 自体を
// 起動できなかった場合に PR を blocked にせず ready へ進めることを検証する。
//
// 「判定が無い」を「不合格」に読み替えず、検証されなかった事実だけを残して
// 先に進める（DESIGN.md 8.4 節）。
func TestProcessNext_VerifyUnavailableMovesToReadyInsteadOfBlocking(t *testing.T) {
	var commented bool
	h := newTestEngine(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/repos/k-wa-wa/pechka/issues/1/comments":
			commented = true
			w.WriteHeader(http.StatusCreated)
			writeJSON(w, `{}`)
		case r.URL.Path == "/repos/k-wa-wa/pechka/pulls/1":
			writeJSON(w, `{"number": 1, "title": "add dark mode", "body": "closes #9", "state": "open", "user": {"login": "bot-wa-wa", "type": "Bot"}}`)
		default:
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
	}, "/nonexistent/claude-cannot-be-launched")
	ctx := context.Background()

	item := newVerifyItem(t, h, "sha-1")
	enqueueCiSuccess(t, h, item, "sha-1")

	if _, err := h.engine.ProcessNext(ctx); err != nil {
		t.Fatalf("ProcessNext() error = %v", err)
	}

	if got := mustPhase(t, h, item.ID).Phase; got != store.PhaseReady {
		t.Fatalf("Phase = %q, want %q (verify の起動失敗で PR を詰まらせない)", got, store.PhaseReady)
	}
	if !commented {
		t.Fatal("検証されないまま進んだことが GitHub に残っていない")
	}
}

// TestProcessNext_LeaseHeldLeavesEventUnprocessed は、他の保持者がリースを
// 握っている間はイベントを消費しないことを検証する。
//
// これはクラッシュ耐性の要である。プロセスが落ちると解放されなかったリースが
// TTL 切れまで残るため、その間に届いたイベントを処理済みにしてしまうと作業が
// 恒久的に失われる（DESIGN.md 11章）。
func TestProcessNext_LeaseHeldLeavesEventUnprocessed(t *testing.T) {
	h := newTestEngine(t, func(w http.ResponseWriter, r *http.Request) {
		t.Fatalf("unexpected GitHub request (agent must not be launched): %s %s", r.Method, r.URL.Path)
	}, "/nonexistent/claude-must-not-be-invoked")
	ctx := context.Background()

	item, _, err := h.store.UpsertItem(ctx, "k-wa-wa/pechka", 1, store.KindIssue)
	if err != nil {
		t.Fatalf("UpsertItem: %v", err)
	}
	// 落ちた前プロセスが残したリースを模す。
	acquired, err := h.store.AcquireLease(ctx, item.ID, "dead-host:1234", time.Hour)
	if err != nil || !acquired {
		t.Fatalf("AcquireLease: acquired=%v err=%v", acquired, err)
	}
	if _, _, err := h.store.EnqueueEvent(ctx, "opened:1", item.ID, "opened", "alice", "body", time.Now()); err != nil {
		t.Fatalf("EnqueueEvent: %v", err)
	}

	processed, err := h.engine.ProcessNext(ctx)
	if err != nil {
		t.Fatalf("ProcessNext() error = %v", err)
	}
	if processed {
		t.Fatal("processed = true, want false (worker はタイマー待ちに入るべきである)")
	}

	// イベントは未処理のまま残り、リース失効後に再試行できる。
	if n, err := h.store.CountUnprocessedEvents(ctx); err != nil || n != 1 {
		t.Fatalf("CountUnprocessedEvents = %d err=%v, want 1 (イベントを失ってはならない)", n, err)
	}
}
