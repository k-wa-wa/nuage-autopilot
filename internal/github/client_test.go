package github

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// newTestClient は handler を提供する httptest.Server に向いた Client を生成する。
// テストは一切実際の GitHub API に到達しない。
func newTestClient(t *testing.T, handler http.HandlerFunc) *Client {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	return NewClient(WithBaseURL(server.URL), WithStaticToken("test-token"))
}

func TestListOpenIssues_FiltersOutPullRequests(t *testing.T) {
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/repos/k-wa-wa/pechka/issues" {
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer test-token" {
			t.Fatalf("Authorization header = %q, want Bearer test-token", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[
			{"number": 1, "title": "issue only", "state": "open", "body": "please add X", "labels": [{"name": "agent:spec"}], "user": {"login": "alice", "type": "User"}},
			{"number": 2, "title": "this is a pr", "state": "open", "labels": [], "user": {"login": "bot", "type": "User"}, "pull_request": {}}
		]`))
	})

	issues, err := client.ListOpenIssues(context.Background(), "k-wa-wa/pechka")
	if err != nil {
		t.Fatalf("ListOpenIssues() error = %v", err)
	}
	if len(issues) != 1 {
		t.Fatalf("len(issues) = %d, want 1 (PR must be filtered out)", len(issues))
	}
	if issues[0].Number != 1 {
		t.Fatalf("issues[0].Number = %d, want 1", issues[0].Number)
	}
	if len(issues[0].Labels) != 1 || issues[0].Labels[0] != "agent:spec" {
		t.Fatalf("issues[0].Labels = %v, want [agent:spec]", issues[0].Labels)
	}
	// GET /repos/{repo}/issues の一覧レスポンスは body を含んでおり、追加の API 呼び出し
	// なしに取得できる（この一覧取得は 1 回のリクエストしか送っていないことを上の
	// パスチェックとテストサーバーの単一ハンドラで保証している）。
	if issues[0].Body != "please add X" {
		t.Fatalf("issues[0].Body = %q, want %q", issues[0].Body, "please add X")
	}
}

func TestListOpenPullRequests(t *testing.T) {
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/repos/k-wa-wa/pechka/pulls" {
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
		_, _ = w.Write([]byte(`[{"number": 5, "title": "add feature", "state": "open", "body": "implements the thing", "labels": [{"name": "agent:review-general"}], "user": {"login": "nuage-autopilot", "type": "User"}, "draft": false}]`))
	})

	prs, err := client.ListOpenPullRequests(context.Background(), "k-wa-wa/pechka")
	if err != nil {
		t.Fatalf("ListOpenPullRequests() error = %v", err)
	}
	if len(prs) != 1 || prs[0].Number != 5 {
		t.Fatalf("prs = %+v, want single PR #5", prs)
	}
	if len(prs[0].Labels) != 1 || prs[0].Labels[0] != "agent:review-general" {
		t.Fatalf("prs[0].Labels = %v, want [agent:review-general]", prs[0].Labels)
	}
	// GET /repos/{repo}/pulls も一覧レスポンスに body を含む（追加の API 呼び出しは不要）。
	if prs[0].Body != "implements the thing" {
		t.Fatalf("prs[0].Body = %q, want %q", prs[0].Body, "implements the thing")
	}
}

func TestGetPullRequest(t *testing.T) {
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/repos/k-wa-wa/pechka/pulls/5" {
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		_, _ = w.Write([]byte(`{"number": 5, "title": "add feature", "state": "open", "head": {"sha": "deadbeef"}}`))
	})

	pr, err := client.GetPullRequest(context.Background(), "k-wa-wa/pechka", 5)
	if err != nil {
		t.Fatalf("GetPullRequest() error = %v", err)
	}
	if pr.Number != 5 || pr.HeadSHA != "deadbeef" {
		t.Fatalf("pr = %+v, want Number=5 HeadSHA=deadbeef", pr)
	}
}

func TestGetIssue(t *testing.T) {
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/repos/k-wa-wa/pechka/issues/7" {
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		_, _ = w.Write([]byte(`{"number": 7, "title": "add X", "state": "open", "body": "please add X", "user": {"login": "alice", "type": "User"}, "labels": [{"name": "agent:ignore"}]}`))
	})

	issue, err := client.GetIssue(context.Background(), "k-wa-wa/pechka", 7)
	if err != nil {
		t.Fatalf("GetIssue() error = %v", err)
	}
	if issue.Number != 7 || issue.Body != "please add X" || issue.User.Login != "alice" {
		t.Fatalf("issue = %+v, want Number=7 Body=%q User.Login=alice", issue, "please add X")
	}
	if len(issue.Labels) != 1 || issue.Labels[0] != "agent:ignore" {
		t.Fatalf("issue.Labels = %v, want [agent:ignore]", issue.Labels)
	}
}

func TestAddLabel(t *testing.T) {
	var gotBody map[string][]string
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/repos/k-wa-wa/pechka/issues/1/labels" {
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Fatalf("decode body: %v", err)
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`[]`))
	})

	if err := client.AddLabel(context.Background(), "k-wa-wa/pechka", 1, "agent:spec"); err != nil {
		t.Fatalf("AddLabel() error = %v", err)
	}
	if want := []string{"agent:spec"}; len(gotBody["labels"]) != 1 || gotBody["labels"][0] != want[0] {
		t.Fatalf("request body labels = %v, want %v", gotBody["labels"], want)
	}
}

func TestRemoveLabel_Success(t *testing.T) {
	var gotPath string
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		if r.Method != http.MethodDelete {
			t.Fatalf("method = %s, want DELETE", r.Method)
		}
		w.WriteHeader(http.StatusOK)
	})

	if err := client.RemoveLabel(context.Background(), "k-wa-wa/pechka", 1, "agent:wait"); err != nil {
		t.Fatalf("RemoveLabel() error = %v", err)
	}
	if want := "/repos/k-wa-wa/pechka/issues/1/labels/agent:wait"; gotPath != want {
		t.Fatalf("path = %q, want %q", gotPath, want)
	}
}

func TestRemoveLabel_AlreadyAbsentIsNotAnError(t *testing.T) {
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"message": "Label does not exist"}`, http.StatusNotFound)
	})

	if err := client.RemoveLabel(context.Background(), "k-wa-wa/pechka", 1, "agent:wait"); err != nil {
		t.Fatalf("RemoveLabel() error = %v, want nil for 404", err)
	}
}

func TestRemoveLabel_OtherErrorsPropagate(t *testing.T) {
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"message": "boom"}`, http.StatusInternalServerError)
	})

	err := client.RemoveLabel(context.Background(), "k-wa-wa/pechka", 1, "agent:wait")
	if err == nil {
		t.Fatal("RemoveLabel() error = nil, want error for 500")
	}
}

func TestListComments(t *testing.T) {
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/k-wa-wa/pechka/issues/3/comments":
			_, _ = w.Write([]byte(`[{"id": 100, "body": "looks good", "user": {"login": "alice", "type": "User"}, "created_at": "2026-07-01T00:00:00Z"}]`))
		case "/repos/k-wa-wa/pechka/pulls/3/reviews":
			http.Error(w, `{"message": "Not Found"}`, http.StatusNotFound)
		default:
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
	})

	comments, err := client.ListComments(context.Background(), "k-wa-wa/pechka", 3, true)
	if err != nil {
		t.Fatalf("ListComments() error = %v", err)
	}
	if len(comments) != 1 || comments[0].User.Login != "alice" {
		t.Fatalf("comments = %+v, want single comment from alice", comments)
	}
}

func TestListComments_FollowsLinkHeaderToLastPage(t *testing.T) {
	var requestedURLs []string
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		requestedURLs = append(requestedURLs, r.URL.String())
		switch {
		case r.URL.Path == "/repos/k-wa-wa/pechka/issues/3/comments" && r.URL.RawQuery == "per_page=100":
			// 1 ページ目（最も古い 100 件）のレスポンス。rel="last" で 2 ページ目を指す。
			lastURL := "http://" + r.Host + "/repos/k-wa-wa/pechka/issues/3/comments?per_page=100&page=2"
			w.Header().Set("Link", `<`+lastURL+`>; rel="last"`)
			_, _ = w.Write([]byte(`[{"id": 1, "body": "oldest (page 1)", "user": {"login": "alice", "type": "User"}, "created_at": "2026-01-01T00:00:00Z"}]`))
		case r.URL.Path == "/repos/k-wa-wa/pechka/issues/3/comments" && r.URL.RawQuery == "per_page=100&page=2":
			_, _ = w.Write([]byte(`[{"id": 2, "body": "newest (page 2)", "user": {"login": "bob", "type": "User"}, "created_at": "2026-07-01T00:00:00Z"}]`))
		case r.URL.Path == "/repos/k-wa-wa/pechka/pulls/3/reviews":
			http.Error(w, `{"message": "Not Found"}`, http.StatusNotFound)
		default:
			t.Fatalf("unexpected request: %s", r.URL.String())
		}
	})

	comments, err := client.ListComments(context.Background(), "k-wa-wa/pechka", 3, true)
	if err != nil {
		t.Fatalf("ListComments() error = %v", err)
	}
	// 1 ページ目の内容は使われず、Link ヘッダが指す最終ページのみが返る。
	if len(comments) != 1 || comments[0].Body != "newest (page 2)" {
		t.Fatalf("comments = %+v, want only the last page's comment", comments)
	}
	if len(requestedURLs) != 3 {
		t.Fatalf("requestedURLs = %v, want exactly 3 requests (comments page 1, comments last page, reviews)", requestedURLs)
	}
}

func TestListComments_NoLinkHeaderUsesSinglePageAsIs(t *testing.T) {
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/k-wa-wa/pechka/issues/3/comments":
			// Link ヘッダを付けない（1 ページに収まる場合の挙動）。
			_, _ = w.Write([]byte(`[{"id": 1, "body": "only comment", "user": {"login": "alice", "type": "User"}, "created_at": "2026-07-01T00:00:00Z"}]`))
		case "/repos/k-wa-wa/pechka/pulls/3/reviews":
			http.Error(w, `{"message": "Not Found"}`, http.StatusNotFound)
		default:
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
	})

	comments, err := client.ListComments(context.Background(), "k-wa-wa/pechka", 3, true)
	if err != nil {
		t.Fatalf("ListComments() error = %v", err)
	}
	if len(comments) != 1 || comments[0].Body != "only comment" {
		t.Fatalf("comments = %+v, want the single comment from the only page", comments)
	}
}

func TestListComments_IncludesReviews(t *testing.T) {
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/k-wa-wa/pechka/issues/3/comments":
			_, _ = w.Write([]byte(`[{"id": 100, "body": "first comment", "user": {"login": "alice", "type": "User"}, "created_at": "2026-07-01T00:00:00Z"}]`))
		case "/repos/k-wa-wa/pechka/pulls/3/reviews":
			_, _ = w.Write([]byte(`[{"id": 101, "body": "[Review Result: PASSED]", "user": {"login": "reviewer", "type": "User"}, "submitted_at": "2026-07-01T01:00:00Z"}]`))
		default:
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
	})

	comments, err := client.ListComments(context.Background(), "k-wa-wa/pechka", 3, true)
	if err != nil {
		t.Fatalf("ListComments() error = %v", err)
	}
	if len(comments) != 2 {
		t.Fatalf("got %d comments, want 2", len(comments))
	}
	if comments[1].Body != "[Review Result: PASSED]" {
		t.Fatalf("comments[1].Body = %q, want review body", comments[1].Body)
	}
}

func TestListCommentsSince_FiltersReviewsClientSideAndSetsKind(t *testing.T) {
	var gotQuery string
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/k-wa-wa/pechka/issues/3/comments":
			gotQuery = r.URL.RawQuery
			_, _ = w.Write([]byte(`[{"id": 1, "body": "new comment", "user": {"login": "alice", "type": "User"}, "created_at": "2026-07-02T00:00:00Z"}]`))
		case "/repos/k-wa-wa/pechka/pulls/3/reviews":
			// レビュー一覧は since に関係なく全件返る想定。since より古いものは
			// クライアント側でフィルタされ、結果に含まれてはならない。
			_, _ = w.Write([]byte(`[
				{"id": 10, "body": "old review", "user": {"login": "bob", "type": "User"}, "submitted_at": "2026-06-01T00:00:00Z"},
				{"id": 11, "body": "new review", "user": {"login": "bob", "type": "User"}, "submitted_at": "2026-07-03T00:00:00Z"}
			]`))
		default:
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
	})

	since := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	comments, err := client.ListCommentsSince(context.Background(), "k-wa-wa/pechka", 3, since, true)
	if err != nil {
		t.Fatalf("ListCommentsSince() error = %v", err)
	}
	if !strings.Contains(gotQuery, "since=2026-07-01T00:00:00Z") {
		t.Fatalf("query = %q, want it to contain an RFC3339 since param", gotQuery)
	}
	if len(comments) != 2 {
		t.Fatalf("len(comments) = %d, want 2 (old review must be filtered out)", len(comments))
	}
	if comments[0].Kind != CommentKindComment || comments[0].Body != "new comment" {
		t.Fatalf("comments[0] = %+v, want kind=comment body=%q", comments[0], "new comment")
	}
	if comments[1].Kind != CommentKindReview || comments[1].Body != "new review" {
		t.Fatalf("comments[1] = %+v, want kind=review body=%q", comments[1], "new review")
	}
}

func TestListCommentsSince_NotPR_SkipsReviews(t *testing.T) {
	requests := 0
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		requests++
		if r.URL.Path != "/repos/k-wa-wa/pechka/issues/3/comments" {
			t.Fatalf("unexpected path (reviews should not be fetched for an issue): %s", r.URL.Path)
		}
		_, _ = w.Write([]byte(`[]`))
	})

	if _, err := client.ListCommentsSince(context.Background(), "k-wa-wa/pechka", 3, time.Now(), false); err != nil {
		t.Fatalf("ListCommentsSince() error = %v", err)
	}
	if requests != 1 {
		t.Fatalf("requests = %d, want 1", requests)
	}
}

func TestGetNotifications_ParsesThreadsAndCapturesLastModified(t *testing.T) {
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/notifications" {
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
		if got := r.URL.Query().Get("since"); got != "2026-07-01T00:00:00Z" {
			t.Fatalf("since query = %q, want %q", got, "2026-07-01T00:00:00Z")
		}
		if got := r.Header.Get("If-Modified-Since"); got != "Wed, 01 Jul 2026 00:00:00 GMT" {
			t.Fatalf("If-Modified-Since header = %q", got)
		}
		w.Header().Set("Last-Modified", "Wed, 01 Jul 2026 01:00:00 GMT")
		_, _ = w.Write([]byte(`[{"id": "1", "unread": true, "reason": "subscribed", "updated_at": "2026-07-01T01:00:00Z", "subject": {"title": "Greetings", "url": "https://api.github.com/repos/k-wa-wa/pechka/issues/42", "type": "Issue"}, "repository": {"full_name": "k-wa-wa/pechka"}}]`))
	})

	threads, lastModified, _, notModified, err := client.GetNotifications(context.Background(),
		"2026-07-01T00:00:00Z", "Wed, 01 Jul 2026 00:00:00 GMT", "")
	if err != nil {
		t.Fatalf("GetNotifications() error = %v", err)
	}
	if notModified {
		t.Fatalf("notModified = true, want false")
	}
	if len(threads) != 1 || threads[0].Repository.FullName != "k-wa-wa/pechka" || threads[0].Subject.Type != "Issue" {
		t.Fatalf("threads = %+v", threads)
	}
	if lastModified != "Wed, 01 Jul 2026 01:00:00 GMT" {
		t.Fatalf("lastModified = %q", lastModified)
	}
}

func TestGetNotifications_304DoesNotError(t *testing.T) {
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotModified)
	})

	threads, _, _, notModified, err := client.GetNotifications(context.Background(), "", "some-value", "")
	if err != nil {
		t.Fatalf("GetNotifications() error = %v, want nil for 304", err)
	}
	if !notModified {
		t.Fatalf("notModified = false, want true")
	}
	if threads != nil {
		t.Fatalf("threads = %v, want nil", threads)
	}
}

func TestSetRepositorySubscription(t *testing.T) {
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "PUT" {
			t.Errorf("Method = %q, want PUT", r.Method)
		}
		if r.URL.Path != "/repos/k-wa-wa/pechka/subscription" {
			t.Errorf("Path = %q, want /repos/k-wa-wa/pechka/subscription", r.URL.Path)
		}
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"subscribed": true}`))
	})

	err := client.SetRepositorySubscription(context.Background(), "k-wa-wa/pechka", true)
	if err != nil {
		t.Fatalf("SetRepositorySubscription() error = %v", err)
	}
}

func TestCreateLabel(t *testing.T) {
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/repos/k-wa-wa/pechka/labels" {
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		// 422 応答（既存ラベル）の場合も成功として扱われるかテスト
		http.Error(w, `{"message": "Already Exists"}`, http.StatusUnprocessableEntity)
	})

	if err := client.CreateLabel(context.Background(), "k-wa-wa/pechka", "agent:running"); err != nil {
		t.Fatalf("CreateLabel() error = %v, want nil for 422", err)
	}
}

func TestCreateComment(t *testing.T) {
	var gotBody map[string]string
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/repos/k-wa-wa/pechka/issues/3/comments" {
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{}`))
	})

	if err := client.CreateComment(context.Background(), "k-wa-wa/pechka", 3, "hello"); err != nil {
		t.Fatalf("CreateComment() error = %v", err)
	}
	if gotBody["body"] != "hello" {
		t.Fatalf("request body = %v, want body=hello", gotBody)
	}
}

func TestCurrentUser(t *testing.T) {
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/user" {
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
		_, _ = w.Write([]byte(`{"login": "nuage-autopilot"}`))
	})

	login, err := client.CurrentUser(context.Background())
	if err != nil {
		t.Fatalf("CurrentUser() error = %v", err)
	}
	if login != "nuage-autopilot" {
		t.Fatalf("login = %q, want nuage-autopilot", login)
	}
}

func TestNewClient_ReReadsGHTokenEnvOnEveryRequest(t *testing.T) {
	var gotAuth []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = append(gotAuth, r.Header.Get("Authorization"))
		w.Write([]byte(`{"login": "x"}`))
	}))
	t.Cleanup(server.Close)

	client := NewClient(WithBaseURL(server.URL))

	t.Setenv("GH_TOKEN", "")
	if _, err := client.CurrentUser(context.Background()); err != nil {
		t.Fatalf("CurrentUser() error = %v", err)
	}

	// secrets.env が起動後に配置されたことを模す。再起動（NewClient の再生成）は行わない。
	t.Setenv("GH_TOKEN", "later-token")
	if _, err := client.CurrentUser(context.Background()); err != nil {
		t.Fatalf("CurrentUser() error = %v", err)
	}

	if len(gotAuth) != 2 {
		t.Fatalf("got %d requests, want 2", len(gotAuth))
	}
	if gotAuth[0] != "" {
		t.Fatalf("1st Authorization header = %q, want empty (GH_TOKEN was unset)", gotAuth[0])
	}
	if gotAuth[1] != "Bearer later-token" {
		t.Fatalf("2nd Authorization header = %q, want %q", gotAuth[1], "Bearer later-token")
	}
}

func TestRequest_NonTwoXXReturnsAPIError(t *testing.T) {
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
	})

	_, err := client.CurrentUser(context.Background())
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "401") {
		t.Fatalf("error = %v, want it to mention status 401", err)
	}
}
