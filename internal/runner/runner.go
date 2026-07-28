// Package runner は LLM CLI (claude) をheadlessモードで起動する。
//
// DESIGN.md フェーズ3の決定に従い、使用する CLI は claude のみである（旧 nuage-agent の
// agy/Antigravity は使わない）。
package runner

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// Command は起動する claude CLI の実行ファイル名である。絶対パスは持たず、PATH解決に
// 委ねる。nix/modules/nuage-autopilot.nix の extraPathPrefixes がサービスの PATH に
// claude を通す（claude は Nix パッケージではなく公式インストーラで導入するため）。
const Command = "claude"

// claude 2.1.220 の --help で確認したフラグのうち採用したもの:
//
//	-p, --print                    非対話モードで実行し、応答を出力して終了する
//	--permission-mode bypassPermissions
//	                                無人実行のため、パーミッション確認で停止しないようにする
//	                                （手元で `echo ... | claude -p --permission-mode bypassPermissions`
//	                                を実際に実行し、確認・停止なく完了することを確認済み）。
//
// --output-format は既定では指定しない（既定の "text"）。worker はこの既定のまま使う。
// --print と組み合わせても、標準出力は色付け等のない素のテキストであることを
// 手元で確認済みであり、json 形式が持つ cost/duration 等のメタデータよりも、
// 応答本文をそのまま構造化ログの行として残せる単純さを優先した。
//
// 一方 dispatcher は判断結果を Go 側でパースする必要があるため、Options.ExtraArgs で
// `--output-format json --json-schema <schema>` を追加指定する（internal/cycle/dispatcher.go
// 参照）。`claude --help` で確認した限り、--json-schema を付けると応答の JSON ラッパの
// 直下に "structured_output" フィールドとしてスキーマに沿った JSON が渡ってくる。
// これは "result" フィールド（応答本文の文字列。--json-schema 無指定時は Markdown の
// コードフェンスで囲まれることがある）よりも厳密で扱いやすいため、dispatcher は
// structured_output を優先して読む。
var runArgs = []string{"-p", "--permission-mode", "bypassPermissions"}

// Options は Run の入力である。
type Options struct {
	// Command は起動する実行ファイル名またはパス。空の場合は Command 定数 ("claude") を
	// 使う。テストでフェイクの実行系に差し替えるために公開している。
	Command string

	// WorkDir は claude を起動する作業ディレクトリ（対象リポジトリの clone 先）である。
	// 必須。dispatcher のように clone を伴わない呼び出しでは StateDir 等、存在する
	// 任意のディレクトリを指定してよい。
	WorkDir string

	// Prompt は claude に渡す指示文である。コマンドライン引数ではなく標準入力経由で
	// 渡す。理由は次の2点である。
	//   - OS の引数長制限（ARG_MAX）を避けるため（プロンプトは長大になりうる）。
	//   - `ps` 等のプロセス一覧からプロンプトの内容が他ユーザーに見えてしまうことを
	//     避けるため。
	Prompt string

	// Model は --model に渡すモデル名。空の場合は --model を付けず claude の既定
	// モデルを使う（worker はこちら）。dispatcher は判断のみで実装を伴わないため
	// haiku を明示指定する（DESIGN.md 8章「dispatcher の契約」）。
	Model string

	// ExtraArgs は runArgs の後ろにそのまま追加する追加のコマンドライン引数である。
	// dispatcher が構造化出力を要求する `--output-format json --json-schema <schema>`
	// を渡すために用意した。worker は指定しない。
	ExtraArgs []string

	// Logger は stdout/stderr を構造化ログとして出力する先。nil の場合 slog.Default()
	// を使う。
	Logger *slog.Logger
}

// Result は claude CLI 1 回の実行結果である。
type Result struct {
	// ExitCode はプロセスの終了コード。
	ExitCode int

	// Success は ExitCode == 0 のときに true。
	Success bool

	// Duration は起動から終了までの所要時間。
	Duration time.Duration

	// Stdout は標準出力全体（行を "\n" で連結したもの）である。Logger には行単位で
	// 既に流しているが、dispatcher は claude の JSON 応答をパースする必要があるため、
	// この呼び出し元向けに全体もあわせて保持しておく。worker の呼び出し元はこの値を
	// 使わなくてよい。
	Stdout string
}

// Run は claude を headless モード（-p / --print）で起動し、完了を待つ。
//
// ctx をキャンセルするとプロセスは exec.CommandContext の挙動に従って強制終了される。
// stdout/stderr は行単位で読み取り、Logger に構造化ログとして流す。プロンプト全文や
// 認証トークンはここでは決してログに出さない（ログに流すのは claude の出力のみであり、
// 入力であるプロンプトや環境変数の値そのものは対象外である）。
//
// 終了コードが 0 以外であっても、それ自体はこの関数のエラーとしない
// （Result.Success = false を返す）。呼び出し側（internal/cycle）は「LLM が
// タスクに失敗した」ことを次サイクルでの再試行対象として扱うべきものであり、
// nuage-autopilot 自体の異常とは区別する。プロセスの起動に失敗した場合や
// 実行ファイルが見つからない場合など、claude自体を実行できなかった場合にのみ
// error を返す。
func Run(ctx context.Context, opts Options) (Result, error) {
	if opts.WorkDir == "" {
		return Result{}, errors.New("runner: WorkDir is required")
	}

	command := opts.Command
	if command == "" {
		command = Command
	}
	logger := opts.Logger
	if logger == nil {
		logger = slog.Default()
	}

	args := make([]string, 0, len(runArgs)+2+len(opts.ExtraArgs))
	args = append(args, runArgs...)
	if opts.Model != "" {
		args = append(args, "--model", opts.Model)
	}
	args = append(args, opts.ExtraArgs...)

	cmd := exec.CommandContext(ctx, command, args...)
	cmd.Dir = opts.WorkDir
	cmd.Env = buildEnv()
	cmd.Stdin = strings.NewReader(opts.Prompt)

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return Result{}, fmt.Errorf("runner: attach stdout pipe: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return Result{}, fmt.Errorf("runner: attach stderr pipe: %w", err)
	}

	cmd.WaitDelay = 2 * time.Second

	started := time.Now()
	if err := cmd.Start(); err != nil {
		return Result{}, fmt.Errorf("runner: start %s: %w", command, err)
	}

	logger.Info("claude started", "command", command, "work_dir", opts.WorkDir)

	var stdoutLines []string
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		stdoutLines = streamToLog(logger, "stdout", stdout)
	}()
	go func() {
		defer wg.Done()
		streamToLog(logger, "stderr", stderr)
	}()

	waitErr := cmd.Wait()
	wg.Wait()
	duration := time.Since(started)

	result := Result{Duration: duration, Stdout: strings.Join(stdoutLines, "\n")}

	var exitErr *exec.ExitError
	switch {
	case waitErr == nil:
		result.ExitCode = 0
		result.Success = true
	case errors.As(waitErr, &exitErr):
		result.ExitCode = exitErr.ExitCode()
		result.Success = false
	default:
		// プロセスの開始/待機自体が失敗した場合（実行ファイル不在、コンテキストの
		// キャンセルによる強制終了など）は runner 自身のエラーとして返す。
		return result, fmt.Errorf("runner: wait for %s: %w", command, waitErr)
	}

	logger.Info("claude finished",
		"command", command,
		"exit_code", result.ExitCode,
		"success", result.Success,
		"duration", result.Duration.String(),
	)

	return result, nil
}

// streamToLog は r の内容を行単位で読み取り、構造化ログとして出力しつつ、
// 読み取った行をそのまま返す。claude の標準出力/標準エラーのみを扱い、
// プロンプト（入力）はここを通らない。
//
// 戻り値は Result.Stdout の組み立て（dispatcher が claude の JSON 応答をパースする
// ために必要）に使う。stderr の呼び出し元は戻り値を無視してよい。
func streamToLog(logger *slog.Logger, stream string, r io.Reader) []string {
	var lines []string
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		lines = append(lines, line)
		logger.Info("claude output", "stream", stream, "line", line)
	}
	if err := scanner.Err(); err != nil {
		logger.Warn("failed to read claude output stream", "stream", stream, "error", err.Error())
	}
	return lines
}

// buildEnv は claude サブプロセスの環境変数を組み立てる。
//
// GIT_COMMITTER_NAME / GIT_COMMITTER_EMAIL を GIT_AUTHOR_NAME / GIT_AUTHOR_EMAIL と
// 同値にするのは、claude が自律的に実行する `git commit` のコミッター名義を
// コミット作成者名義と一致させるためである（DESIGN.md 10.5節、実装指示4項）。
// os.Environ() で継承した値の後に追記することで、後勝ちの exec.Cmd.Env の仕様
// （重複キーは最後の値が使われる）により確実に上書きされる。
//
// claude / gh の認証情報（Claude の TUI サインイン情報、GH_TOKEN）は os.Environ() を
// そのまま継承することで渡る。ここで新たに特別な扱いはしない。
func buildEnv() []string {
	env := os.Environ()
	if name := os.Getenv("GIT_AUTHOR_NAME"); name != "" {
		env = append(env, "GIT_COMMITTER_NAME="+name)
	}
	if email := os.Getenv("GIT_AUTHOR_EMAIL"); email != "" {
		env = append(env, "GIT_COMMITTER_EMAIL="+email)
	}
	return env
}
