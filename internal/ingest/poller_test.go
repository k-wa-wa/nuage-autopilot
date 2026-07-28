package ingest

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"autopilot/internal/github"
	"autopilot/internal/store"
)

func newTestPoller(t *testing.T, handler http.HandlerFunc, opts ...func(*Poller)) (*Poller, *store.Store) {
	t.Helper()

	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)

	client := github.NewClient(github.WithBaseURL(server.URL), github.WithStaticToken("test-token"))

	st, err := store.Open(context.Background(), ":memory:")
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	p := &Poller{
		Client: client,
		Store:  st,
		Repos:  []string{"k-wa-wa/pechka"},
	}
	for _, opt := range opts {
		opt(p)
	}
	return p, st
}

func writeJSON(w http.ResponseWriter, body string) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(body))
}

// TestPoll_NotModifiedDoesNothing は 304 応答時に events が一切増えず、
// カーソルも更新されないことを検証する（DESIGN.md 7.2 節: rate limit を
// 消費しない）。
func TestPoll_NotModifiedDoesNothing(t *testing.T) {
	calls := 0
	p, st := newTestPoller(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/user":
			writeJSON(w, `{"login": "nuage-autopilot"}`)
		case "/notifications":
			calls++
			w.WriteHeader(http.StatusNotModified)
		default:
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
	})

	n, err := p.Poll(context.Background())
	if err != nil {
		t.Fatalf("Poll() error = %v", err)
	}
	if n != 0 {
		t.Fatalf("enqueued = %d, want 0", n)
	}
	if calls != 1 {
		t.Fatalf("notifications calls = %d, want 1", calls)
	}
	if _, ok, err := st.GetCursor(context.Background(), notificationsSource); err != nil || ok {
		t.Fatalf("cursor should not be saved on a 304: ok=%v err=%v", ok, err)
	}
}

// TestPoll_BootstrapDoesNotIgniteExistingItems は、カーソルが一度も保存されて
// いない最初のポーリング（＝既存の未読通知の棚卸し）では、新規に見つけた
// アイテムを DB に登録するだけで "opened" イベントを起こさないことを検証する
// （DESIGN.md 7.6 節）。
func TestPoll_BootstrapDoesNotIgniteExistingItems(t *testing.T) {
	p, st := newTestPoller(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/user":
			writeJSON(w, `{"login": "nuage-autopilot"}`)
		case r.URL.Path == "/notifications":
			writeJSON(w, `[{"id": "1", "unread": true, "reason": "subscribed", "updated_at": "2026-07-01T00:00:00Z",
				"subject": {"title": "old issue", "url": "https://api.github.com/repos/k-wa-wa/pechka/issues/42", "type": "Issue"},
				"repository": {"full_name": "k-wa-wa/pechka"}}]`)
		case r.URL.Path == "/repos/k-wa-wa/pechka/issues/42":
			writeJSON(w, `{"number": 42, "title": "old issue", "body": "written long ago", "state": "open",
				"user": {"login": "alice", "type": "User"}, "created_at": "2020-01-01T00:00:00Z"}`)
		default:
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
	})

	n, err := p.Poll(context.Background())
	if err != nil {
		t.Fatalf("Poll() error = %v", err)
	}
	if n != 0 {
		t.Fatalf("enqueued = %d, want 0 (bootstrap must not ignite)", n)
	}

	item, ok, err := st.GetItem(context.Background(), "k-wa-wa/pechka", 42)
	if err != nil || !ok {
		t.Fatalf("item should be registered: ok=%v err=%v", ok, err)
	}
	if item.Phase != store.PhaseNew {
		t.Fatalf("phase = %q, want %q", item.Phase, store.PhaseNew)
	}
	if item.LastSeenAt == nil {
		t.Fatalf("LastSeenAt should be baselined even without an event")
	}

	if n, err := st.CountUnprocessedEvents(context.Background()); err != nil || n != 0 {
		t.Fatalf("CountUnprocessedEvents = %d, err=%v, want 0", n, err)
	}
}

// TestPoll_GenuinelyNewIssueEnqueuesOpenedEvent は、前回のポーリング以降に
// 作成された Issue（＝真に新規）については "opened" イベントを起こすことを
// 検証する。
func TestPoll_GenuinelyNewIssueEnqueuesOpenedEvent(t *testing.T) {
	p, st := newTestPoller(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/user":
			writeJSON(w, `{"login": "nuage-autopilot"}`)
		case r.URL.Path == "/notifications":
			writeJSON(w, `[{"id": "1", "unread": true, "reason": "subscribed", "updated_at": "2026-07-29T00:00:00Z",
				"subject": {"title": "new issue", "url": "https://api.github.com/repos/k-wa-wa/pechka/issues/99", "type": "Issue"},
				"repository": {"full_name": "k-wa-wa/pechka"}}]`)
		case r.URL.Path == "/repos/k-wa-wa/pechka/issues/99":
			writeJSON(w, `{"number": 99, "title": "please add X", "body": "want X added", "state": "open",
				"user": {"login": "alice", "type": "User"}, "created_at": "2026-07-29T00:00:00Z"}`)
		default:
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
	})

	// 前回のポーリングが「昔」成功していたことにする。
	if err := st.SaveCursor(context.Background(), notificationsSource, "", "", "2026-01-01T00:00:00Z"); err != nil {
		t.Fatalf("SaveCursor (seed): %v", err)
	}

	n, err := p.Poll(context.Background())
	if err != nil {
		t.Fatalf("Poll() error = %v", err)
	}
	if n != 1 {
		t.Fatalf("enqueued = %d, want 1", n)
	}

	item, ok, err := st.GetItem(context.Background(), "k-wa-wa/pechka", 99)
	if err != nil || !ok {
		t.Fatalf("item should be registered: ok=%v err=%v", ok, err)
	}

	ev, ok, err := st.NextUnprocessedEvent(context.Background())
	if err != nil || !ok {
		t.Fatalf("NextUnprocessedEvent: ok=%v err=%v", ok, err)
	}
	if ev.Type != "opened" || ev.ItemID != item.ID || ev.Actor != "alice" || ev.Body != "want X added" {
		t.Fatalf("event = %+v, want type=opened actor=alice body=%q", ev, "want X added")
	}
}

// TestPoll_RejectsDisallowedAuthor は NUAGE_ALLOWED_AUTHORS に該当しない作成者の
// アイテムを DB に登録しないことを検証する。
func TestPoll_RejectsDisallowedAuthor(t *testing.T) {
	p, st := newTestPoller(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/user":
			writeJSON(w, `{"login": "nuage-autopilot"}`)
		case r.URL.Path == "/notifications":
			writeJSON(w, `[{"id": "1", "unread": true, "reason": "subscribed", "updated_at": "2026-07-29T00:00:00Z",
				"subject": {"title": "spam", "url": "https://api.github.com/repos/k-wa-wa/pechka/issues/1", "type": "Issue"},
				"repository": {"full_name": "k-wa-wa/pechka"}}]`)
		case r.URL.Path == "/repos/k-wa-wa/pechka/issues/1":
			writeJSON(w, `{"number": 1, "title": "spam", "body": "buy now", "state": "open",
				"user": {"login": "random-stranger", "type": "User"}, "created_at": "2026-07-29T00:00:00Z"}`)
		default:
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
	}, func(p *Poller) { p.AllowedAuthors = []string{"k-wa-wa"} })

	if err := st.SaveCursor(context.Background(), notificationsSource, "", "", "2026-01-01T00:00:00Z"); err != nil {
		t.Fatalf("SaveCursor (seed): %v", err)
	}

	n, err := p.Poll(context.Background())
	if err != nil {
		t.Fatalf("Poll() error = %v", err)
	}
	if n != 0 {
		t.Fatalf("enqueued = %d, want 0", n)
	}
	if _, ok, err := st.GetItem(context.Background(), "k-wa-wa/pechka", 1); err != nil || ok {
		t.Fatalf("disallowed item should not be registered: ok=%v err=%v", ok, err)
	}
}

// TestPoll_RejectsIgnoreLabel は agent:ignore ラベルが付いたアイテムを
// DB に登録しないことを検証する。
func TestPoll_RejectsIgnoreLabel(t *testing.T) {
	p, st := newTestPoller(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/user":
			writeJSON(w, `{"login": "nuage-autopilot"}`)
		case r.URL.Path == "/notifications":
			writeJSON(w, `[{"id": "1", "unread": true, "reason": "subscribed", "updated_at": "2026-07-29T00:00:00Z",
				"subject": {"title": "manual only", "url": "https://api.github.com/repos/k-wa-wa/pechka/issues/2", "type": "Issue"},
				"repository": {"full_name": "k-wa-wa/pechka"}}]`)
		case r.URL.Path == "/repos/k-wa-wa/pechka/issues/2":
			writeJSON(w, `{"number": 2, "title": "manual only", "body": "do not touch", "state": "open",
				"user": {"login": "alice", "type": "User"}, "labels": [{"name": "agent:ignore"}], "created_at": "2026-07-29T00:00:00Z"}`)
		default:
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
	})

	if err := st.SaveCursor(context.Background(), notificationsSource, "", "", "2026-01-01T00:00:00Z"); err != nil {
		t.Fatalf("SaveCursor (seed): %v", err)
	}

	n, err := p.Poll(context.Background())
	if err != nil {
		t.Fatalf("Poll() error = %v", err)
	}
	if n != 0 {
		t.Fatalf("enqueued = %d, want 0", n)
	}
	if _, ok, err := st.GetItem(context.Background(), "k-wa-wa/pechka", 2); err != nil || ok {
		t.Fatalf("ignored item should not be registered: ok=%v err=%v", ok, err)
	}
}

// TestPoll_SkipsThreadsOutsideConfiguredRepos は Repos に含まれないリポジトリの
// 通知を無視することを検証する。
func TestPoll_SkipsThreadsOutsideConfiguredRepos(t *testing.T) {
	p, _ := newTestPoller(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/user":
			writeJSON(w, `{"login": "nuage-autopilot"}`)
		case "/notifications":
			writeJSON(w, `[{"id": "1", "unread": true, "reason": "subscribed", "updated_at": "2026-07-29T00:00:00Z",
				"subject": {"title": "unrelated", "url": "https://api.github.com/repos/someone-else/other-repo/issues/1", "type": "Issue"},
				"repository": {"full_name": "someone-else/other-repo"}}]`)
		default:
			t.Fatalf("unexpected request (repo not configured should not trigger a detail fetch): %s %s", r.Method, r.URL.Path)
		}
	})

	n, err := p.Poll(context.Background())
	if err != nil {
		t.Fatalf("Poll() error = %v", err)
	}
	if n != 0 {
		t.Fatalf("enqueued = %d, want 0", n)
	}
}

// TestPoll_SkipsNonIssuePRSubjectTypes は Issue/PullRequest 以外の通知
// （Discussion, Commit 等）を無視することを検証する。
func TestPoll_SkipsNonIssuePRSubjectTypes(t *testing.T) {
	p, _ := newTestPoller(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/user":
			writeJSON(w, `{"login": "nuage-autopilot"}`)
		case "/notifications":
			writeJSON(w, `[{"id": "1", "unread": true, "reason": "subscribed", "updated_at": "2026-07-29T00:00:00Z",
				"subject": {"title": "a discussion", "url": "https://api.github.com/repos/k-wa-wa/pechka/discussions/1", "type": "Discussion"},
				"repository": {"full_name": "k-wa-wa/pechka"}}]`)
		default:
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
	})

	n, err := p.Poll(context.Background())
	if err != nil {
		t.Fatalf("Poll() error = %v", err)
	}
	if n != 0 {
		t.Fatalf("enqueued = %d, want 0", n)
	}
}

// TestPoll_ExistingItemEnqueuesNewCommentAndFiltersSelf は、既存アイテムについて
// last_seen_at 以降の新着コメントのみを "commented" として enqueue し、
// 自分自身の投稿は無視することを検証する（DESIGN.md 7.3 節）。
func TestPoll_ExistingItemEnqueuesNewCommentAndFiltersSelf(t *testing.T) {
	p, st := newTestPoller(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/user":
			writeJSON(w, `{"login": "nuage-autopilot"}`)
		case r.URL.Path == "/notifications":
			writeJSON(w, `[{"id": "1", "unread": true, "reason": "subscribed", "updated_at": "2026-07-29T00:00:00Z",
				"subject": {"title": "ongoing", "url": "https://api.github.com/repos/k-wa-wa/pechka/issues/5", "type": "Issue"},
				"repository": {"full_name": "k-wa-wa/pechka"}}]`)
		case r.URL.Path == "/repos/k-wa-wa/pechka/issues/5/comments":
			writeJSON(w, `[
				{"id": 200, "body": "still working on it", "user": {"login": "nuage-autopilot", "type": "User"}, "created_at": "2026-07-29T01:00:00Z"},
				{"id": 201, "body": "here is my answer", "user": {"login": "alice", "type": "User"}, "created_at": "2026-07-29T02:00:00Z"}
			]`)
		default:
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
	})

	ctx := context.Background()
	item, _, err := st.UpsertItem(ctx, "k-wa-wa/pechka", 5, store.KindIssue)
	if err != nil {
		t.Fatalf("UpsertItem (seed): %v", err)
	}
	if err := st.UpdateItemPhase(ctx, item.ID, store.PhaseAwaitingAnswer); err != nil {
		t.Fatalf("UpdateItemPhase (seed): %v", err)
	}
	if err := st.UpdateItemLastSeenAt(ctx, item.ID, time.Date(2026, 7, 29, 0, 0, 0, 0, time.UTC)); err != nil {
		t.Fatalf("UpdateItemLastSeenAt (seed): %v", err)
	}

	n, err := p.Poll(ctx)
	if err != nil {
		t.Fatalf("Poll() error = %v", err)
	}
	if n != 1 {
		t.Fatalf("enqueued = %d, want 1 (self-comment must be filtered)", n)
	}

	ev, ok, err := st.NextUnprocessedEvent(ctx)
	if err != nil || !ok {
		t.Fatalf("NextUnprocessedEvent: ok=%v err=%v", ok, err)
	}
	if ev.Type != "commented" || ev.Actor != "alice" || ev.Body != "here is my answer" {
		t.Fatalf("event = %+v, want type=commented actor=alice", ev)
	}

	reloaded, ok, err := st.GetItemByID(ctx, item.ID)
	if err != nil || !ok {
		t.Fatalf("GetItemByID: ok=%v err=%v", ok, err)
	}
	want := time.Date(2026, 7, 29, 2, 0, 0, 0, time.UTC)
	if reloaded.LastSeenAt == nil || !reloaded.LastSeenAt.Equal(want) {
		t.Fatalf("LastSeenAt = %v, want %v (must advance past the newest comment, including the filtered self-comment)", reloaded.LastSeenAt, want)
	}
}

// TestPoll_ExistingItemWithNilLastSeenAtBaselinesWithoutEvent は、resync が
// last_seen_at を設定しないまま登録したアイテムを poller が初めて処理する場合、
// 全既存コメントをイベント化せず静かにベースラインすることを検証する。
func TestPoll_ExistingItemWithNilLastSeenAtBaselinesWithoutEvent(t *testing.T) {
	p, st := newTestPoller(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/user":
			writeJSON(w, `{"login": "nuage-autopilot"}`)
		case "/notifications":
			writeJSON(w, `[{"id": "1", "unread": true, "reason": "subscribed", "updated_at": "2026-07-29T00:00:00Z",
				"subject": {"title": "resync discovered", "url": "https://api.github.com/repos/k-wa-wa/pechka/issues/6", "type": "Issue"},
				"repository": {"full_name": "k-wa-wa/pechka"}}]`)
		default:
			t.Fatalf("unexpected request (comments must not be fetched without a baseline): %s %s", r.Method, r.URL.Path)
		}
	})

	ctx := context.Background()
	if _, _, err := st.UpsertItem(ctx, "k-wa-wa/pechka", 6, store.KindIssue); err != nil {
		t.Fatalf("UpsertItem (seed): %v", err)
	}

	n, err := p.Poll(ctx)
	if err != nil {
		t.Fatalf("Poll() error = %v", err)
	}
	if n != 0 {
		t.Fatalf("enqueued = %d, want 0", n)
	}

	item, ok, err := st.GetItem(ctx, "k-wa-wa/pechka", 6)
	if err != nil || !ok {
		t.Fatalf("GetItem: ok=%v err=%v", ok, err)
	}
	if item.LastSeenAt == nil {
		t.Fatalf("LastSeenAt should now be baselined")
	}
}

// TestPollCheckRuns_EnqueuesOnceThenDedupsUntilShaChanges は CI 判定が確定した
// 場合にのみイベントを起こし、同じ (sha, status) の組み合わせでは重複させない
// ことを検証する（DESIGN.md 7.4 節）。
func TestPollCheckRuns_EnqueuesOnceThenDedupsUntilShaChanges(t *testing.T) {
	state := "success"
	p, st := newTestPoller(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/user":
			writeJSON(w, `{"login": "nuage-autopilot"}`)
		case r.URL.Path == "/notifications":
			w.WriteHeader(http.StatusNotModified)
		case r.URL.Path == "/repos/k-wa-wa/pechka/commits/deadbeef/check-runs":
			writeJSON(w, fmt.Sprintf(`{"total_count": 1, "check_runs": [{"name": "test", "status": "completed", "conclusion": %q}]}`,
				map[string]string{"success": "success", "failure": "failure"}[state]))
		default:
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
	})

	ctx := context.Background()
	item, _, err := st.UpsertItem(ctx, "k-wa-wa/pechka", 10, store.KindPullRequest)
	if err != nil {
		t.Fatalf("UpsertItem (seed): %v", err)
	}
	if err := st.UpdateItemPhase(ctx, item.ID, store.PhaseInReview); err != nil {
		t.Fatalf("UpdateItemPhase (seed): %v", err)
	}
	if err := st.UpdateItemHeadSHA(ctx, item.ID, "deadbeef"); err != nil {
		t.Fatalf("UpdateItemHeadSHA (seed): %v", err)
	}

	n, err := p.Poll(ctx)
	if err != nil {
		t.Fatalf("Poll() (1st) error = %v", err)
	}
	if n != 1 {
		t.Fatalf("enqueued (1st) = %d, want 1", n)
	}
	ev, ok, err := st.NextUnprocessedEvent(ctx)
	if err != nil || !ok {
		t.Fatalf("NextUnprocessedEvent: ok=%v err=%v", ok, err)
	}
	if ev.Type != "ci_success" {
		t.Fatalf("event type = %q, want ci_success", ev.Type)
	}
	if err := st.MarkEventProcessed(ctx, ev.ID); err != nil {
		t.Fatalf("MarkEventProcessed: %v", err)
	}

	// 同じ sha・同じ状態のまま再ポーリングしても重複しない。
	n, err = p.Poll(ctx)
	if err != nil {
		t.Fatalf("Poll() (2nd) error = %v", err)
	}
	if n != 0 {
		t.Fatalf("enqueued (2nd) = %d, want 0 (dedup must suppress repeat)", n)
	}
}
