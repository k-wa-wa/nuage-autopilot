package main

import (
	"bytes"
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
	t.Setenv("NUAGE_STATE_DIR", "/tmp/nuage-autopilot-test")

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
