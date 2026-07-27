package config

import "testing"

func TestParse_RepoRequired(t *testing.T) {
	if _, err := Parse(nil); err != ErrRepoRequired {
		t.Fatalf("Parse(nil) error = %v, want %v", err, ErrRepoRequired)
	}
}

func TestParse_VersionSkipsRepoRequirement(t *testing.T) {
	cfg, err := Parse([]string{"--version"})
	if err != nil {
		t.Fatalf("Parse(--version) unexpected error: %v", err)
	}
	if !cfg.ShowVersion {
		t.Fatalf("cfg.ShowVersion = false, want true")
	}
}

func TestParse_RepoAndDefaultStateDir(t *testing.T) {
	t.Setenv("NUAGE_STATE_DIR", "")

	cfg, err := Parse([]string{"--repo", "k-wa-wa/pechka"})
	if err != nil {
		t.Fatalf("Parse(--repo) unexpected error: %v", err)
	}
	if cfg.Repo != "k-wa-wa/pechka" {
		t.Fatalf("cfg.Repo = %q, want %q", cfg.Repo, "k-wa-wa/pechka")
	}
	if cfg.StateDir != DefaultStateDir {
		t.Fatalf("cfg.StateDir = %q, want %q", cfg.StateDir, DefaultStateDir)
	}
}

func TestParse_StateDirFromEnv(t *testing.T) {
	t.Setenv("NUAGE_STATE_DIR", "/tmp/nuage-autopilot-test")

	cfg, err := Parse([]string{"--repo", "k-wa-wa/pechka"})
	if err != nil {
		t.Fatalf("Parse(--repo) unexpected error: %v", err)
	}
	if cfg.StateDir != "/tmp/nuage-autopilot-test" {
		t.Fatalf("cfg.StateDir = %q, want %q", cfg.StateDir, "/tmp/nuage-autopilot-test")
	}
}

func TestMissingEnv_ReportsUnsetRequiredVars(t *testing.T) {
	t.Setenv("GH_TOKEN", "")

	missing := MissingEnv()
	if len(missing) != 1 || missing[0] != "GH_TOKEN" {
		t.Fatalf("GH_TOKEN が未設定のときは報告されるべきである: got %v", missing)
	}
}

func TestMissingEnv_EmptyWhenAllSet(t *testing.T) {
	for _, name := range RequiredEnvVars {
		t.Setenv(name, "dummy")
	}

	if missing := MissingEnv(); missing != nil {
		t.Fatalf("すべて設定済みのときは nil を返すべきである: got %v", missing)
	}
}
