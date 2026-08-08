// Package agentcli は LLM CLI ごとの差異を吸収し、engine に単一のインタフェースを
// 提供する（DESIGN.md 4章）。
//
// CLI は名前だけでなく、プロンプトの渡し方・無人実行のフラグ・セッション継続の方法・
// 応答 JSON の形がそれぞれ異なる。これらの知識を設定（環境変数）に持たせると、
// CLI を増やすたびに運用側が正しいフラグを覚える必要があり、間違えても起動するまで
// 気づけない。そこで **CLI 1 種類につき 1 つの Client 実装**を置き、engine は
// 「実行して結果を受け取る」ことだけを知る形にする。
//
// 新しい CLI を追加する場合はこのパッケージにファイルを 1 つ足し、New に登録する。
// engine・config・prompt のいずれも変更しない。
package agentcli

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
)

// Request は 1 回の実行要求である。
type Request struct {
	// WorkDir は CLI を起動する作業ディレクトリ（対象リポジトリの clone 先）である。
	WorkDir string

	// Prompt は CLI に渡す指示文である。標準入力で渡すか位置引数で渡すかは
	// Client の実装が決める。
	Prompt string

	// ResumeID は継続したいセッションの ID である。空の場合は新規セッションで起動する。
	// セッション継続に対応しない CLI では無視される。
	ResumeID string

	// Env はサブプロセスへ追加する環境変数（"KEY=VALUE" 形式）である。
	Env []string
}

// Result は 1 回の実行結果である。
type Result struct {
	// ExitCode はプロセスの終了コードである。0 以外でも Run のエラーとはしない
	// （「LLM がタスクに失敗した」ことと「CLI を実行できなかった」ことを区別する）。
	ExitCode int

	// SessionID は次回の継続に使う ID である。取得できなかった場合は空になる。
	SessionID string

	// CostUSD はこの実行の実測コストである。
	//
	// **コストを返さない CLI があるため nil を許す。** nil の場合、呼び出し側は
	// 金額を積算できない。予算の歯止めは実行回数のみになる（DESIGN.md 10章）。
	CostUSD *float64
}

// Client は LLM CLI 1 種類を表す。
type Client interface {
	// Name は CLI の識別名を返す（"claude" 等）。ログ用。
	Name() string

	// Run はプロンプトを渡して CLI を 1 回実行し、完了を待つ。
	// CLI 自体を起動できなかった場合にのみ error を返す。
	Run(ctx context.Context, req Request) (Result, error)
}

// Options は Client の生成オプションである。
type Options struct {
	// Model は CLI に指定するモデル名である。空の場合、CLI の既定モデルに任せる。
	Model string

	// Command は実行ファイル名/パスの上書きである。空の場合、各 CLI の既定名を使う。
	// テストでフェイクの実行系に差し替えるために公開している。
	Command string

	Logger *slog.Logger
}

// constructors は名前から Client を作る関数の一覧である。CLI を増やすときはここに足す。
var constructors = map[string]func(Options) Client{
	"claude": newClaude,
	"agy":    newAgy,
}

// DefaultName は CLI が指定されなかった場合に使う既定の CLI 名である。
const DefaultName = "claude"

// New は名前から Client を生成する。未知の名前はエラーとする。
//
// 既定値へ読み替えず起動時に落とすのは、綴り間違いで意図しない CLI が黙って使われる
// 事故を防ぐためである。
func New(name string, opts Options) (Client, error) {
	if name == "" {
		name = DefaultName
	}
	ctor, ok := constructors[name]
	if !ok {
		return nil, fmt.Errorf("agentcli: unknown cli %q (available: %v)", name, Names())
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	return ctor(opts), nil
}

// Names は利用できる CLI 名を辞書順で返す。エラーメッセージとドキュメント用。
func Names() []string {
	names := make([]string, 0, len(constructors))
	for name := range constructors {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}
