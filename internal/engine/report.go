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
