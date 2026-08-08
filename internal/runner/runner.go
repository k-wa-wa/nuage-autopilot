// Package runner は外部プロセスを起動して完了を待つだけの薄い層である。
//
// どの CLI をどの引数で起動するか、プロンプトをどう渡すか、出力をどう解釈するかは
// すべて internal/agentcli の各クライアントが持つ（DESIGN.md 4章）。このパッケージは
// LLM CLI の存在すら知らず、コマンドと引数と標準入力を受け取って出力を返すに留める。
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

// Options は Run の入力である。
type Options struct {
	// Command は起動する実行ファイル名またはパス。必須。
	Command string

	// Args はコマンドライン引数である。
	Args []string

	// Stdin は標準入力へ流す内容である。空の場合、標準入力は接続しない。
	//
	// プロンプトを標準入力で渡せる CLI ではここに載せる。ARG_MAX の制限を避けられ、
	// `ps` のプロセス一覧にプロンプトが出ないためである。位置引数でしか受け取れない
	// CLI では Args 側に積むことになる（その CLI の制約であり、ここでは判断しない）。
	Stdin string

	// WorkDir はプロセスの作業ディレクトリ。必須。
	WorkDir string

	// ExtraEnv は buildEnv() の後ろに追加する環境変数（"KEY=VALUE" 形式）である。
	// 重複キーは後勝ちとなる（exec.Cmd.Env の仕様）。
	ExtraEnv []string

	// Logger は stdout/stderr を構造化ログとして出力する先。nil の場合 slog.Default()。
	Logger *slog.Logger
}

// Result はプロセス 1 回の実行結果である。
type Result struct {
	// ExitCode はプロセスの終了コード。
	ExitCode int

	// Success は ExitCode == 0 のときに true。
	Success bool

	// Duration は起動から終了までの所要時間。
	Duration time.Duration

	// Stdout は標準出力全体（行を "\n" で連結したもの）である。Logger には行単位で
	// 既に流しているが、`--output-format json` を指定した呼び出し元が応答 JSON を
	// パースできるよう、全体もあわせて保持しておく（internal/engine が session_id と
	// total_cost_usd を読む）。
	Stdout string
}

// Run はコマンドを起動し、完了を待つ。
//
// ctx をキャンセルするとプロセスは exec.CommandContext の挙動に従って強制終了される。
// stdout/stderr は行単位で読み取り、Logger に構造化ログとして流す。プロンプト全文や
// 認証トークンはここでは決してログに出さない（ログに流すのは CLI の出力のみであり、
// 入力であるプロンプトや環境変数の値そのものは対象外である）。
//
// 終了コードが 0 以外であっても、それ自体はこの関数のエラーとしない
// （Result.Success = false を返す）。呼び出し側は「LLM がタスクに失敗した」ことを
// 再試行対象として扱うべきものであり、nuage-autopilot 自体の異常とは区別する。
// プロセスの起動に失敗した場合や実行ファイルが見つからない場合など、CLI 自体を
// 実行できなかった場合にのみ error を返す。
func Run(ctx context.Context, opts Options) (Result, error) {
	if opts.WorkDir == "" {
		return Result{}, errors.New("runner: WorkDir is required")
	}

	if opts.Command == "" {
		return Result{}, errors.New("runner: Command is required")
	}
	command := opts.Command
	logger := opts.Logger
	if logger == nil {
		logger = slog.Default()
	}

	cmd := exec.CommandContext(ctx, command, opts.Args...)
	cmd.Dir = opts.WorkDir
	cmd.Env = append(buildEnv(), opts.ExtraEnv...)
	if opts.Stdin != "" {
		cmd.Stdin = strings.NewReader(opts.Stdin)
	}

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

	logger.Info("cli started", "command", command, "work_dir", opts.WorkDir)

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

	logger.Info("cli finished",
		"command", command,
		"exit_code", result.ExitCode,
		"success", result.Success,
		"duration", result.Duration.String(),
	)

	return result, nil
}

// streamToLog は r の内容を行単位で読み取り、構造化ログとして出力しつつ、
// 読み取った行をそのまま返す。扱うのは CLI の標準出力/標準エラーのみであり、
// プロンプト（入力）はここを通らない。
//
// 戻り値は Result.Stdout の組み立て（呼び出し元が応答 JSON をパースするために必要）に
// 使う。stderr の呼び出し元は戻り値を無視してよい。
func streamToLog(logger *slog.Logger, stream string, r io.Reader) []string {
	var lines []string
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 64*1024), 10*1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		lines = append(lines, line)
		logger.Info("cli output", "stream", stream, "line", line)
	}
	if err := scanner.Err(); err != nil {
		logger.Warn("failed to read cli output stream", "stream", stream, "error", err.Error())
	}
	return lines
}

// buildEnv はサブプロセスの環境変数を組み立てる。
//
// GIT_COMMITTER_NAME / GIT_COMMITTER_EMAIL を GIT_AUTHOR_NAME / GIT_AUTHOR_EMAIL と
// 同値にするのは、エージェントが自律的に実行する `git commit` のコミッター名義を
// コミット作成者名義と一致させるためである（DESIGN.md 13章の環境変数）。
// os.Environ() で継承した値の後に追記することで、後勝ちの exec.Cmd.Env の仕様
// （重複キーは最後の値が使われる）により確実に上書きされる。
//
// CLI / gh の認証情報（各 CLI のサインイン情報、GH_TOKEN）は os.Environ() をそのまま
// 継承することで渡る。ここで新たに特別な扱いはしない。
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
