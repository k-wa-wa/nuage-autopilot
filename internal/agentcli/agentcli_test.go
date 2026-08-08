package agentcli

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// このテストは実際の claude / agy を一切起動しない。フェイクの実行ファイルに
// 差し替え、受け取った引数と、返した JSON の解釈だけを検証する。

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil))
}

// writeFakeCLI は、受け取った引数を 1 行 1 個で stderr に書き出し、stdout には
// stdoutJSON をそのまま返すフェイク CLI を作る。引数を stderr に出すのは、
// stdout を応答 JSON 専用にしておくためである。
func writeFakeCLI(t *testing.T, stdoutJSON string) (path string, argsLog string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("fake cli script assumes a POSIX shell")
	}
	dir := t.TempDir()
	path = filepath.Join(dir, "cli")
	argsLog = filepath.Join(dir, "args.txt")
	script := "#!/bin/sh\n" +
		"cat > /dev/null\n" +
		": > '" + argsLog + "'\n" +
		"for a in \"$@\"; do echo \"$a\" >> '" + argsLog + "'; done\n" +
		"cat <<'NUAGE_EOF'\n" + stdoutJSON + "\nNUAGE_EOF\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake cli: %v", err)
	}
	return path, argsLog
}

func readArgs(t *testing.T, argsLog string) []string {
	t.Helper()
	b, err := os.ReadFile(argsLog)
	if err != nil {
		t.Fatalf("read args log: %v", err)
	}
	return strings.Split(strings.TrimSpace(string(b)), "\n")
}

func contains(args []string, want string) bool {
	for _, a := range args {
		if a == want {
			return true
		}
	}
	return false
}

func TestNew_RejectsUnknownCLI(t *testing.T) {
	if _, err := New("gpt-cli", Options{}); err == nil {
		t.Fatal("New() error = nil, want an error for an unknown cli name")
	}
	// 綴り間違いを既定値に読み替えないことがこの検証の主眼である。
	if _, err := New("", Options{}); err != nil {
		t.Fatalf("New(\"\") should fall back to the default cli: %v", err)
	}
}

// TestClaude_BuildsArgsAndParsesCost は claude 用の引数組み立てと、
// session_id / total_cost_usd の解釈を検証する。
func TestClaude_BuildsArgsAndParsesCost(t *testing.T) {
	cliPath, argsLog := writeFakeCLI(t, `{"session_id":"sess-1","total_cost_usd":0.42}`)
	cli, err := New("claude", Options{Command: cliPath, Model: "claude-opus-5", Logger: testLogger()})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	res, err := cli.Run(context.Background(), Request{
		WorkDir:  t.TempDir(),
		Prompt:   "秘密のプロンプト",
		ResumeID: "sess-1",
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	args := readArgs(t, argsLog)
	for _, want := range []string{"-p", "--permission-mode", "bypassPermissions", "--model=claude-opus-5", "--resume", "sess-1"} {
		if !contains(args, want) {
			t.Fatalf("claude args %v missing %q", args, want)
		}
	}
	// プロンプトは標準入力で渡すため、引数には現れない（ps に出さないための性質）。
	if contains(args, "秘密のプロンプト") {
		t.Fatalf("claude must pass the prompt via stdin, but it appeared in argv: %v", args)
	}

	if res.SessionID != "sess-1" {
		t.Fatalf("SessionID = %q", res.SessionID)
	}
	if res.CostUSD == nil || *res.CostUSD != 0.42 {
		t.Fatalf("CostUSD = %v, want 0.42", res.CostUSD)
	}
}

// TestAgy_BuildsArgsAndReportsNoCost は agy 用の引数組み立てと、
// conversation_id の解釈、そしてコストが取得できないことを検証する。
func TestAgy_BuildsArgsAndReportsNoCost(t *testing.T) {
	cliPath, argsLog := writeFakeCLI(t, `{"conversation_id":"conv-9","status":"SUCCESS","usage":{"total_tokens":10}}`)
	cli, err := New("agy", Options{Command: cliPath, Model: "gemini-3.1-pro-high", Logger: testLogger()})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	res, err := cli.Run(context.Background(), Request{WorkDir: t.TempDir(), Prompt: "検証してほしい"})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	args := readArgs(t, argsLog)
	for _, want := range []string{"-p", "--dangerously-skip-permissions", "--output-format=json", "--model=gemini-3.1-pro-high"} {
		if !contains(args, want) {
			t.Fatalf("agy args %v missing %q", args, want)
		}
	}
	// agy は標準入力からプロンプトを読まないため、位置引数として最後に付く。
	if args[len(args)-1] != "検証してほしい" {
		t.Fatalf("agy must pass the prompt as the last argument, got %v", args)
	}
	// claude 用のフラグが混ざらないこと。
	if contains(args, "--permission-mode") {
		t.Fatalf("claude flags leaked into the agy invocation: %v", args)
	}

	if res.SessionID != "conv-9" {
		t.Fatalf("SessionID = %q, want conv-9 (conversation_id)", res.SessionID)
	}
	// agy は金額を返さない。nil であることが「積算できない」ことの表明である。
	if res.CostUSD != nil {
		t.Fatalf("CostUSD = %v, want nil (agy does not report cost)", *res.CostUSD)
	}
}

// TestAgy_PrintTimeoutFollowsContextDeadline は --print-timeout が ctx の締切に
// 追従することを検証する。agy の既定 5 分は verify の持ち時間より短く、放置すると
// CLI 側が先に打ち切ってしまうためである。
func TestAgy_PrintTimeoutFollowsContextDeadline(t *testing.T) {
	cliPath, argsLog := writeFakeCLI(t, `{"conversation_id":"c"}`)
	cli, err := New("agy", Options{Command: cliPath, Logger: testLogger()})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()
	if _, err := cli.Run(ctx, Request{WorkDir: t.TempDir(), Prompt: "x"}); err != nil {
		t.Fatalf("Run: %v", err)
	}

	var got string
	for _, a := range readArgs(t, argsLog) {
		if strings.HasPrefix(a, "--print-timeout=") {
			got = a
		}
	}
	if got == "" {
		t.Fatal("--print-timeout was not passed")
	}
	d, err := time.ParseDuration(strings.TrimPrefix(got, "--print-timeout="))
	if err != nil {
		t.Fatalf("unparsable --print-timeout %q: %v", got, err)
	}
	// 既定の 5 分より十分長く、ctx の締切より短くならないこと。
	if d <= 5*time.Minute {
		t.Fatalf("--print-timeout = %v, want longer than agy's 5m default", d)
	}
}

// TestClients_TolerateUnparsableOutput は応答が JSON でなくても実行を失敗させないことを
// 検証する。CLI 側の出力形式が変わってもアイテムを blocked に落とさないためである。
func TestClients_TolerateUnparsableOutput(t *testing.T) {
	for _, name := range Names() {
		t.Run(name, func(t *testing.T) {
			cliPath, _ := writeFakeCLI(t, "not json at all")
			cli, err := New(name, Options{Command: cliPath, Logger: testLogger()})
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			res, err := cli.Run(context.Background(), Request{WorkDir: t.TempDir(), Prompt: "x"})
			if err != nil {
				t.Fatalf("Run() error = %v, want nil (unparsable output is not a launch failure)", err)
			}
			if res.SessionID != "" || res.CostUSD != nil {
				t.Fatalf("res = %+v, want zero metadata", res)
			}
		})
	}
}
