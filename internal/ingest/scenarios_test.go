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
| S-207 | InReview     | Push -> CI None -> Poll      | System/CI       | 発火 (1件) | in_review PR での CI なし発火 (ci_success)
| S-208 | InReview     | Push -> CI Pending -> Poll   | System/CI       | 無視 (0件) | CI 実行中は結果未確定なので発火しない
| S-209 | InReview     | Push -> CI None -> Poll      | System/CI       | 無視 (0件) | 猶予内の none は確定させない (CI 登録遅延との混同防止)
| S-210 | InReview     | (head_sha なし) -> Poll      | System/CI       | 無視 (0件) | head_sha 未設定なら CI を問い合わせない
| S-211 | New          | Push -> CI Failure -> Poll   | System/CI       | 無視 (0件) | in_review/ready 以外では CI をポーリングしない
| S-212 | Ready        | Push -> CI Failure -> Poll   | System/CI       | 発火 (1件) | ready PR での CI 失敗発火 (in_review へ戻す経路)
| S-213 | InReview     | CI Success -> Ready -> Poll  | System/CI       | 無視 (0件) | 判定済み sha は ready で再発火しない (dedup)
| S-214 | New/InReview | Comment -> Poll              | ThirdPartyBot   | 無視 (0件) | 第三者 Bot (type=="Bot") のコメント無視

3. クローズ & 完了フェーズ (Done)
----------------------------------------------------------------------------------------------------------------------
| ID    | 初期状態      | 操作順序 (Sequence)           | Actor           | 判定期待値  | 目的・検証内容
----------------------------------------------------------------------------------------------------------------------
| S-301 | InReview     | Close -> Resync              | AllowedUser     | 状態更新   | Close されたアイテムは resync で phase=done へ
| S-302 | Done         | Comment -> Poll              | AllowedUser     | 発火 (1件) | done アイテムへの人間コメントもイベント化 (resume対応)
| S-303 | Done         | Resync -> Resync             | N/A             | 無視 (0件) | resync 反復でも done を再処理せずイベントも積まない
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

	// checkStateCalls は sha ごとの check-runs 問い合わせ回数である。
	// 「そのフェーズでは CI を見に行かない」ことの検証に使う。
	checkStateCalls map[string]int
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
		checkStateCalls:      make(map[string]int),
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

// seedPR は指定した phase / head_sha を持つ PR を DB に用意する。CI 系シナリオの
// 定型的な前準備をまとめたものである。
func (e *scenarioEnv) seedPR(number int, phase store.Phase, headSHA string) store.Item {
	e.t.Helper()
	item, _, err := e.store.UpsertItem(e.ctx, "k-wa-wa/pechka", number, store.KindPullRequest)
	if err != nil {
		e.t.Fatalf("UpsertItem: %v", err)
	}
	if err := e.store.UpdateItemPhase(e.ctx, item.ID, phase); err != nil {
		e.t.Fatalf("UpdateItemPhase: %v", err)
	}
	if headSHA != "" {
		if err := e.store.UpdateItemHeadSHA(e.ctx, item.ID, headSHA); err != nil {
			e.t.Fatalf("UpdateItemHeadSHA: %v", err)
		}
	}
	return item
}

// wantNoEvent は events が空であることを検証する。
func (e *scenarioEnv) wantNoEvent() {
	e.t.Helper()
	if _, ok, err := e.store.NextUnprocessedEvent(e.ctx); err != nil || ok {
		e.t.Fatalf("no event must be enqueued: ok=%v err=%v", ok, err)
	}
}

// wantEventType は未処理イベントが1件あり、その type が want であることを検証する。
func (e *scenarioEnv) wantEventType(want string) store.Event {
	e.t.Helper()
	ev, ok, err := e.store.NextUnprocessedEvent(e.ctx)
	if err != nil || !ok {
		e.t.Fatalf("NextUnprocessedEvent: ok=%v err=%v", ok, err)
	}
	if ev.Type != want {
		e.t.Fatalf("event type = %q, want %q", ev.Type, want)
	}
	return ev
}

func (e *scenarioEnv) handleHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	if strings.HasPrefix(r.URL.Path, "/repos/k-wa-wa/pechka/commits/") && strings.HasSuffix(r.URL.Path, "/check-runs") {
		parts := strings.Split(r.URL.Path, "/")
		if len(parts) >= 6 {
			sha := parts[5]
			e.checkStateCalls[sha]++
			if state, ok := e.checkStateResponses[sha]; ok {
				// "pending" は未完了の check run を、それ以外は完了した conclusion を表す。
				if state == "pending" {
					_, _ = w.Write([]byte(`{"total_count": 1, "check_runs": [{"status": "in_progress", "conclusion": null}]}`))
					return
				}
				_, _ = fmt.Fprintf(w, `{"total_count": 1, "check_runs": [{"status": "completed", "conclusion": %q}]}`, state)
				return
			}
		}
		// 未登録の sha は Check Runs 0 件（none）を表す。
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

	// S-207: Check Runs が 0 件（none）の場合も ci_success イベントが発生して verify へ進むこと
	t.Run("S-207: in_review PR での CI なし（none）発火", func(t *testing.T) {
		env := newScenarioEnv(t)
		item, _, err := env.store.UpsertItem(env.ctx, "k-wa-wa/pechka", 11, store.KindPullRequest)
		if err != nil {
			t.Fatalf("UpsertItem: %v", err)
		}
		if err := env.store.UpdateItemPhase(env.ctx, item.ID, store.PhaseInReview); err != nil {
			t.Fatalf("UpdateItemPhase: %v", err)
		}
		if err := env.store.UpdateItemHeadSHA(env.ctx, item.ID, "sha2000"); err != nil {
			t.Fatalf("UpdateItemHeadSHA: %v", err)
		}

		// checkStateResponses に登録しないことで total_count: 0 (none) をシミュレート

		n, err := env.poller.Poll(env.ctx)
		if err != nil || n != 1 {
			t.Fatalf("Poll() = %d, err = %v, want 1", n, err)
		}

		ev, ok, err := env.store.NextUnprocessedEvent(env.ctx)
		if err != nil || !ok || ev.Type != "ci_success" {
			t.Fatalf("event = %+v, want ci_success", ev)
		}
	})

	// S-208: CI 実行中（pending）は結果が確定していないため何も発火しないこと。
	// none と混同して ci_success を積んでしまうと、CI を待たずに verify へ進んでしまう。
	t.Run("S-208: in_review PR での CI 実行中（pending）は発火しない", func(t *testing.T) {
		env := newScenarioEnv(t)
		env.seedPR(208, store.PhaseInReview, "sha208")
		env.checkStateResponses["sha208"] = "pending"

		if n, err := env.poller.Poll(env.ctx); err != nil || n != 0 {
			t.Fatalf("Poll() = %d, err = %v, want 0", n, err)
		}
		env.wantNoEvent()
	})

	// S-209: push 直後で check run がまだ登録されていない none は、猶予が明けるまで
	// 確定させないこと（CI 設定済みリポジトリで CI 結果を待たずにパスさせない）。
	t.Run("S-209: in_review PR での CI なしは猶予内なら発火しない", func(t *testing.T) {
		checkRunsNoneGrace = time.Minute
		t.Cleanup(func() { checkRunsNoneGrace = 0 })

		env := newScenarioEnv(t)
		// seedPR 直後は updated_at がほぼ現在時刻であり、猶予の内側にある。
		env.seedPR(209, store.PhaseInReview, "sha209")

		if n, err := env.poller.Poll(env.ctx); err != nil || n != 0 {
			t.Fatalf("Poll() = %d, err = %v, want 0", n, err)
		}
		env.wantNoEvent()
	})

	// S-210: head_sha が未設定の in_review アイテムでは CI を問い合わせないこと。
	t.Run("S-210: head_sha 未設定の in_review PR では CI を見に行かない", func(t *testing.T) {
		env := newScenarioEnv(t)
		env.seedPR(210, store.PhaseInReview, "")

		if n, err := env.poller.Poll(env.ctx); err != nil || n != 0 {
			t.Fatalf("Poll() = %d, err = %v, want 0", n, err)
		}
		env.wantNoEvent()
		if len(env.checkStateCalls) != 0 {
			t.Fatalf("check-runs calls = %v, want none", env.checkStateCalls)
		}
	})

	// S-211: in_review / ready 以外のフェーズでは CI をポーリングしないこと。
	// new の PR で CI が落ちていても、実装が完了していない段階では起こす意味が無い。
	t.Run("S-211: in_review/ready 以外のフェーズでは CI を見に行かない", func(t *testing.T) {
		env := newScenarioEnv(t)
		env.seedPR(211, store.PhaseNew, "sha211")
		env.checkStateResponses["sha211"] = "failure"

		if n, err := env.poller.Poll(env.ctx); err != nil || n != 0 {
			t.Fatalf("Poll() = %d, err = %v, want 0", n, err)
		}
		env.wantNoEvent()
		if env.checkStateCalls["sha211"] != 0 {
			t.Fatalf("check-runs calls for sha211 = %d, want 0", env.checkStateCalls["sha211"])
		}
	})

	// S-212: ready の PR に人間が追加 push して CI が落ちた場合、ci_failure を発火して
	// in_review へ戻せること（DESIGN.md 8.1 節）。ready を CI ポーリングの対象に
	// 含めなければ、CI が落ちた PR が ready のまま放置される。
	t.Run("S-212: ready PR での CI 失敗発火", func(t *testing.T) {
		env := newScenarioEnv(t)
		env.seedPR(212, store.PhaseReady, "sha212")
		env.checkStateResponses["sha212"] = "failure"

		if n, err := env.poller.Poll(env.ctx); err != nil || n != 1 {
			t.Fatalf("Poll() = %d, err = %v, want 1", n, err)
		}
		env.wantEventType("ci_failure")
	})

	// S-213: in_review のうちに判定済みの sha を ready で見直しても、dedup_key が同じで
	// あるため二重に発火しないこと（ready をポーリング対象に加えた副作用が無いことの保証）。
	t.Run("S-213: in_review で判定済みの sha は ready で再発火しない", func(t *testing.T) {
		env := newScenarioEnv(t)
		item := env.seedPR(213, store.PhaseInReview, "sha213")
		env.checkStateResponses["sha213"] = "success"

		if n, err := env.poller.Poll(env.ctx); err != nil || n != 1 {
			t.Fatalf("Poll() (in_review) = %d, err = %v, want 1", n, err)
		}
		ev := env.wantEventType("ci_success")
		if err := env.store.MarkEventProcessed(env.ctx, ev.ID); err != nil {
			t.Fatalf("MarkEventProcessed: %v", err)
		}

		// verify を通って ready へ進んだ後も、同じ sha では何も起きてはならない。
		if err := env.store.UpdateItemPhase(env.ctx, item.ID, store.PhaseReady); err != nil {
			t.Fatalf("UpdateItemPhase: %v", err)
		}
		if n, err := env.poller.Poll(env.ctx); err != nil || n != 0 {
			t.Fatalf("Poll() (ready) = %d, err = %v, want 0 (dedup must suppress the repeat)", n, err)
		}
		env.wantNoEvent()
	})

	// S-214: bot 自身以外の第三者 Bot（dependabot 等。type == "Bot"）のコメントも
	// 無視すること。許可リストに載らない Bot でエージェントを起こすと予算を焼く
	// （DESIGN.md 7.3 節）。
	t.Run("S-214: 第三者 Bot のコメント無視", func(t *testing.T) {
		env := newScenarioEnv(t)
		item, _, err := env.store.UpsertItem(env.ctx, "k-wa-wa/pechka", 214, store.KindIssue)
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

		env.notificationsResponse = `[{"id": "214", "updated_at": "2026-08-05T10:00:00Z",
			"subject": {"title": "Issue 214", "url": "https://api.github.com/repos/k-wa-wa/pechka/issues/214", "type": "Issue"},
			"repository": {"full_name": "k-wa-wa/pechka"}}]`
		env.commentsResponses[214] = `[{"id": 2140, "body": "Bumps foo from 1.0 to 1.1", "user": {"login": "dependabot[bot]", "type": "Bot"}, "created_at": "2026-08-05T09:50:00Z"}]`

		if n, err := env.poller.Poll(env.ctx); err != nil || n != 0 {
			t.Fatalf("Poll() = %d, err = %v, want 0", n, err)
		}
		env.wantNoEvent()
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

	// S-303: resync を繰り返しても done のアイテムを再処理せず、イベントも積まないこと
	// （resync はイベントの source ではない。DESIGN.md 7.5・7.6 節）。
	t.Run("S-303: done アイテムは resync を繰り返しても再処理されない", func(t *testing.T) {
		env := newScenarioEnv(t)
		item, _, err := env.store.UpsertItem(env.ctx, "k-wa-wa/pechka", 303, store.KindIssue)
		if err != nil {
			t.Fatalf("UpsertItem: %v", err)
		}
		if err := env.store.UpdateItemPhase(env.ctx, item.ID, store.PhaseDone); err != nil {
			t.Fatalf("UpdateItemPhase: %v", err)
		}

		// GitHub 上では既にクローズ済み（open なリストに出てこない）。
		env.issuesResponse = `[]`
		env.pullsResponse = `[]`

		for i := 0; i < 2; i++ {
			if err := env.resyncer.Resync(env.ctx); err != nil {
				t.Fatalf("Resync() #%d: %v", i+1, err)
			}
		}

		got, ok, err := env.store.GetItemByID(env.ctx, item.ID)
		if err != nil || !ok {
			t.Fatalf("GetItemByID: ok=%v err=%v", ok, err)
		}
		if got.Phase != store.PhaseDone {
			t.Fatalf("phase = %q, want done", got.Phase)
		}
		env.wantNoEvent()
	})
}
