package report

import (
	"os"
	"path/filepath"
	"testing"
)

func TestParse_ValidLineWithSHA(t *testing.T) {
	body := "<!-- nuage-autopilot worker=verify status=failed sha=abc123 -->\n本文はここから。"
	sl, ok := Parse(body)
	if !ok {
		t.Fatalf("Parse() ok = false, want true")
	}
	if sl.Worker != WorkerVerify || sl.Status != StatusFailed || sl.SHA != "abc123" {
		t.Fatalf("Parse() = %+v, want Worker=verify Status=failed SHA=abc123", sl)
	}
}

func TestParse_ValidLineWithoutSHA(t *testing.T) {
	sl, ok := Parse("<!-- nuage-autopilot worker=work status=done -->\n完了した。")
	if !ok {
		t.Fatalf("Parse() ok = false, want true")
	}
	if sl.SHA != "" {
		t.Fatalf("sl.SHA = %q, want empty", sl.SHA)
	}
}

func TestParse_RejectsNonStatusLine(t *testing.T) {
	tests := []string{
		"ただの人間のコメント",
		"<!-- something else -->",
		"<!-- nuage-autopilot worker=work -->", // status 欠落
		"<!-- nuage-autopilot status=done -->", // worker 欠落
		"",
	}
	for _, body := range tests {
		if _, ok := Parse(body); ok {
			t.Fatalf("Parse(%q) ok = true, want false", body)
		}
	}
}

func TestParse_OnlyFirstLineIsConsidered(t *testing.T) {
	body := "1 行目は普通の文章\n<!-- nuage-autopilot worker=work status=done -->"
	if _, ok := Parse(body); ok {
		t.Fatalf("Parse() ok = true, want false (status line must be on the first line)")
	}
}

func TestRender_RoundTripsThroughParse(t *testing.T) {
	rendered := Render(WorkerVerify, StatusPassed, "deadbeef", "すべてのテストが通過した。")
	sl, ok := Parse(rendered)
	if !ok {
		t.Fatalf("Parse(Render(...)) ok = false, want true")
	}
	if sl.Worker != WorkerVerify || sl.Status != StatusPassed || sl.SHA != "deadbeef" {
		t.Fatalf("round-trip = %+v, want Worker=verify Status=passed SHA=deadbeef", sl)
	}
}

func TestRender_OmitsSHAWhenEmpty(t *testing.T) {
	rendered := Render(WorkerWork, StatusDone, "", "実装した。")
	if want := "<!-- nuage-autopilot worker=work status=done -->\n実装した。"; rendered != want {
		t.Fatalf("Render() = %q, want %q", rendered, want)
	}
}

func TestValidStatus(t *testing.T) {
	tests := []struct {
		worker, status string
		want           bool
	}{
		{WorkerWork, StatusDone, true},
		{WorkerWork, StatusBlocked, true},
		{WorkerWork, StatusPassed, false},
		{WorkerWork, StatusFailed, false},
		{WorkerVerify, StatusPassed, true},
		{WorkerVerify, StatusFailed, true},
		{WorkerVerify, StatusBlocked, true},
		{WorkerVerify, StatusDone, false},
		{"bogus", StatusDone, false},
	}
	for _, tt := range tests {
		if got := ValidStatus(tt.worker, tt.status); got != tt.want {
			t.Errorf("ValidStatus(%q, %q) = %v, want %v", tt.worker, tt.status, got, tt.want)
		}
	}
}

func TestReadResultFile_Success(t *testing.T) {
	path := filepath.Join(t.TempDir(), "report.json")
	if err := os.WriteFile(path, []byte(`{"status":"done","summary":"実装した"}`), 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	res, err := ReadResultFile(path)
	if err != nil {
		t.Fatalf("ReadResultFile() error = %v", err)
	}
	if res.Status != "done" || res.Summary != "実装した" {
		t.Fatalf("res = %+v, want Status=done Summary=実装した", res)
	}
}

func TestReadResultFile_MissingFile(t *testing.T) {
	_, err := ReadResultFile(filepath.Join(t.TempDir(), "does-not-exist.json"))
	if err == nil {
		t.Fatalf("ReadResultFile() error = nil, want an error when the file is missing")
	}
}

func TestReadResultFile_InvalidJSON(t *testing.T) {
	path := filepath.Join(t.TempDir(), "report.json")
	if err := os.WriteFile(path, []byte(`not json`), 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	if _, err := ReadResultFile(path); err == nil {
		t.Fatalf("ReadResultFile() error = nil, want an error for invalid JSON")
	}
}

func TestReadResultFile_MissingStatus(t *testing.T) {
	path := filepath.Join(t.TempDir(), "report.json")
	if err := os.WriteFile(path, []byte(`{"summary":"実装した"}`), 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	if _, err := ReadResultFile(path); err == nil {
		t.Fatalf("ReadResultFile() error = nil, want an error when status is missing")
	}
}
