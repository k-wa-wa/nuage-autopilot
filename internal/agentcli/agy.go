package agentcli

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"autopilot/internal/runner"
)

// agyClient は antigravity CLI（agy）用の Client である。
//
// claude との差は名前だけではない。実測した範囲では次のように異なる。
//   - プロンプトを標準入力から読まない。位置引数で渡す必要がある
//   - 無人実行のフラグが --dangerously-skip-permissions である
//   - セッション ID が conversation_id で、継続は --conversation である
//   - **コストを返さない。** usage にトークン数があるだけで金額は含まれない
//   - --print-timeout の既定が 5 分であり、それを超えると CLI 側が自ら打ち切る
type agyClient struct {
	command string
	model   string
	logger  *slog.Logger
}

func newAgy(opts Options) Client {
	command := opts.Command
	if command == "" {
		command = "agy"
	}
	return &agyClient{command: command, model: opts.Model, logger: opts.Logger}
}

func (c *agyClient) Name() string { return "agy" }

// Run は agy を非対話モードで起動する。
//
// プロンプトは位置引数として渡す。agy が標準入力からプロンプトを読まないためであり、
// その結果 `ps` のプロセス一覧にプロンプトが見える。単一ユーザーのホストで動かす前提の
// 割り切りである。
func (c *agyClient) Run(ctx context.Context, req Request) (Result, error) {
	args := []string{
		"-p",                             // 非対話モードで実行し、応答を出力して終了する
		"--dangerously-skip-permissions", // 無人実行のためパーミッション確認で止めない
		"--output-format=json",
		// --print-timeout の既定 5 分は verify の持ち時間より短い。呼び出し側が
		// ctx に持たせた締切に合わせ、CLI 側で先に打ち切られないようにする。
		"--print-timeout=" + printTimeout(ctx).String(),
	}
	if c.model != "" {
		args = append(args, "--model="+c.model)
	}
	if req.ResumeID != "" {
		// 2 トークン形式が期待どおり解釈されない場合があるため = 形式で渡す。
		args = append(args, "--conversation="+req.ResumeID)
	}
	args = append(args, req.Prompt)

	res, err := runner.Run(ctx, runner.Options{
		Command:  c.command,
		Args:     args,
		WorkDir:  req.WorkDir,
		ExtraEnv: req.Env,
		Logger:   c.logger,
	})
	if err != nil {
		return Result{}, err
	}

	out := Result{ExitCode: res.ExitCode}
	var meta struct {
		ConversationID string `json:"conversation_id"`
	}
	if jsonErr := json.Unmarshal([]byte(res.Stdout), &meta); jsonErr != nil {
		c.logger.Warn("agy: could not parse the json output",
			"error", fmt.Sprintf("%v", jsonErr))
		return out, nil
	}
	out.SessionID = meta.ConversationID
	// CostUSD は nil のままにする。agy は金額を返さないためである。
	return out, nil
}

// defaultPrintTimeout は ctx に締切が無い場合に --print-timeout へ渡す値である。
const defaultPrintTimeout = 30 * time.Minute

// printTimeout は ctx の残り時間から --print-timeout に渡す値を決める。
// CLI 側のタイムアウトが Go 側より先に来ると、打ち切りの理由が分かりにくくなるため、
// 残り時間より少しだけ長く取る。
func printTimeout(ctx context.Context) time.Duration {
	deadline, ok := ctx.Deadline()
	if !ok {
		return defaultPrintTimeout
	}
	remaining := time.Until(deadline) + time.Minute
	if remaining < time.Minute {
		return time.Minute
	}
	return remaining.Round(time.Second)
}
