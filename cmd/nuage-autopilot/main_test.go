package main

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestRun_VersionFlag(t *testing.T) {
	var stdout, stderr bytes.Buffer

	code := run([]string{"--version"}, &stdout, &stderr)

	if code != 0 {
		t.Fatalf("run(--version) exit code = %d, want 0", code)
	}
	if got := strings.TrimSpace(stdout.String()); got != version {
		t.Fatalf("stdout = %q, want %q", got, version)
	}
}

func TestRun_MissingRepo(t *testing.T) {
	var stdout, stderr bytes.Buffer

	code := run(nil, &stdout, &stderr)

	if code != 2 {
		t.Fatalf("run(nil) exit code = %d, want 2", code)
	}
	if stderr.Len() == 0 {
		t.Fatalf("stderr is empty, want error message about missing --repo")
	}
}

func TestRun_RepoLogsCycleCompletion(t *testing.T) {
	// このテストは実際の GitHub API には一切到達しない。cycle.Run は internal/github
	// 経由で GitHub REST API を呼び出すため、main の統合テストとして成立させるには
	// httptest.Server を立て、NUAGE_GITHUB_API_BASE_URL で参照先を差し替える。
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/repos/k-wa-wa/pechka/issues", "/repos/k-wa-wa/pechka/pulls":
			_, _ = w.Write([]byte(`[]`))
		default:
			http.Error(w, "not found", http.StatusNotFound)
		}
	}))
	defer server.Close()

	t.Setenv("NUAGE_STATE_DIR", "/tmp/nuage-autopilot-test")
	t.Setenv("NUAGE_GITHUB_API_BASE_URL", server.URL)
	t.Setenv("GH_TOKEN", "test-token")
	t.Setenv("GIT_AUTHOR_NAME", "nuage-autopilot")
	t.Setenv("GIT_AUTHOR_EMAIL", "nuage-autopilot@example.invalid")

	var stdout, stderr bytes.Buffer

	code := run([]string{"--repo", "k-wa-wa/pechka"}, &stdout, &stderr)

	if code != 0 {
		t.Fatalf("run(--repo) exit code = %d, want 0 (stderr: %s)", code, stderr.String())
	}

	out := stdout.String()
	for _, want := range []string{"k-wa-wa/pechka", "/tmp/nuage-autopilot-test", "cycle completed"} {
		if !strings.Contains(out, want) {
			t.Fatalf("stdout = %q, want it to contain %q", out, want)
		}
	}
}
