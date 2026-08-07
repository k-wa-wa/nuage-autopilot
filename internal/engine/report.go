package engine

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
)

// Outcome* はエージェントが NUAGE_REPORT_FILE に書く outcome の許容値である
// （DESIGN.md 8.3 節）。
const (
	OutcomeAsked       = "asked"
	OutcomeImplemented = "implemented"
	OutcomeSplit       = "split"
	OutcomeBlocked     = "blocked"
	OutcomeIdle        = "idle"
)

// OutcomeVerify* は verify が NUAGE_REPORT_FILE に書く outcome の許容値である
// （DESIGN.md 8.3 節）。agent 用とは別集合として扱う。verify に "implemented" を
// 名乗る余地を与えないためであり、逆に agent が検証結果を騙ることも防ぐ。
const (
	OutcomeVerifyPassed       = "verify_passed"
	OutcomeVerifyFailed       = "verify_failed"
	OutcomeVerifyInconclusive = "verify_inconclusive"
)

var validOutcomes = map[string]bool{
	OutcomeAsked:       true,
	OutcomeImplemented: true,
	OutcomeSplit:       true,
	OutcomeBlocked:     true,
	OutcomeIdle:        true,
}

var validVerifyOutcomes = map[string]bool{
	OutcomeVerifyPassed:       true,
	OutcomeVerifyFailed:       true,
	OutcomeVerifyInconclusive: true,
}

// agentResult は NUAGE_REPORT_FILE の内容である。
type agentResult struct {
	Outcome  string `json:"outcome"`
	Children []int  `json:"children"`
}

// readAgentResult は path から agentResult を読み取り、outcome が agent 用の
// 既知の値であることを検証する。
func readAgentResult(path string) (agentResult, error) {
	return readResult(path, validOutcomes)
}

// readVerifyResult は path から agentResult を読み取り、outcome が verify 用の
// 既知の値であることを検証する。
func readVerifyResult(path string) (agentResult, error) {
	return readResult(path, validVerifyOutcomes)
}

func readResult(path string, valid map[string]bool) (agentResult, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return agentResult{}, fmt.Errorf("read result file: %w", err)
	}

	var res agentResult
	if err := json.Unmarshal(data, &res); err != nil {
		return agentResult{}, fmt.Errorf("decode result file: %w", err)
	}
	if !valid[res.Outcome] {
		return agentResult{}, fmt.Errorf("%w: %q", errInvalidOutcome, res.Outcome)
	}
	return res, nil
}

var errInvalidOutcome = errors.New("invalid outcome")

// claudeMeta は claude --output-format json の応答ラッパのうち、
// internal/engine が必要とするフィールドだけを抜き出したものである
// （DESIGN.md 8.6 節: セッション継続、10章: 実測コストの累積）。
type claudeMeta struct {
	SessionID    string  `json:"session_id"`
	TotalCostUSD float64 `json:"total_cost_usd"`
}

// parseClaudeMeta は claude の標準出力（JSON 1 件）から session_id と
// total_cost_usd を抜き出す。--output-format json を指定している限り stdout は
// この JSON 1 件のみのはずである。パースに失敗した場合はゼロ値と err を返す。
// 呼び出し元はこれを致命的エラーとはせず、「コスト計上・セッション継続ができ
// なかった」以上の意味を持たせずに処理を続行してよい。
func parseClaudeMeta(stdout string) (claudeMeta, error) {
	var meta claudeMeta
	if err := json.Unmarshal([]byte(stdout), &meta); err != nil {
		return claudeMeta{}, fmt.Errorf("decode claude stdout as json: %w", err)
	}
	return meta, nil
}
