package runner

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

// writeFakeClaude はテスト用のフェイク claude 実行ファイルを書き出し、そのパスを返す。
// 実際の claude/API へは一切到達しない。標準入力の内容を stdinLogPath に書き出し、
// cwd を出力し、exitCode で終了する。
func writeFakeClaude(t *testing.T, exitCode int, stdinLogPath string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("fake claude script assumes a POSIX shell")
	}

	dir := t.TempDir()
	path := filepath.Join(dir, "claude")
	script := "#!/bin/sh\n" +
		"cat > " + shellQuote(stdinLogPath) + "\n" +
		"pwd\n" +
		"echo \"stderr-line\" 1>&2\n" +
		"echo \"GIT_COMMITTER_NAME=$GIT_COMMITTER_NAME\"\n" +
		"echo \"GIT_COMMITTER_EMAIL=$GIT_COMMITTER_EMAIL\"\n" +
		"exit " + strconv.Itoa(exitCode) + "\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake claude script: %v", err)
	}
	return path
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

func testLoggerTo(buf *bytes.Buffer) *slog.Logger {
	return slog.New(slog.NewTextHandler(buf, nil))
}

func TestRun_SuccessExitZero(t *testing.T) {
	workDir := t.TempDir()
	stdinLog := filepath.Join(t.TempDir(), "stdin.txt")
	fakeClaude := writeFakeClaude(t, 0, stdinLog)

	t.Setenv("GIT_AUTHOR_NAME", "nuage-autopilot")
	t.Setenv("GIT_AUTHOR_EMAIL", "nuage-autopilot@example.invalid")

	var logBuf bytes.Buffer
	result, err := Run(context.Background(), Options{
		Command: fakeClaude,
		WorkDir: workDir,
		Stdin:   "とても長い秘密の指示文プロンプト",
		Logger:  testLoggerTo(&logBuf),
	})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if !result.Success || result.ExitCode != 0 {
		t.Fatalf("result = %+v, want Success=true ExitCode=0", result)
	}
	if result.Duration <= 0 {
		t.Fatalf("result.Duration = %v, want > 0", result.Duration)
	}

	// プロンプトは標準入力経由で渡されている（位置引数ではない）。
	stdinContent, err := os.ReadFile(stdinLog)
	if err != nil {
		t.Fatalf("read stdin log: %v", err)
	}
	if string(stdinContent) != "とても長い秘密の指示文プロンプト" {
		t.Fatalf("stdin content = %q, want the prompt text", stdinContent)
	}

	logs := logBuf.String()
	// stdout/stderr はログに出力される。
	if !strings.Contains(logs, workDir) {
		t.Fatalf("logs do not contain cwd (%s) echoed by fake claude via stdout:\n%s", workDir, logs)
	}
	if !strings.Contains(logs, "stderr-line") {
		t.Fatalf("logs do not contain stderr output:\n%s", logs)
	}
	// GIT_COMMITTER_* が GIT_AUTHOR_* と同値で渡っている。
	if !strings.Contains(logs, "GIT_COMMITTER_NAME=nuage-autopilot") {
		t.Fatalf("logs do not show GIT_COMMITTER_NAME propagated from GIT_AUTHOR_NAME:\n%s", logs)
	}
	if !strings.Contains(logs, "GIT_COMMITTER_EMAIL=nuage-autopilot@example.invalid") {
		t.Fatalf("logs do not show GIT_COMMITTER_EMAIL propagated from GIT_AUTHOR_EMAIL:\n%s", logs)
	}
	// プロンプト全文をログに出していないこと。
	if strings.Contains(logs, "とても長い秘密の指示文プロンプト") {
		t.Fatalf("logs must not contain the prompt text itself:\n%s", logs)
	}
}

// writeArgEchoingFakeClaude はテスト用のフェイク CLI 実行ファイルを書き出す。
// 受け取った引数をそのまま 1 行ずつ標準出力に書き出して終了する。
// Options.Args が実際のコマンドライン引数として渡っていることを検証するために使う。
func writeArgEchoingFakeClaude(t *testing.T) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("fake cli script assumes a POSIX shell")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "cli")
	script := "#!/bin/sh\ncat > /dev/null\nfor a in \"$@\"; do echo \"$a\"; done\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake cli script: %v", err)
	}
	return path
}

// TestRun_PassesArgsVerbatim は Options.Args がそのままコマンドライン引数として渡ることを
// 検証する。どんな引数を組み立てるかは internal/agentcli の各クライアントの責務であり、
// このパッケージは受け取ったものをそのまま渡すだけである。
func TestRun_PassesArgsVerbatim(t *testing.T) {
	fakeCLI := writeArgEchoingFakeClaude(t)

	result, err := Run(context.Background(), Options{
		Command: fakeCLI,
		Args:    []string{"-p", "--output-format=json", "--model=some-model", "位置引数のプロンプト"},
		WorkDir: t.TempDir(),
	})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	for _, want := range []string{"-p", "--output-format=json", "--model=some-model", "位置引数のプロンプト"} {
		if !strings.Contains(result.Stdout, want) {
			t.Fatalf("arg %q was not passed: %q", want, result.Stdout)
		}
	}
}

// TestRun_OmitsStdinWhenEmpty は Stdin が空のとき標準入力を接続しないことを検証する。
// 標準入力からプロンプトを読まない CLI（agy 等）のための経路である。
func TestRun_OmitsStdinWhenEmpty(t *testing.T) {
	stdinLog := filepath.Join(t.TempDir(), "stdin.txt")
	fakeCLI := writeFakeClaude(t, 0, stdinLog)

	if _, err := Run(context.Background(), Options{
		Command: fakeCLI,
		Args:    []string{"-p"},
		WorkDir: t.TempDir(),
	}); err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	got, err := os.ReadFile(stdinLog)
	if err != nil {
		t.Fatalf("read stdin log: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("stdin should be empty, got %q", got)
	}
}

func TestRun_NonZeroExitIsNotAnError(t *testing.T) {
	workDir := t.TempDir()
	stdinLog := filepath.Join(t.TempDir(), "stdin.txt")
	fakeClaude := writeFakeClaude(t, 7, stdinLog)

	result, err := Run(context.Background(), Options{
		Command: fakeClaude,
		WorkDir: workDir,
		Stdin:   "fail please",
	})
	if err != nil {
		t.Fatalf("Run() error = %v, want nil (non-zero exit must not be a Go error)", err)
	}
	if result.Success {
		t.Fatalf("result.Success = true, want false")
	}
	if result.ExitCode != 7 {
		t.Fatalf("result.ExitCode = %d, want 7", result.ExitCode)
	}
}

func TestRun_RequiresWorkDir(t *testing.T) {
	_, err := Run(context.Background(), Options{Stdin: "x"})
	if err == nil {
		t.Fatalf("Run() error = nil, want an error when WorkDir is empty")
	}
}

func TestRun_CommandNotFoundReturnsError(t *testing.T) {
	_, err := Run(context.Background(), Options{
		Command: filepath.Join(t.TempDir(), "does-not-exist"),
		WorkDir: t.TempDir(),
		Stdin:   "x",
	})
	if err == nil {
		t.Fatalf("Run() error = nil, want an error when the executable does not exist")
	}
}

func TestRun_ContextCancellationStopsTheProcess(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fake claude script assumes a POSIX shell")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "claude")
	script := "#!/bin/sh\nsleep 30\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake claude script: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	started := time.Now()
	_, _ = Run(ctx, Options{
		Command: path,
		WorkDir: t.TempDir(),
		Stdin:   "x",
	})
	if elapsed := time.Since(started); elapsed > 10*time.Second {
		t.Fatalf("Run() took %v, want it to return promptly after context cancellation", elapsed)
	}
}
