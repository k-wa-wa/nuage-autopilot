package cycle

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"autopilot/internal/github"
)

// このファイルのテストは実際の claude を一切起動しない。DefaultDispatcher.Command に
// フェイクの実行ファイル（POSIX シェルスクリプト）を差し替えて検証する。

func testLoggerDiscard() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelError + 1}))
}

// writeFakeDispatcherClaude はテスト用のフェイク claude 実行ファイルを書き出す。
// 起動されるたびに 1 ずつ増える呼び出し回数を counterPath に記録し、その回数に応じて
// wrapperByAttempt の中身をそのまま標準出力へ書き出す（1-indexed）。
// 呼び出し回数が wrapperByAttempt の要素数を超えた場合は最後の要素を使い続ける。
// 受け取った引数は argsLogPath に 1 行として追記する。
func writeFakeDispatcherClaude(t *testing.T, argsLogPath, counterPath string, wrapperByAttempt []string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("fake claude script assumes a POSIX shell")
	}

	dir := t.TempDir()
	scriptPath := filepath.Join(dir, "claude")

	var b strings.Builder
	b.WriteString("#!/bin/sh\n")
	b.WriteString("cat > /dev/null\n")
	b.WriteString("echo \"$@\" >> " + shellQuoteDispatcher(argsLogPath) + "\n")
	b.WriteString("if [ -f " + shellQuoteDispatcher(counterPath) + " ]; then N=$(cat " + shellQuoteDispatcher(counterPath) + "); else N=0; fi\n")
	b.WriteString("N=$((N+1))\n")
	b.WriteString("echo \"$N\" > " + shellQuoteDispatcher(counterPath) + "\n")

	for i, wrapper := range wrapperByAttempt {
		attempt := i + 1
		wrapperFile := filepath.Join(dir, "wrapper-"+strconv.Itoa(attempt)+".json")
		if err := os.WriteFile(wrapperFile, []byte(wrapper), 0o644); err != nil {
			t.Fatalf("write wrapper fixture: %v", err)
		}
		if i == 0 {
			b.WriteString("if [ \"$N\" -le " + strconv.Itoa(attempt) + " ]; then cat " + shellQuoteDispatcher(wrapperFile) + "; exit 0; fi\n")
		} else {
			b.WriteString("if [ \"$N\" -eq " + strconv.Itoa(attempt) + " ]; then cat " + shellQuoteDispatcher(wrapperFile) + "; exit 0; fi\n")
		}
	}
	// 呼び出し回数が指定より多い場合は最後の wrapper を使い続ける。
	if len(wrapperByAttempt) > 0 {
		lastFile := filepath.Join(dir, "wrapper-"+strconv.Itoa(len(wrapperByAttempt))+".json")
		b.WriteString("cat " + shellQuoteDispatcher(lastFile) + "\n")
	}

	if err := os.WriteFile(scriptPath, []byte(b.String()), 0o755); err != nil {
		t.Fatalf("write fake claude script: %v", err)
	}
	return scriptPath
}

func shellQuoteDispatcher(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

func sampleCandidates() []DispatchCandidate {
	return []DispatchCandidate{
		{Kind: kindIssue, Number: 1, Title: "issue one", Author: "alice", UpdatedAt: time.Now()},
		{Kind: kindPullRequest, Number: 2, Title: "pr two", Author: "bob", UpdatedAt: time.Now()},
	}
}

func TestDefaultDispatcher_Dispatch_ParsesStructuredOutputOnFirstAttempt(t *testing.T) {
	dir := t.TempDir()
	argsLog := filepath.Join(dir, "args.log")
	counter := filepath.Join(dir, "counter")
	wrapper := `{"is_error":false,"result":"{\"number\":1,\"kind\":\"issue\",\"worker\":\"spec\",\"reason\":\"new issue\"}","structured_output":{"number":1,"kind":"issue","worker":"spec","reason":"new issue"}}`
	fakeClaude := writeFakeDispatcherClaude(t, argsLog, counter, []string{wrapper})

	d := &DefaultDispatcher{StateDir: t.TempDir(), Logger: testLoggerDiscard(), Command: fakeClaude}
	decision, ok, err := d.Dispatch(context.Background(), "k-wa-wa/pechka", sampleCandidates())
	if err != nil {
		t.Fatalf("Dispatch() error = %v", err)
	}
	if !ok {
		t.Fatalf("Dispatch() ok = false, want true")
	}
	if decision.Number != 1 || decision.Kind != "issue" || decision.Worker != WorkerSpec {
		t.Fatalf("decision = %+v, want number=1 kind=issue worker=spec", decision)
	}

	argsContent, err := os.ReadFile(argsLog)
	if err != nil {
		t.Fatalf("read args log: %v", err)
	}
	if !strings.Contains(string(argsContent), DispatcherModel) {
		t.Fatalf("args log = %q, want it to contain the dispatcher model %q", argsContent, DispatcherModel)
	}
	if !strings.Contains(string(argsContent), "--json-schema") {
		t.Fatalf("args log = %q, want it to contain --json-schema", argsContent)
	}

	counterContent, _ := os.ReadFile(counter)
	if strings.TrimSpace(string(counterContent)) != "1" {
		t.Fatalf("claude was invoked %s times, want exactly 1 (no retry needed)", strings.TrimSpace(string(counterContent)))
	}
}

func TestDefaultDispatcher_Dispatch_RetriesOnceOnInvalidJSONThenSucceeds(t *testing.T) {
	dir := t.TempDir()
	argsLog := filepath.Join(dir, "args.log")
	counter := filepath.Join(dir, "counter")
	invalid := `not valid json at all`
	valid := `{"is_error":false,"result":"{\"number\":2,\"kind\":\"pull_request\",\"worker\":\"review\",\"reason\":\"needs review\"}","structured_output":{"number":2,"kind":"pull_request","worker":"review","reason":"needs review"}}`
	fakeClaude := writeFakeDispatcherClaude(t, argsLog, counter, []string{invalid, valid})

	d := &DefaultDispatcher{StateDir: t.TempDir(), Logger: testLoggerDiscard(), Command: fakeClaude}
	decision, ok, err := d.Dispatch(context.Background(), "k-wa-wa/pechka", sampleCandidates())
	if err != nil {
		t.Fatalf("Dispatch() error = %v", err)
	}
	if !ok {
		t.Fatalf("Dispatch() ok = false, want true after a successful retry")
	}
	if decision.Number != 2 || decision.Kind != "pull_request" || decision.Worker != WorkerReview {
		t.Fatalf("decision = %+v, want number=2 kind=pull_request worker=review", decision)
	}

	counterContent, _ := os.ReadFile(counter)
	if strings.TrimSpace(string(counterContent)) != "2" {
		t.Fatalf("claude was invoked %s times, want exactly 2 (1 retry)", strings.TrimSpace(string(counterContent)))
	}
}

func TestDefaultDispatcher_Dispatch_GivesUpAfterRetryExhausted(t *testing.T) {
	dir := t.TempDir()
	argsLog := filepath.Join(dir, "args.log")
	counter := filepath.Join(dir, "counter")
	invalid := `still not valid json`
	fakeClaude := writeFakeDispatcherClaude(t, argsLog, counter, []string{invalid})

	d := &DefaultDispatcher{StateDir: t.TempDir(), Logger: testLoggerDiscard(), Command: fakeClaude}
	decision, ok, err := d.Dispatch(context.Background(), "k-wa-wa/pechka", sampleCandidates())
	if err != nil {
		t.Fatalf("Dispatch() error = %v, want nil (a persistently failing dispatcher must not fail the cycle)", err)
	}
	if ok {
		t.Fatalf("Dispatch() ok = true, want false")
	}
	if decision != (Decision{}) {
		t.Fatalf("decision = %+v, want zero value", decision)
	}

	counterContent, _ := os.ReadFile(counter)
	if strings.TrimSpace(string(counterContent)) != "2" {
		t.Fatalf("claude was invoked %s times, want exactly 2 (max attempts)", strings.TrimSpace(string(counterContent)))
	}
}

func TestDefaultDispatcher_Dispatch_WorkerNoneIsNotOK(t *testing.T) {
	dir := t.TempDir()
	argsLog := filepath.Join(dir, "args.log")
	counter := filepath.Join(dir, "counter")
	wrapper := `{"is_error":false,"result":"{\"worker\":\"none\",\"reason\":\"nothing to do\"}","structured_output":{"worker":"none","reason":"nothing to do"}}`
	fakeClaude := writeFakeDispatcherClaude(t, argsLog, counter, []string{wrapper})

	d := &DefaultDispatcher{StateDir: t.TempDir(), Logger: testLoggerDiscard(), Command: fakeClaude}
	decision, ok, err := d.Dispatch(context.Background(), "k-wa-wa/pechka", sampleCandidates())
	if err != nil {
		t.Fatalf("Dispatch() error = %v", err)
	}
	if ok {
		t.Fatalf("Dispatch() ok = true, want false for worker=none")
	}
	if decision != (Decision{}) {
		t.Fatalf("decision = %+v, want zero value", decision)
	}
}

func TestDefaultDispatcher_Dispatch_RejectsItemOutsideCandidateSet(t *testing.T) {
	dir := t.TempDir()
	argsLog := filepath.Join(dir, "args.log")
	counter := filepath.Join(dir, "counter")
	// number=999 は sampleCandidates() に含まれない。
	wrapper := `{"is_error":false,"result":"{\"number\":999,\"kind\":\"issue\",\"worker\":\"spec\",\"reason\":\"bogus\"}","structured_output":{"number":999,"kind":"issue","worker":"spec","reason":"bogus"}}`
	fakeClaude := writeFakeDispatcherClaude(t, argsLog, counter, []string{wrapper})

	d := &DefaultDispatcher{StateDir: t.TempDir(), Logger: testLoggerDiscard(), Command: fakeClaude}
	_, ok, err := d.Dispatch(context.Background(), "k-wa-wa/pechka", sampleCandidates())
	if err != nil {
		t.Fatalf("Dispatch() error = %v", err)
	}
	if ok {
		t.Fatalf("Dispatch() ok = true, want false for a decision outside the candidate set")
	}

	counterContent, _ := os.ReadFile(counter)
	if strings.TrimSpace(string(counterContent)) != "2" {
		t.Fatalf("claude was invoked %s times, want exactly 2 (validation failure must also retry)", strings.TrimSpace(string(counterContent)))
	}
}

func TestDefaultDispatcher_Dispatch_RejectsWorkerKindMismatch(t *testing.T) {
	dir := t.TempDir()
	argsLog := filepath.Join(dir, "args.log")
	counter := filepath.Join(dir, "counter")
	// number=2 は sampleCandidates() 内で kind=pull_request だが、worker=spec は
	// Issue にしか選べない。
	wrapper := `{"is_error":false,"result":"{\"number\":2,\"kind\":\"pull_request\",\"worker\":\"spec\",\"reason\":\"bogus\"}","structured_output":{"number":2,"kind":"pull_request","worker":"spec","reason":"bogus"}}`
	fakeClaude := writeFakeDispatcherClaude(t, argsLog, counter, []string{wrapper})

	d := &DefaultDispatcher{StateDir: t.TempDir(), Logger: testLoggerDiscard(), Command: fakeClaude}
	_, ok, err := d.Dispatch(context.Background(), "k-wa-wa/pechka", sampleCandidates())
	if err != nil {
		t.Fatalf("Dispatch() error = %v", err)
	}
	if ok {
		t.Fatalf("Dispatch() ok = true, want false when worker is not valid for the item's kind")
	}
}

func TestValidateDecision(t *testing.T) {
	candidates := sampleCandidates()

	tests := []struct {
		name    string
		d       Decision
		wantErr bool
	}{
		{"valid spec on issue", Decision{Number: 1, Kind: "issue", Worker: WorkerSpec}, false},
		{"valid review on pr", Decision{Number: 2, Kind: "pull_request", Worker: WorkerReview}, false},
		{"valid dev on issue", Decision{Number: 1, Kind: "issue", Worker: WorkerDev}, false},
		{"valid dev on pr", Decision{Number: 2, Kind: "pull_request", Worker: WorkerDev}, false},
		{"none is always valid", Decision{Worker: WorkerNone}, false},
		{"invalid worker", Decision{Number: 1, Kind: "issue", Worker: "bogus"}, true},
		{"spec on pr is invalid", Decision{Number: 2, Kind: "pull_request", Worker: WorkerSpec}, true},
		{"qa on issue is invalid", Decision{Number: 1, Kind: "issue", Worker: WorkerQA}, true},
		{"number not in candidate set", Decision{Number: 42, Kind: "issue", Worker: WorkerSpec}, true},
		{"kind mismatch for existing number", Decision{Number: 1, Kind: "pull_request", Worker: WorkerDev}, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateDecision(tt.d, candidates)
			if (err != nil) != tt.wantErr {
				t.Fatalf("validateDecision(%+v) error = %v, wantErr %v", tt.d, err, tt.wantErr)
			}
		})
	}
}

func TestBuildDispatchCandidates_SortsTruncatesAndLimitsComments(t *testing.T) {
	now := time.Now()
	longIssueBody := strings.Repeat("い", bodyPreviewLimit+50)
	items := []Item{
		{Kind: kindIssue, Number: 1, Title: "t", Author: "alice", Body: longIssueBody, UpdatedAt: now},
	}
	longBody := strings.Repeat("あ", commentPreviewLimit+50)
	// recentCommentLimit (8) より多い 10 件を用意し、切り捨てが実際に発生することを検証する。
	comments := map[int][]github.Comment{
		1: {
			{Body: "oldest", User: github.Author{Login: "alice", Type: "User"}, CreatedAt: now.Add(-10 * time.Hour)},
			{Body: "c9", User: github.Author{Login: "nuage-autopilot", Type: "User"}, CreatedAt: now.Add(-9 * time.Hour)},
			{Body: "c8", User: github.Author{Login: "nuage-autopilot", Type: "User"}, CreatedAt: now.Add(-8 * time.Hour)},
			{Body: "c7", User: github.Author{Login: "nuage-autopilot", Type: "User"}, CreatedAt: now.Add(-7 * time.Hour)},
			{Body: "c6", User: github.Author{Login: "nuage-autopilot", Type: "User"}, CreatedAt: now.Add(-6 * time.Hour)},
			{Body: "c5", User: github.Author{Login: "nuage-autopilot", Type: "User"}, CreatedAt: now.Add(-5 * time.Hour)},
			{Body: "c4", User: github.Author{Login: "nuage-autopilot", Type: "User"}, CreatedAt: now.Add(-4 * time.Hour)},
			{Body: "c3", User: github.Author{Login: "nuage-autopilot", Type: "User"}, CreatedAt: now.Add(-3 * time.Hour)},
			{Body: "c2", User: github.Author{Login: "nuage-autopilot", Type: "User"}, CreatedAt: now.Add(-2 * time.Hour)},
			{Body: longBody, User: github.Author{Login: "nuage-autopilot", Type: "User"}, CreatedAt: now.Add(-1 * time.Hour)},
		},
	}

	got := buildDispatchCandidates(items, comments, "nuage-autopilot", buildIssuePRLinks(items))
	if len(got) != 1 {
		t.Fatalf("len(got) = %d, want 1", len(got))
	}
	c := got[0]

	// Issue 本文が bodyPreviewLimit で rune 単位で切り詰められ、省略が分かる印が付く。
	if got, want := len([]rune(c.Body)), bodyPreviewLimit+len([]rune("…")); got != want {
		t.Fatalf("truncated body rune length = %d, want %d", got, want)
	}
	if !strings.HasSuffix(c.Body, "…") {
		t.Fatalf("c.Body = %q, want it to end with the truncation marker", c.Body)
	}

	if len(c.RecentComments) != recentCommentLimit {
		t.Fatalf("len(RecentComments) = %d, want %d (capped)", len(c.RecentComments), recentCommentLimit)
	}
	// 新しい順（"oldest" と "c9" が切り捨てられているはず）。
	for _, cm := range c.RecentComments {
		if cm.Preview == "oldest" || cm.Preview == "c9" {
			t.Fatalf("RecentComments contains %+v, want the 2 oldest comments dropped by the limit", cm)
		}
	}
	// 一番新しいコメントの本文が rune 単位で切り詰められている。
	if got, want := len([]rune(c.RecentComments[0].Preview)), commentPreviewLimit+len([]rune("…")); got != want {
		t.Fatalf("truncated preview rune length = %d, want %d", got, want)
	}
	if !c.RecentComments[0].IsBot {
		t.Fatalf("RecentComments[0].IsBot = false, want true (author matches botLogin)")
	}
}

func TestBuildDispatchCandidates_ShortBodyIsNotTruncated(t *testing.T) {
	items := []Item{
		{Kind: kindIssue, Number: 1, Title: "t", Author: "alice", Body: "short body"},
	}
	got := buildDispatchCandidates(items, nil, "nuage-autopilot", buildIssuePRLinks(items))
	if got[0].Body != "short body" {
		t.Fatalf("Body = %q, want unchanged short body", got[0].Body)
	}
}

func TestExtractRelatedIssueNumbers(t *testing.T) {
	tests := []struct {
		name string
		body string
		want []int
	}{
		{"closes", "Closes #10", []int{10}},
		{"close", "Close #11", []int{11}},
		{"closed", "Closed #12", []int{12}},
		{"fixes", "Fixes #20", []int{20}},
		{"fix", "Fix #21", []int{21}},
		{"fixed", "Fixed #22", []int{22}},
		{"resolves", "Resolves #30", []int{30}},
		{"resolve", "Resolve #31", []int{31}},
		{"resolved", "Resolved #32", []int{32}},
		{"multiple", "Fixes #40, Resolves #41", []int{40, 41}},
		{"none", "This PR fixes nothing", nil},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := extractRelatedIssueNumbers(tt.body)
			if len(got) != len(tt.want) {
				t.Fatalf("extractRelatedIssueNumbers(%q) = %v, want %v", tt.body, got, tt.want)
			}
			for i, v := range got {
				if v != tt.want[i] {
					t.Errorf("got[%d] = %d, want %d", i, v, tt.want[i])
				}
			}
		})
	}
}

func TestBuildIssuePRLinks_PreservesExcludedPRs(t *testing.T) {
	// 全 open アイテム（フィルタ前）
	allItems := []Item{
		{Kind: kindIssue, Number: 10, Title: "Issue 10"},
		{Kind: kindPullRequest, Number: 43, Title: "PR 43", Body: "Closes #10", Labels: []string{"agent:awaiting_user_review"}},
	}

	links := buildIssuePRLinks(allItems)
	if len(links[10]) != 1 || links[10][0] != 43 {
		t.Fatalf("buildIssuePRLinks() = %+v, want map[10:[43]]", links)
	}

	// 候補（PR 43 が agent:awaiting_user_review で除外された状態）
	candidates := []Item{
		{Kind: kindIssue, Number: 10, Title: "Issue 10"},
	}

	got := buildDispatchCandidates(candidates, nil, "bot", links)
	if len(got) != 1 {
		t.Fatalf("len(got) = %d, want 1", len(got))
	}
	if len(got[0].RelatedPRs) != 1 || got[0].RelatedPRs[0] != 43 {
		t.Fatalf("candidate RelatedPRs = %v, want [43]", got[0].RelatedPRs)
	}
}

func TestBuildDispatchPrompt_IncludesBodyAndComments(t *testing.T) {
	candidates := []DispatchCandidate{
		{
			Kind: kindIssue, Number: 1, Title: "issue one", Author: "alice",
			Body: "これは要件の本文である",
			RecentComments: []DispatchCommentSummary{
				{Author: "bob", IsBot: false, Preview: "承認します"},
			},
		},
	}
	prompt := buildDispatchPrompt("k-wa-wa/pechka", candidates)

	if !strings.Contains(prompt, "これは要件の本文である") {
		t.Fatalf("prompt does not contain the issue body: %q", prompt)
	}
	if !strings.Contains(prompt, "承認します") {
		t.Fatalf("prompt does not contain the comment preview: %q", prompt)
	}
	if !strings.Contains(prompt, "…") {
		t.Fatalf("prompt does not explain the truncation marker: %q", prompt)
	}
}

func TestBuildDispatchPrompt_EmptyBodyIsMarked(t *testing.T) {
	candidates := []DispatchCandidate{
		{Kind: kindIssue, Number: 1, Title: "issue one", Author: "alice"},
	}
	prompt := buildDispatchPrompt("k-wa-wa/pechka", candidates)

	if !strings.Contains(prompt, "本文: (無し)") {
		t.Fatalf("prompt does not mark an empty body: %q", prompt)
	}
}

func TestTruncateRunes(t *testing.T) {
	if got := truncateRunes("hello", 10); got != "hello" {
		t.Fatalf("truncateRunes(short) = %q, want unchanged", got)
	}
	if got := truncateRunes("こんにちは世界", 5); got != "こんにちは…" {
		t.Fatalf("truncateRunes(multibyte) = %q, want %q", got, "こんにちは…")
	}
}
