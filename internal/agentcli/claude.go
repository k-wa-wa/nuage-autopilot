package agentcli

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"

	"autopilot/internal/runner"
)

// claudeClient は claude CLI 用の Client である。
type claudeClient struct {
	command string
	model   string
	logger  *slog.Logger
}

func newClaude(opts Options) Client {
	command := opts.Command
	if command == "" {
		command = "claude"
	}
	return &claudeClient{command: command, model: opts.Model, logger: opts.Logger}
}

func (c *claudeClient) Name() string { return "claude" }

// Run は claude を非対話モードで起動する。
//
// プロンプトは標準入力で渡す。ARG_MAX の制限を受けず、`ps` のプロセス一覧に
// プロンプトが出ないためである。
func (c *claudeClient) Run(ctx context.Context, req Request) (Result, error) {
	args := []string{
		"-p",                                     // 非対話モードで実行し、応答を出力して終了する
		"--permission-mode", "bypassPermissions", // 無人実行のためパーミッション確認で止めない
		"--output-format", "json", // session_id と total_cost_usd を得る
	}
	if c.model != "" {
		args = append(args, "--model="+c.model)
	}
	if req.ResumeID != "" {
		args = append(args, "--resume", req.ResumeID)
	}

	res, err := runner.Run(ctx, runner.Options{
		Command:  c.command,
		Args:     args,
		Stdin:    req.Prompt,
		WorkDir:  req.WorkDir,
		ExtraEnv: req.Env,
		Logger:   c.logger,
	})
	if err != nil {
		return Result{}, err
	}

	out := Result{ExitCode: res.ExitCode}
	var meta struct {
		SessionID    string   `json:"session_id"`
		TotalCostUSD *float64 `json:"total_cost_usd"`
	}
	if jsonErr := json.Unmarshal([]byte(res.Stdout), &meta); jsonErr != nil {
		// --output-format json を指定している以上、通常はここに来ない。
		// 実行自体は成功しているため、コストとセッション ID を欠いたまま返す。
		c.logger.Warn("claude: could not parse the json output",
			"error", fmt.Sprintf("%v", jsonErr))
		return out, nil
	}
	out.SessionID = meta.SessionID
	out.CostUSD = meta.TotalCostUSD
	return out, nil
}
