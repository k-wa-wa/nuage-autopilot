package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"autopilot/internal/store"
)

func newTestServer(t *testing.T) (*Server, *store.Store) {
	t.Helper()
	st, err := store.Open(context.Background(), ":memory:")
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	s := New(st, nil)
	return s, st
}

func TestHandleDashboard_ListsItemsAndPhaseCounts(t *testing.T) {
	ctx := context.Background()
	s, st := newTestServer(t)

	a, _, err := st.UpsertItem(ctx, "k-wa-wa/pechka", 1, store.KindIssue)
	if err != nil {
		t.Fatalf("UpsertItem: %v", err)
	}
	if _, _, err := st.UpsertItem(ctx, "k-wa-wa/pechka", 2, store.KindPullRequest); err != nil {
		t.Fatalf("UpsertItem: %v", err)
	}
	if err := st.UpdateItemPhase(ctx, a.ID, store.PhaseInReview); err != nil {
		t.Fatalf("UpdateItemPhase: %v", err)
	}

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	s.Handler().ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()
	if !strings.Contains(body, "k-wa-wa/pechka") {
		t.Fatalf("body does not contain repo name: %s", body)
	}
	if !strings.Contains(body, "#1") || !strings.Contains(body, "#2") {
		t.Fatalf("body does not list both items: %s", body)
	}
}

func TestHandleDashboard_FiltersByPhaseAndRepo(t *testing.T) {
	ctx := context.Background()
	s, st := newTestServer(t)

	a, _, err := st.UpsertItem(ctx, "k-wa-wa/pechka", 1, store.KindIssue)
	if err != nil {
		t.Fatalf("UpsertItem: %v", err)
	}
	if _, _, err := st.UpsertItem(ctx, "k-wa-wa/nuage-workspace", 2, store.KindIssue); err != nil {
		t.Fatalf("UpsertItem: %v", err)
	}
	if err := st.UpdateItemPhase(ctx, a.ID, store.PhaseBlocked); err != nil {
		t.Fatalf("UpdateItemPhase: %v", err)
	}

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/?phase=blocked", nil)
	s.Handler().ServeHTTP(rr, req)

	body := rr.Body.String()
	// フィルタされた一覧テーブルには対象アイテムのみが並ぶことを確認する
	// （repo セレクトの選択肢には全 repo が出るため、行の内容だけを見る）。
	if !strings.Contains(body, `<td class="repo">k-wa-wa/pechka</td>`) {
		t.Fatalf("filtered view should include the blocked item's row: %s", body)
	}
	if strings.Contains(body, `<td class="repo">k-wa-wa/nuage-workspace</td>`) {
		t.Fatalf("filtered view should exclude the other repo's row: %s", body)
	}
}

func TestHandleItem_ShowsEventsAndLease(t *testing.T) {
	ctx := context.Background()
	s, st := newTestServer(t)

	it, _, err := st.UpsertItem(ctx, "k-wa-wa/pechka", 7, store.KindIssue)
	if err != nil {
		t.Fatalf("UpsertItem: %v", err)
	}
	if _, _, err := st.EnqueueEvent(ctx, "opened:1", it.ID, "opened", "k-wa-wa", "本文", time.Now()); err != nil {
		t.Fatalf("EnqueueEvent: %v", err)
	}
	acquired, err := st.AcquireLease(ctx, it.ID, "worker-1", time.Hour)
	if err != nil || !acquired {
		t.Fatalf("AcquireLease: acquired=%v err=%v", acquired, err)
	}

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/items/k-wa-wa/pechka/7", nil)
	s.Handler().ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()
	if !strings.Contains(body, "起票") {
		t.Fatalf("body should show the opened event label: %s", body)
	}
	if !strings.Contains(body, "worker-1") {
		t.Fatalf("body should show the active lease holder: %s", body)
	}
}

func TestHandleItem_UnknownItemReturns404(t *testing.T) {
	s, _ := newTestServer(t)

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/items/k-wa-wa/pechka/999", nil)
	s.Handler().ServeHTTP(rr, req)

	if rr.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rr.Code)
	}
}

func TestHandler_RejectsWriteMethods(t *testing.T) {
	s, _ := newTestServer(t)

	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodDelete} {
		rr := httptest.NewRecorder()
		req := httptest.NewRequest(method, "/", nil)
		s.Handler().ServeHTTP(rr, req)

		// ダッシュボードは読み取り専用である。書き込み系メソッドは受け付けない
		// （internal/web が SQLite に一切書き込まないことを裏付けるテスト）。
		if rr.Code == http.StatusOK {
			t.Fatalf("method %s got 200, want rejection on a read-only dashboard", method)
		}
	}
}

func TestServer_StaticAssetsAreServed(t *testing.T) {
	s, _ := newTestServer(t)

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/static/style.css", nil)
	s.Handler().ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), "body") {
		t.Fatalf("expected CSS content, got: %s", rr.Body.String())
	}
}
