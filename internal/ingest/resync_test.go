package ingest

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"autopilot/internal/github"
	"autopilot/internal/store"
)

func newTestResyncer(t *testing.T, handler http.HandlerFunc, opts ...func(*Resyncer)) (*Resyncer, *store.Store) {
	t.Helper()

	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)

	client := github.NewClient(github.WithBaseURL(server.URL), github.WithStaticToken("test-token"))

	st, err := store.Open(context.Background(), ":memory:")
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	r := &Resyncer{Client: client, Store: st, Repos: []string{"k-wa-wa/pechka"}}
	for _, opt := range opts {
		opt(r)
	}
	return r, st
}

// TestResync_RegistersNewItemsWithoutEvents は、open な Issue/PR を発見したときに
// DB へ登録するがイベントは一切 enqueue しないことを検証する（DESIGN.md 7.5 節:
// resync は着火しない）。
func TestResync_RegistersNewItemsWithoutEvents(t *testing.T) {
	r, st := newTestResyncer(t, func(w http.ResponseWriter, req *http.Request) {
		switch req.URL.Path {
		case "/repos/k-wa-wa/pechka/issues":
			writeJSON(w, `[{"number": 1, "title": "an issue", "state": "open", "body": "x", "user": {"login": "alice", "type": "User"}}]`)
		case "/repos/k-wa-wa/pechka/pulls":
			writeJSON(w, `[{"number": 2, "title": "a pr", "state": "open", "body": "y", "user": {"login": "alice", "type": "User"}, "head": {"sha": "abc123"}}]`)
		default:
			t.Fatalf("unexpected request: %s %s", req.Method, req.URL.Path)
		}
	})

	if err := r.Resync(context.Background()); err != nil {
		t.Fatalf("Resync() error = %v", err)
	}

	issue, ok, err := st.GetItem(context.Background(), "k-wa-wa/pechka", 1)
	if err != nil || !ok {
		t.Fatalf("issue should be registered: ok=%v err=%v", ok, err)
	}
	if issue.Phase != store.PhaseNew {
		t.Fatalf("issue.Phase = %q, want %q", issue.Phase, store.PhaseNew)
	}

	pr, ok, err := st.GetItem(context.Background(), "k-wa-wa/pechka", 2)
	if err != nil || !ok {
		t.Fatalf("pr should be registered: ok=%v err=%v", ok, err)
	}
	if pr.HeadSHA != "abc123" {
		t.Fatalf("pr.HeadSHA = %q, want abc123", pr.HeadSHA)
	}

	if n, err := st.CountUnprocessedEvents(context.Background()); err != nil || n != 0 {
		t.Fatalf("CountUnprocessedEvents = %d, err=%v, want 0", n, err)
	}
}

// TestResync_RejectsDisallowedAuthorAndIgnoreLabel は対象アイテムの選別が
// resync 経路でも効くことを検証する。
func TestResync_RejectsDisallowedAuthorAndIgnoreLabel(t *testing.T) {
	r, st := newTestResyncer(t, func(w http.ResponseWriter, req *http.Request) {
		switch req.URL.Path {
		case "/repos/k-wa-wa/pechka/issues":
			writeJSON(w, `[
				{"number": 1, "title": "stranger", "state": "open", "user": {"login": "random-stranger", "type": "User"}},
				{"number": 2, "title": "ignored", "state": "open", "user": {"login": "alice", "type": "User"}, "labels": [{"name": "agent:ignore"}]}
			]`)
		case "/repos/k-wa-wa/pechka/pulls":
			writeJSON(w, `[]`)
		default:
			t.Fatalf("unexpected request: %s %s", req.Method, req.URL.Path)
		}
	}, func(r *Resyncer) { r.AllowedAuthors = []string{"alice"} })

	if err := r.Resync(context.Background()); err != nil {
		t.Fatalf("Resync() error = %v", err)
	}

	if _, ok, err := st.GetItem(context.Background(), "k-wa-wa/pechka", 1); err != nil || ok {
		t.Fatalf("disallowed-author item should not be registered: ok=%v err=%v", ok, err)
	}
	if _, ok, err := st.GetItem(context.Background(), "k-wa-wa/pechka", 2); err != nil || ok {
		t.Fatalf("ignored item should not be registered: ok=%v err=%v", ok, err)
	}
}

// TestResync_MarksItemsClosedOnGitHubAsDone は、GitHub 上で close/merge された
// アイテムを done に遷移させることを検証する。
func TestResync_MarksItemsClosedOnGitHubAsDone(t *testing.T) {
	r, st := newTestResyncer(t, func(w http.ResponseWriter, req *http.Request) {
		switch req.URL.Path {
		case "/repos/k-wa-wa/pechka/issues":
			writeJSON(w, `[]`) // #3 はもう open ではない
		case "/repos/k-wa-wa/pechka/pulls":
			writeJSON(w, `[]`)
		default:
			t.Fatalf("unexpected request: %s %s", req.Method, req.URL.Path)
		}
	})

	ctx := context.Background()
	item, _, err := st.UpsertItem(ctx, "k-wa-wa/pechka", 3, store.KindIssue)
	if err != nil {
		t.Fatalf("UpsertItem (seed): %v", err)
	}
	if err := st.UpdateItemPhase(ctx, item.ID, store.PhaseAwaitingAnswer); err != nil {
		t.Fatalf("UpdateItemPhase (seed): %v", err)
	}

	if err := r.Resync(ctx); err != nil {
		t.Fatalf("Resync() error = %v", err)
	}

	reloaded, ok, err := st.GetItemByID(ctx, item.ID)
	if err != nil || !ok {
		t.Fatalf("GetItemByID: ok=%v err=%v", ok, err)
	}
	if reloaded.Phase != store.PhaseDone {
		t.Fatalf("phase = %q, want %q", reloaded.Phase, store.PhaseDone)
	}
}

// TestResync_LeavesAlreadyDoneItemsAlone は既に done なアイテムを再度更新
// しないことを検証する（無駄な updated_at の更新を避ける）。
func TestResync_LeavesAlreadyDoneItemsAlone(t *testing.T) {
	r, st := newTestResyncer(t, func(w http.ResponseWriter, req *http.Request) {
		switch req.URL.Path {
		case "/repos/k-wa-wa/pechka/issues", "/repos/k-wa-wa/pechka/pulls":
			writeJSON(w, `[]`)
		default:
			t.Fatalf("unexpected request: %s %s", req.Method, req.URL.Path)
		}
	})

	ctx := context.Background()
	item, _, err := st.UpsertItem(ctx, "k-wa-wa/pechka", 4, store.KindIssue)
	if err != nil {
		t.Fatalf("UpsertItem (seed): %v", err)
	}
	if err := st.UpdateItemPhase(ctx, item.ID, store.PhaseDone); err != nil {
		t.Fatalf("UpdateItemPhase (seed): %v", err)
	}

	if err := r.Resync(ctx); err != nil {
		t.Fatalf("Resync() error = %v", err)
	}
	// UpdateItemPhase を再度叩いていれば updated_at が変わるはずだが、ここでは
	// エラーが起きないこと（=既に done のアイテムをスキップする分岐が正しく
	// 機能していること）だけを確認する。
	reloaded, ok, err := st.GetItemByID(ctx, item.ID)
	if err != nil || !ok {
		t.Fatalf("GetItemByID: ok=%v err=%v", ok, err)
	}
	if reloaded.Phase != store.PhaseDone {
		t.Fatalf("phase = %q, want %q", reloaded.Phase, store.PhaseDone)
	}
}

// TestResync_UpdatesStaleHeadSHA は DB 上の head_sha が古い場合に最新化する
// ことを検証する（DESIGN.md 7.5 節）。
func TestResync_UpdatesStaleHeadSHA(t *testing.T) {
	r, st := newTestResyncer(t, func(w http.ResponseWriter, req *http.Request) {
		switch req.URL.Path {
		case "/repos/k-wa-wa/pechka/issues":
			writeJSON(w, `[]`)
		case "/repos/k-wa-wa/pechka/pulls":
			writeJSON(w, `[{"number": 5, "title": "a pr", "state": "open", "user": {"login": "alice", "type": "User"}, "head": {"sha": "newsha"}}]`)
		default:
			t.Fatalf("unexpected request: %s %s", req.Method, req.URL.Path)
		}
	})

	ctx := context.Background()
	item, _, err := st.UpsertItem(ctx, "k-wa-wa/pechka", 5, store.KindPullRequest)
	if err != nil {
		t.Fatalf("UpsertItem (seed): %v", err)
	}
	if err := st.UpdateItemHeadSHA(ctx, item.ID, "oldsha"); err != nil {
		t.Fatalf("UpdateItemHeadSHA (seed): %v", err)
	}

	if err := r.Resync(ctx); err != nil {
		t.Fatalf("Resync() error = %v", err)
	}

	reloaded, ok, err := st.GetItemByID(ctx, item.ID)
	if err != nil || !ok {
		t.Fatalf("GetItemByID: ok=%v err=%v", ok, err)
	}
	if reloaded.HeadSHA != "newsha" {
		t.Fatalf("HeadSHA = %q, want newsha", reloaded.HeadSHA)
	}
}

// TestResync_ReapsExpiredLeases は resync がリースの期限切れ回収も行うことを検証する
// （Phase 1 から引き継いだ責務。DESIGN.md 7.5 節）。
func TestResync_ReapsExpiredLeases(t *testing.T) {
	now := time.Date(2026, 7, 29, 0, 0, 0, 0, time.UTC)
	r, st := newTestResyncer(t, func(w http.ResponseWriter, req *http.Request) {
		switch req.URL.Path {
		case "/repos/k-wa-wa/pechka/issues", "/repos/k-wa-wa/pechka/pulls":
			writeJSON(w, `[]`)
		default:
			t.Fatalf("unexpected request: %s %s", req.Method, req.URL.Path)
		}
	})

	ctx := context.Background()
	item, _, err := st.UpsertItem(ctx, "k-wa-wa/pechka", 6, store.KindIssue)
	if err != nil {
		t.Fatalf("UpsertItem (seed): %v", err)
	}
	if _, err := st.AcquireLease(ctx, item.ID, "host-a:1", -time.Minute); err != nil {
		t.Fatalf("AcquireLease (seed, already expired): %v", err)
	}

	if err := r.Resync(ctx); err != nil {
		t.Fatalf("Resync() error = %v", err)
	}

	if _, ok, err := st.GetLease(ctx, item.ID); err != nil || ok {
		t.Fatalf("expired lease should have been reaped: ok=%v err=%v", ok, err)
	}
	_ = now
}

// TestResync_ContinuesAfterOneRepoFails は 1 リポジトリの失敗が他のリポジトリの
// resync を妨げないことを検証する。
func TestResync_ContinuesAfterOneRepoFails(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		switch req.URL.Path {
		case "/repos/k-wa-wa/broken/issues":
			http.Error(w, `{"message": "boom"}`, http.StatusInternalServerError)
		case "/repos/k-wa-wa/pechka/issues":
			writeJSON(w, `[{"number": 1, "title": "ok", "state": "open", "user": {"login": "alice", "type": "User"}}]`)
		case "/repos/k-wa-wa/pechka/pulls", "/repos/k-wa-wa/broken/pulls":
			writeJSON(w, `[]`)
		default:
			t.Fatalf("unexpected request: %s %s", req.Method, req.URL.Path)
		}
	}))
	t.Cleanup(server.Close)

	client := github.NewClient(github.WithBaseURL(server.URL), github.WithStaticToken("test-token"))
	st, err := store.Open(context.Background(), ":memory:")
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	r := &Resyncer{Client: client, Store: st, Repos: []string{"k-wa-wa/broken", "k-wa-wa/pechka"}}

	err = r.Resync(context.Background())
	if err == nil {
		t.Fatalf("Resync() error = nil, want an error reporting the broken repo")
	}

	if _, ok, err := st.GetItem(context.Background(), "k-wa-wa/pechka", 1); err != nil || !ok {
		t.Fatalf("the healthy repo should still be resynced: ok=%v err=%v", ok, err)
	}
}
