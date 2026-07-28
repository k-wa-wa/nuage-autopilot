package github

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// newTestClient は handler を提供する httptest.Server に向いた Client を生成する。
// テストは一切実際の GitHub API に到達しない。
func newTestClient(t *testing.T, handler http.HandlerFunc) *Client {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	return NewClient("test-token", WithBaseURL(server.URL))
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

	comments, err := client.ListComments(context.Background(), "k-wa-wa/pechka", 3)
	if err != nil {
		t.Fatalf("ListComments() error = %v", err)
	}
	if len(comments) != 1 || comments[0].User.Login != "alice" {
		t.Fatalf("comments = %+v, want single comment from alice", comments)
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

	comments, err := client.ListComments(context.Background(), "k-wa-wa/pechka", 3)
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
