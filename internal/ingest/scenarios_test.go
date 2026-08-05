package ingest

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"autopilot/internal/github"
	"autopilot/internal/store"
)

/*
シナリオテストの直交マトリクス（DESIGN.md 7.2〜7.6 節）

4 つの軸（① Actor × ② Action × ③ 初期状態 × ④ 発生順序）の組み合わせによる全シナリオ検証一覧：

1. 新規登録 & 起票フェーズ (Unregistered / New)
----------------------------------------------------------------------------------------------------------------------
| ID    | 初期状態      | 操作順序 (Sequence)           | Actor           | 判定期待値  | 目的・検証内容
----------------------------------------------------------------------------------------------------------------------
| S-101 | Unregistered | Open -> Poll                 | AllowedUser     | 発火 (1件) | 許可ユーザーの新規起票で opened 発火
| S-102 | Unregistered | Open -> Poll                 | DisallowedUser  | 無視 (0件) | 未許可ユーザーの起票無視
| S-103 | Unregistered | Open -> Poll                 | BotUser         | 無視 (0件) | Bot自身の起票で無限ループ防止
| S-104 | Unregistered | Resync -> Poll               | N/A             | 無視 (0件) | resync による初回発見では着火しない
| S-105 | Unregistered | Resync -> Comment -> Poll    | AllowedUser     | 発火 (1件) | resync登録済みアイテムへの人間コメントで commented 発火
| S-106 | Unregistered | Resync -> Comment -> Poll    | BotUser         | 無視 (0件) | resync登録済みアイテムへのBotコメントは無視

2. 作業中 & コメント対話フェーズ (New / InReview)
----------------------------------------------------------------------------------------------------------------------
| ID    | 初期状態      | 操作順序 (Sequence)           | Actor           | 判定期待値  | 目的・検証内容
----------------------------------------------------------------------------------------------------------------------
| S-201 | New/InReview | Comment -> Poll              | AllowedUser     | 発火 (1件) | 登録済みアイテムへの許可ユーザーコメント発火
| S-202 | New/InReview | Comment -> Poll              | BotUser         | 無視 (0件) | 登録済みアイテムへの Bot コメント無視
| S-203 | New/InReview | Comment -> Poll              | DisallowedUser  | 無視 (0件) | 登録済みアイテムへの未許可ユーザーコメント無視
| S-204 | InReview     | Review -> Poll               | AllowedUser     | 発火 (1件) | 人間による PR レビュー投稿で reviewed 発火
| S-205 | InReview     | Push -> CI Failure -> Poll   | System/CI       | 発火 (1件) | in_review PR での CI 失敗発火 (ci_failure)
| S-206 | InReview     | Push -> CI Success -> Poll   | System/CI       | 発火 (1件) | in_review PR での CI 成功発火 (ci_success)

3. クローズ & 完了フェーズ (Done)
----------------------------------------------------------------------------------------------------------------------
| ID    | 初期状態      | 操作順序 (Sequence)           | Actor           | 判定期待値  | 目的・検証内容
----------------------------------------------------------------------------------------------------------------------
| S-301 | InReview     | Close -> Resync              | AllowedUser     | 状態更新   | Close されたアイテムは resync で phase=done へ
| S-302 | Done         | Comment -> Poll              | AllowedUser     | 発火 (1件) | done アイテムへの人間コメントもイベント化 (resume対応)
*/

// scenarioEnv はシナリオテストの実行環境（モック API・SQLite DB・Poller/Resyncer）を管理する。
// アクター・状態・アクション・順序の多様な組み合わせを簡潔かつ明確に検証するために
// シナリオヘルパーを提供する（DESIGN.md 7章参照）。
type scenarioEnv struct {
	t        *testing.T
	ctx      context.Context
	store    *store.Store
	poller   *Poller
	resyncer *Resyncer

	// モックレスポンス制御用
	botLogin       string
	allowedAuthors []string

	issuesResponse        string
	pullsResponse         string
	notificationsResponse string
	issueDetailResponses  map[int]string
	pullDetailResponses   map[int]string
	commentsResponses     map[int]string
	pullReviewsResponses  map[int]string
	checkStateResponses   map[string]string
}

func newScenarioEnv(t *testing.T) *scenarioEnv {
	t.Helper()
	ctx := context.Background()

	env := &scenarioEnv{
		t:                    t,
		ctx:                  ctx,
		botLogin:             "nuage-autopilot",
		allowedAuthors:       []string{"alice", "bob"},
		issueDetailResponses: make(map[int]string),
		pullDetailResponses:  make(map[int]string),
		commentsResponses:    make(map[int]string),
		pullReviewsResponses: make(map[int]string),
		checkStateResponses:  make(map[string]string),
	}

	st, err := store.Open(ctx, ":memory:")
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	env.store = st

	server := httptest.NewServer(http.HandlerFunc(env.handleHTTP))
	t.Cleanup(server.Close)

	client := github.NewClient(github.WithBaseURL(server.URL), github.WithStaticToken("test-token"))

	env.poller = &Poller{
		Client:         client,
		Store:          st,
		Repos:          []string{"k-wa-wa/pechka"},
		AllowedAuthors: env.allowedAuthors,
	}

	env.resyncer = &Resyncer{
		Client:         client,
		Store:          st,
		Repos:          []string{"k-wa-wa/pechka"},
		AllowedAuthors: env.allowedAuthors,
	}

	return env
}

func (e *scenarioEnv) handleHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	if strings.HasPrefix(r.URL.Path, "/repos/k-wa-wa/pechka/commits/") && strings.HasSuffix(r.URL.Path, "/check-runs") {
		parts := strings.Split(r.URL.Path, "/")
		if len(parts) >= 6 {
			sha := parts[5]
			if state, ok := e.checkStateResponses[sha]; ok {
				_, _ = fmt.Fprintf(w, `{"total_count": 1, "check_runs": [{"status": "completed", "conclusion": %q}]}`, state)
				return
			}
		}
		_, _ = w.Write([]byte(`{"total_count": 0, "check_runs": []}`))
		return
	}

	switch {
	case r.URL.Path == "/user":
		_, _ = fmt.Fprintf(w, `{"login": %q}`, e.botLogin)
	case r.URL.Path == "/repos/k-wa-wa/pechka/issues":
		if e.issuesResponse != "" {
			_, _ = w.Write([]byte(e.issuesResponse))
		} else {
			_, _ = w.Write([]byte("[]"))
		}
	case r.URL.Path == "/repos/k-wa-wa/pechka/pulls":
		if e.pullsResponse != "" {
			_, _ = w.Write([]byte(e.pullsResponse))
		} else {
			_, _ = w.Write([]byte("[]"))
		}
	case r.URL.Path == "/notifications":
		if e.notificationsResponse != "" {
			_, _ = w.Write([]byte(e.notificationsResponse))
		} else {
			w.WriteHeader(http.StatusNotModified)
		}
	default:
		var num int
		if _, err := fmt.Sscanf(r.URL.Path, "/repos/k-wa-wa/pechka/pulls/%d/reviews", &num); err == nil {
			if resp, ok := e.pullReviewsResponses[num]; ok {
				_, _ = w.Write([]byte(resp))
				return
			}
			_, _ = w.Write([]byte("[]"))
			return
		}
		if _, err := fmt.Sscanf(r.URL.Path, "/repos/k-wa-wa/pechka/issues/%d/comments", &num); err == nil {
			if resp, ok := e.commentsResponses[num]; ok {
				_, _ = w.Write([]byte(resp))
				return
			}
			_, _ = w.Write([]byte("[]"))
			return
		}
		if _, err := fmt.Sscanf(r.URL.Path, "/repos/k-wa-wa/pechka/pulls/%d/comments", &num); err == nil {
			_, _ = w.Write([]byte("[]"))
			return
		}
		if _, err := fmt.Sscanf(r.URL.Path, "/repos/k-wa-wa/pechka/issues/%d", &num); err == nil {
			if resp, ok := e.issueDetailResponses[num]; ok {
				_, _ = w.Write([]byte(resp))
				return
			}
		}
		if _, err := fmt.Sscanf(r.URL.Path, "/repos/k-wa-wa/pechka/pulls/%d", &num); err == nil {
			if resp, ok := e.pullDetailResponses[num]; ok {
				_, _ = w.Write([]byte(resp))
				return
			}
		}

		e.t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
	}
}

// TestIngestScenarios は、Actor × 状態 × Action × 順序の直交マトリクスに基づく
// 体系的な全シナリオテスト群である（DESIGN.md 7.2〜7.6 節）。
func TestIngestScenarios(t *testing.T) {
	// S-101: 許可ユーザーによる新規起票で opened イベントが発生すること
	t.Run("S-101: 許可ユーザーによる新規起票で opened 発火", func(t *testing.T) {
		env := newScenarioEnv(t)
		if err := env.store.SaveCursor(env.ctx, notificationsSource, "", "", "2026-01-01T00:00:00Z"); err != nil {
			t.Fatalf("SaveCursor: %v", err)
		}

		env.notificationsResponse = `[{"id": "1", "updated_at": "2026-08-01T10:00:00Z",
			"subject": {"title": "new issue", "url": "https://api.github.com/repos/k-wa-wa/pechka/issues/1", "type": "Issue"},
			"repository": {"full_name": "k-wa-wa/pechka"}}]`
		env.issueDetailResponses[1] = `{"number": 1, "title": "new issue", "body": "hello", "state": "open",
			"user": {"login": "alice", "type": "User"}, "created_at": "2026-08-01T10:00:00Z"}`

		n, err := env.poller.Poll(env.ctx)
		if err != nil || n != 1 {
			t.Fatalf("Poll() = %d, err = %v, want 1", n, err)
		}
		ev, ok, err := env.store.NextUnprocessedEvent(env.ctx)
		if err != nil || !ok || ev.Type != "opened" || ev.Actor != "alice" {
			t.Fatalf("event = %+v, want opened by alice", ev)
		}
	})

	// S-102: 未許可ユーザーの起票は無視されること
	t.Run("S-102: 未許可ユーザーの起票は無視", func(t *testing.T) {
		env := newScenarioEnv(t)
		if err := env.store.SaveCursor(env.ctx, notificationsSource, "", "", "2026-01-01T00:00:00Z"); err != nil {
			t.Fatalf("SaveCursor: %v", err)
		}

		env.notificationsResponse = `[{"id": "1", "updated_at": "2026-08-01T10:00:00Z",
			"subject": {"title": "stranger issue", "url": "https://api.github.com/repos/k-wa-wa/pechka/issues/2", "type": "Issue"},
			"repository": {"full_name": "k-wa-wa/pechka"}}]`
		env.issueDetailResponses[2] = `{"number": 2, "title": "stranger issue", "body": "spam", "state": "open",
			"user": {"login": "stranger", "type": "User"}, "created_at": "2026-08-01T10:00:00Z"}`

		n, err := env.poller.Poll(env.ctx)
		if err != nil || n != 0 {
			t.Fatalf("Poll() = %d, err = %v, want 0", n, err)
		}
	})

	// S-103: Bot 自身の起票は opened イベントを起こさないこと（無限ループ防止）
	t.Run("S-103: Bot 自身の起票はイベント化しない", func(t *testing.T) {
		env := newScenarioEnv(t)
		if err := env.store.SaveCursor(env.ctx, notificationsSource, "", "", "2026-01-01T00:00:00Z"); err != nil {
			t.Fatalf("SaveCursor: %v", err)
		}

		env.notificationsResponse = `[{"id": "1", "updated_at": "2026-08-01T10:00:00Z",
			"subject": {"title": "bot issue", "url": "https://api.github.com/repos/k-wa-wa/pechka/issues/3", "type": "Issue"},
			"repository": {"full_name": "k-wa-wa/pechka"}}]`
		env.issueDetailResponses[3] = `{"number": 3, "title": "bot issue", "body": "auto", "state": "open",
			"user": {"login": "nuage-autopilot", "type": "User"}, "created_at": "2026-08-01T10:00:00Z"}`

		n, err := env.poller.Poll(env.ctx)
		if err != nil || n != 0 {
			t.Fatalf("Poll() = %d, err = %v, want 0", n, err)
		}
	})

	// S-104: resync による未登録アイテムの初回発見時はイベントを発行しないこと
	t.Run("S-104: resync による初回発見ではイベントを起こさない", func(t *testing.T) {
		env := newScenarioEnv(t)
		env.issuesResponse = `[{"number": 4, "title": "existing", "state": "open", "body": "old",
			"user": {"login": "alice", "type": "User"}, "created_at": "2026-07-01T00:00:00Z"}]`

		if err := env.resyncer.Resync(env.ctx); err != nil {
			t.Fatalf("Resync() = %v", err)
		}

		count, err := env.store.CountUnprocessedEvents(env.ctx)
		if err != nil || count != 0 {
			t.Fatalf("CountUnprocessedEvents() = %d, want 0", count)
		}

		item, ok, err := env.store.GetItem(env.ctx, "k-wa-wa/pechka", 4)
		if err != nil || !ok || item.LastSeenAt == nil {
			t.Fatalf("item must be registered with last_seen_at set: ok=%v, item=%+v", ok, item)
		}
	})

	// S-105: resync 登録済みアイテムに対して人間が投稿した新着コメントが発火すること（再発防止テスト）
	t.Run("S-105: resync 登録済みアイテムへの人間コメントで commented 発火", func(t *testing.T) {
		env := newScenarioEnv(t)
		env.pullsResponse = `[{"number": 5, "title": "PR 5", "state": "open", "body": "pr body",
			"user": {"login": "alice", "type": "User"}, "head": {"sha": "sha5"}, "created_at": "2026-08-01T00:00:00Z"}]`

		if err := env.resyncer.Resync(env.ctx); err != nil {
			t.Fatalf("Resync() = %v", err)
		}

		if err := env.store.SaveCursor(env.ctx, notificationsSource, "", "", "2026-01-01T00:00:00Z"); err != nil {
			t.Fatalf("SaveCursor: %v", err)
		}

		env.notificationsResponse = `[{"id": "5", "updated_at": "2026-08-05T10:00:00Z",
			"subject": {"title": "PR 5", "url": "https://api.github.com/repos/k-wa-wa/pechka/pulls/5", "type": "PullRequest"},
			"repository": {"full_name": "k-wa-wa/pechka"}}]`
		env.pullDetailResponses[5] = `{"number": 5, "title": "PR 5", "body": "pr body", "state": "open",
			"user": {"login": "alice", "type": "User"}, "head": {"sha": "sha5"}, "created_at": "2026-08-01T00:00:00Z"}`
		env.commentsResponses[5] = `[{"id": 501, "body": "please fix this", "user": {"login": "alice", "type": "User"}, "created_at": "2026-08-05T09:50:00Z"}]`

		n, err := env.poller.Poll(env.ctx)
		if err != nil || n != 1 {
			t.Fatalf("Poll() = %d, err = %v, want 1", n, err)
		}

		ev, ok, err := env.store.NextUnprocessedEvent(env.ctx)
		if err != nil || !ok || ev.Type != "commented" || ev.Actor != "alice" || ev.Body != "please fix this" {
			t.Fatalf("event = %+v, want commented by alice", ev)
		}
	})

	// S-106: resync 登録済みアイテムに対して Bot 自身がコメントした場合はスルーされること
	t.Run("S-106: resync 登録済みアイテムへの Bot コメントは無視", func(t *testing.T) {
		env := newScenarioEnv(t)
		env.issuesResponse = `[{"number": 6, "title": "Issue 6", "state": "open", "body": "body",
			"user": {"login": "alice", "type": "User"}, "created_at": "2026-08-01T00:00:00Z"}]`

		if err := env.resyncer.Resync(env.ctx); err != nil {
			t.Fatalf("Resync() = %v", err)
		}

		if err := env.store.SaveCursor(env.ctx, notificationsSource, "", "", "2026-01-01T00:00:00Z"); err != nil {
			t.Fatalf("SaveCursor: %v", err)
		}

		env.notificationsResponse = `[{"id": "6", "updated_at": "2026-08-05T10:00:00Z",
			"subject": {"title": "Issue 6", "url": "https://api.github.com/repos/k-wa-wa/pechka/issues/6", "type": "Issue"},
			"repository": {"full_name": "k-wa-wa/pechka"}}]`
		env.commentsResponses[6] = `[{"id": 601, "body": "bot progress update", "user": {"login": "nuage-autopilot", "type": "User"}, "created_at": "2026-08-05T09:50:00Z"}]`

		n, err := env.poller.Poll(env.ctx)
		if err != nil || n != 0 {
			t.Fatalf("Poll() = %d, err = %v, want 0", n, err)
		}
	})

	// S-201: 既存アイテムへの許可ユーザーコメントで commented イベントが発生すること
	t.Run("S-201: 登録済みアイテムへの許可ユーザーコメント発火", func(t *testing.T) {
		env := newScenarioEnv(t)
		item, _, err := env.store.UpsertItem(env.ctx, "k-wa-wa/pechka", 7, store.KindIssue)
		if err != nil {
			t.Fatalf("UpsertItem: %v", err)
		}
		past := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
		if err := env.store.UpdateItemLastSeenAt(env.ctx, item.ID, past); err != nil {
			t.Fatalf("UpdateItemLastSeenAt: %v", err)
		}

		if err := env.store.SaveCursor(env.ctx, notificationsSource, "", "", "2026-01-01T00:00:00Z"); err != nil {
			t.Fatalf("SaveCursor: %v", err)
		}

		env.notificationsResponse = `[{"id": "7", "updated_at": "2026-08-05T10:00:00Z",
			"subject": {"title": "Issue 7", "url": "https://api.github.com/repos/k-wa-wa/pechka/issues/7", "type": "Issue"},
			"repository": {"full_name": "k-wa-wa/pechka"}}]`
		env.commentsResponses[7] = `[{"id": 701, "body": "good job", "user": {"login": "bob", "type": "User"}, "created_at": "2026-08-05T09:50:00Z"}]`

		n, err := env.poller.Poll(env.ctx)
		if err != nil || n != 1 {
			t.Fatalf("Poll() = %d, err = %v, want 1", n, err)
		}

		ev, ok, err := env.store.NextUnprocessedEvent(env.ctx)
		if err != nil || !ok || ev.Type != "commented" || ev.Actor != "bob" {
			t.Fatalf("event = %+v, want commented by bob", ev)
		}
	})

	// S-202: 登録済みアイテムへの Bot コメントが無視されること
	t.Run("S-202: 登録済みアイテムへの Bot コメント無視", func(t *testing.T) {
		env := newScenarioEnv(t)
		item, _, err := env.store.UpsertItem(env.ctx, "k-wa-wa/pechka", 202, store.KindIssue)
		if err != nil {
			t.Fatalf("UpsertItem: %v", err)
		}
		past := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
		if err := env.store.UpdateItemLastSeenAt(env.ctx, item.ID, past); err != nil {
			t.Fatalf("UpdateItemLastSeenAt: %v", err)
		}

		if err := env.store.SaveCursor(env.ctx, notificationsSource, "", "", "2026-01-01T00:00:00Z"); err != nil {
			t.Fatalf("SaveCursor: %v", err)
		}

		env.notificationsResponse = `[{"id": "202", "updated_at": "2026-08-05T10:00:00Z",
			"subject": {"title": "Issue 202", "url": "https://api.github.com/repos/k-wa-wa/pechka/issues/202", "type": "Issue"},
			"repository": {"full_name": "k-wa-wa/pechka"}}]`
		env.commentsResponses[202] = `[{"id": 2020, "body": "bot internal status", "user": {"login": "nuage-autopilot", "type": "User"}, "created_at": "2026-08-05T09:50:00Z"}]`

		n, err := env.poller.Poll(env.ctx)
		if err != nil || n != 0 {
			t.Fatalf("Poll() = %d, err = %v, want 0", n, err)
		}
	})

	// S-203: 未許可ユーザーによる既存アイテムへのコメント追加は無視されること
	t.Run("S-203: 登録済みアイテムへの未許可ユーザーコメント無視", func(t *testing.T) {
		env := newScenarioEnv(t)
		item, _, err := env.store.UpsertItem(env.ctx, "k-wa-wa/pechka", 8, store.KindIssue)
		if err != nil {
			t.Fatalf("UpsertItem: %v", err)
		}
		past := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
		if err := env.store.UpdateItemLastSeenAt(env.ctx, item.ID, past); err != nil {
			t.Fatalf("UpdateItemLastSeenAt: %v", err)
		}

		if err := env.store.SaveCursor(env.ctx, notificationsSource, "", "", "2026-01-01T00:00:00Z"); err != nil {
			t.Fatalf("SaveCursor: %v", err)
		}

		env.notificationsResponse = `[{"id": "8", "updated_at": "2026-08-05T10:00:00Z",
			"subject": {"title": "Issue 8", "url": "https://api.github.com/repos/k-wa-wa/pechka/issues/8", "type": "Issue"},
			"repository": {"full_name": "k-wa-wa/pechka"}}]`
		env.commentsResponses[8] = `[{"id": 801, "body": "unauthorized comment", "user": {"login": "stranger", "type": "User"}, "created_at": "2026-08-05T09:50:00Z"}]`

		n, err := env.poller.Poll(env.ctx)
		if err != nil || n != 0 {
			t.Fatalf("Poll() = %d, err = %v, want 0", n, err)
		}
	})

	// S-204: 人間による PR レビュー投稿で reviewed イベントが発生すること
	t.Run("S-204: 人間による PR レビュー投稿で reviewed 発火", func(t *testing.T) {
		env := newScenarioEnv(t)
		item, _, err := env.store.UpsertItem(env.ctx, "k-wa-wa/pechka", 204, store.KindPullRequest)
		if err != nil {
			t.Fatalf("UpsertItem: %v", err)
		}
		past := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
		if err := env.store.UpdateItemLastSeenAt(env.ctx, item.ID, past); err != nil {
			t.Fatalf("UpdateItemLastSeenAt: %v", err)
		}

		if err := env.store.SaveCursor(env.ctx, notificationsSource, "", "", "2026-01-01T00:00:00Z"); err != nil {
			t.Fatalf("SaveCursor: %v", err)
		}

		env.notificationsResponse = `[{"id": "204", "updated_at": "2026-08-05T10:00:00Z",
			"subject": {"title": "PR 204", "url": "https://api.github.com/repos/k-wa-wa/pechka/pulls/204", "type": "PullRequest"},
			"repository": {"full_name": "k-wa-wa/pechka"}}]`
		env.pullDetailResponses[204] = `{"number": 204, "title": "PR 204", "body": "PR body", "state": "open",
			"user": {"login": "alice", "type": "User"}, "head": {"sha": "abc204"}, "created_at": "2026-08-01T00:00:00Z"}`
		env.pullReviewsResponses[204] = `[{"id": 2040, "body": "looks good to me", "user": {"login": "alice", "type": "User"}, "submitted_at": "2026-08-05T09:50:00Z"}]`

		n, err := env.poller.Poll(env.ctx)
		if err != nil || n != 1 {
			t.Fatalf("Poll() = %d, err = %v, want 1", n, err)
		}

		ev, ok, err := env.store.NextUnprocessedEvent(env.ctx)
		if err != nil || !ok || ev.Type != "reviewed" || ev.Actor != "alice" {
			t.Fatalf("event = %+v, want reviewed by alice", ev)
		}
	})

	// S-205: in_review 状態の PR で CI が失敗した場合は ci_failure イベントが発生すること
	t.Run("S-205: in_review PR での CI 失敗発火", func(t *testing.T) {
		env := newScenarioEnv(t)
		item, _, err := env.store.UpsertItem(env.ctx, "k-wa-wa/pechka", 9, store.KindPullRequest)
		if err != nil {
			t.Fatalf("UpsertItem: %v", err)
		}
		if err := env.store.UpdateItemPhase(env.ctx, item.ID, store.PhaseInReview); err != nil {
			t.Fatalf("UpdateItemPhase: %v", err)
		}
		if err := env.store.UpdateItemHeadSHA(env.ctx, item.ID, "sha999"); err != nil {
			t.Fatalf("UpdateItemHeadSHA: %v", err)
		}

		env.checkStateResponses["sha999"] = "failure"

		n, err := env.poller.Poll(env.ctx)
		if err != nil || n != 1 {
			t.Fatalf("Poll() = %d, err = %v, want 1", n, err)
		}

		ev, ok, err := env.store.NextUnprocessedEvent(env.ctx)
		if err != nil || !ok || ev.Type != "ci_failure" {
			t.Fatalf("event = %+v, want ci_failure", ev)
		}
	})

	// S-206: in_review 状態の PR で CI が成功した場合は ci_success イベントが発生すること
	t.Run("S-206: in_review PR での CI 成功発火", func(t *testing.T) {
		env := newScenarioEnv(t)
		item, _, err := env.store.UpsertItem(env.ctx, "k-wa-wa/pechka", 10, store.KindPullRequest)
		if err != nil {
			t.Fatalf("UpsertItem: %v", err)
		}
		if err := env.store.UpdateItemPhase(env.ctx, item.ID, store.PhaseInReview); err != nil {
			t.Fatalf("UpdateItemPhase: %v", err)
		}
		if err := env.store.UpdateItemHeadSHA(env.ctx, item.ID, "sha1000"); err != nil {
			t.Fatalf("UpdateItemHeadSHA: %v", err)
		}

		env.checkStateResponses["sha1000"] = "success"

		n, err := env.poller.Poll(env.ctx)
		if err != nil || n != 1 {
			t.Fatalf("Poll() = %d, err = %v, want 1", n, err)
		}

		ev, ok, err := env.store.NextUnprocessedEvent(env.ctx)
		if err != nil || !ok || ev.Type != "ci_success" {
			t.Fatalf("event = %+v, want ci_success", ev)
		}
	})

	// S-301: GitHub 上で Close されたアイテムは resync で phase=done へ自動更新されること
	t.Run("S-301: Close されたアイテムは resync で phase=done へ遷移", func(t *testing.T) {
		env := newScenarioEnv(t)
		item, _, err := env.store.UpsertItem(env.ctx, "k-wa-wa/pechka", 301, store.KindIssue)
		if err != nil {
			t.Fatalf("UpsertItem: %v", err)
		}
		if err := env.store.UpdateItemPhase(env.ctx, item.ID, store.PhaseInReview); err != nil {
			t.Fatalf("UpdateItemPhase: %v", err)
		}

		// GitHub 上で Issue 301 がクローズされ open なリストに存在しない
		env.issuesResponse = `[]`

		if err := env.resyncer.Resync(env.ctx); err != nil {
			t.Fatalf("Resync() = %v", err)
		}

		updated, ok, err := env.store.GetItem(env.ctx, "k-wa-wa/pechka", 301)
		if err != nil || !ok {
			t.Fatalf("GetItem: ok=%v, err=%v", ok, err)
		}
		if updated.Phase != store.PhaseDone {
			t.Fatalf("updated.Phase = %q, want %q", updated.Phase, store.PhaseDone)
		}
	})

	// S-302: クローズ済みアイテムへの人間コメントもイベント化され自律的に resume されること
	t.Run("S-302: クローズ済みアイテムへの人間コメントも commented イベント化する", func(t *testing.T) {
		env := newScenarioEnv(t)
		item, _, err := env.store.UpsertItem(env.ctx, "k-wa-wa/pechka", 302, store.KindIssue)
		if err != nil {
			t.Fatalf("UpsertItem: %v", err)
		}
		if err := env.store.UpdateItemPhase(env.ctx, item.ID, store.PhaseDone); err != nil {
			t.Fatalf("UpdateItemPhase: %v", err)
		}
		past := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
		if err := env.store.UpdateItemLastSeenAt(env.ctx, item.ID, past); err != nil {
			t.Fatalf("UpdateItemLastSeenAt: %v", err)
		}

		if err := env.store.SaveCursor(env.ctx, notificationsSource, "", "", "2026-01-01T00:00:00Z"); err != nil {
			t.Fatalf("SaveCursor: %v", err)
		}

		env.notificationsResponse = `[{"id": "302", "updated_at": "2026-08-05T10:00:00Z",
			"subject": {"title": "Issue 302", "url": "https://api.github.com/repos/k-wa-wa/pechka/issues/302", "type": "Issue"},
			"repository": {"full_name": "k-wa-wa/pechka"}}]`
		env.commentsResponses[302] = `[{"id": 3020, "body": "reopen please", "user": {"login": "alice", "type": "User"}, "created_at": "2026-08-05T09:50:00Z"}]`

		n, err := env.poller.Poll(env.ctx)
		if err != nil || n != 1 {
			t.Fatalf("Poll() = %d, err = %v, want 1", n, err)
		}

		ev, ok, err := env.store.NextUnprocessedEvent(env.ctx)
		if err != nil || !ok || ev.Type != "commented" || ev.Actor != "alice" {
			t.Fatalf("event = %+v, want commented by alice", ev)
		}
	})
}
