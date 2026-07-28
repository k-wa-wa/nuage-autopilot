// Package github は GitHub REST API のうち nuage-autopilot が必要とする最小限の操作
// （Issue/PR 一覧取得・ラベル操作・コメント操作・認証ユーザー取得）を提供する。
//
// DESIGN.md 5章の方針に従い、標準ライブラリの net/http のみに依存する。
// go-github 等の外部ライブラリは使わない（追加した場合は vendor/ の生成が必須になり、
// Phase 2 の時点ではその複雑さに見合う理由がないと判断した）。
package github

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"
)

// DefaultBaseURL は本番の GitHub REST API のベース URL である。
const DefaultBaseURL = "https://api.github.com"

// apiVersion は GitHub REST API のバージョンヘッダに渡す値である。
// 参照: https://docs.github.com/en/rest/about-the-rest-api/api-versions
const apiVersion = "2022-11-28"

// userAgent は GitHub API が必須とする User-Agent ヘッダの値である。
const userAgent = "nuage-autopilot"

// Client は GitHub REST API を叩く最小限のクライアントである。
type Client struct {
	baseURL    string
	tokenFunc  func() string
	httpClient *http.Client
}

// Option は Client の生成時に挙動を変更するための関数オプションである。
type Option func(*Client)

// WithBaseURL は API のベース URL を差し替える。既定値は DefaultBaseURL。
// 主にテスト（httptest.Server を指す）や GitHub Enterprise 対応のために用意する。
func WithBaseURL(baseURL string) Option {
	return func(c *Client) {
		if baseURL != "" {
			c.baseURL = baseURL
		}
	}
}

// WithHTTPClient は内部で使用する *http.Client を差し替える。
func WithHTTPClient(hc *http.Client) Option {
	return func(c *Client) {
		if hc != nil {
			c.httpClient = hc
		}
	}
}

// WithStaticToken は固定のトークンを使う。主にテスト用。
// 本番では既定の挙動（呼び出しごとに GH_TOKEN 環境変数を読む）を使う。
func WithStaticToken(token string) Option {
	return func(c *Client) {
		c.tokenFunc = func() string { return token }
	}
}

// NewClient は Client を生成する。
//
// token は Client の生成時に固定しない。リクエストのたびに GH_TOKEN 環境変数を
// 読み直す（既定の tokenFunc）。secrets.env が起動後に配置された場合でも、
// nuage-autopilot は常駐プロセスであるため、再起動せずに次の呼び出しから
// 認証が通るようにするためである（DESIGN.md 15章）。
//
// GH_TOKEN が空文字列の場合、Authorization ヘッダを付与せずリクエストする
// （未認証のレート制限が適用される）。
func NewClient(opts ...Option) *Client {
	c := &Client{
		baseURL:    DefaultBaseURL,
		tokenFunc:  func() string { return os.Getenv("GH_TOKEN") },
		httpClient: &http.Client{Timeout: 30 * time.Second},
	}
	for _, opt := range opts {
		opt(c)
	}
	return c
}

// APIError は GitHub API が 2xx 以外を返した場合のエラーである。
type APIError struct {
	Method     string
	Path       string
	StatusCode int
	Body       string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("github: %s %s: status %d: %s", e.Method, e.Path, e.StatusCode, e.Body)
}

// request は path（"/repos/..." のようにスラッシュから始まる相対パス）に対して
// method で HTTP リクエストを行う。body が非 nil の場合は JSON エンコードして送信し、
// out が非 nil の場合はレスポンスボディを JSON デコードして書き込む。
func (c *Client) request(ctx context.Context, method, path string, body, out any) error {
	_, err := c.requestWithHeader(ctx, method, c.baseURL+path, body, out)
	return err
}

// requestWithHeader は request と同じ動作をするが、レスポンスヘッダも返し、かつ
// url は（c.baseURL を前置しない）完全な URL を受け取る。
//
// ページネーションされたエンドポイント（ListComments 等）が、レスポンスの
// Link ヘッダに含まれる rel="last" の URL をそのまま次のリクエスト先として
// 使えるようにするために公開している。
func (c *Client) requestWithHeader(ctx context.Context, method, url string, body, out any) (http.Header, error) {
	status, header, data, err := c.doRequest(ctx, method, url, body, nil)
	if err != nil {
		return nil, err
	}

	if status < 200 || status >= 300 {
		return header, &APIError{Method: method, Path: url, StatusCode: status, Body: string(data)}
	}

	if out != nil && len(data) > 0 {
		if err := json.Unmarshal(data, out); err != nil {
			return header, fmt.Errorf("github: %s %s: decode response body: %w", method, url, err)
		}
	}

	return header, nil
}

// doRequest は HTTP リクエストを 1 回発行し、ステータスコード・レスポンスヘッダ・
// ボディをそのまま返す。requestWithHeader と異なり、非 2xx を自動的にはエラーに
// しない（呼び出し側がステータスコードごとに異なる扱いをしたい場合に使う。
// 例えば GetNotifications は 304 を「変化なし」という正常な結果として扱う必要が
// あり、APIError に変換されては困る）。
//
// err は transport レベルの失敗（DNS 解決失敗、接続エラー、ボディ読み取り失敗等）
// のみを表す。
func (c *Client) doRequest(ctx context.Context, method, url string, body any, extraHeaders map[string]string) (status int, header http.Header, data []byte, err error) {
	var reqBody io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return 0, nil, nil, fmt.Errorf("github: encode request body: %w", err)
		}
		reqBody = bytes.NewReader(b)
	}

	req, err := http.NewRequestWithContext(ctx, method, url, reqBody)
	if err != nil {
		return 0, nil, nil, fmt.Errorf("github: build request: %w", err)
	}

	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", apiVersion)
	req.Header.Set("User-Agent", userAgent)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if token := c.tokenFunc(); token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	for k, v := range extraHeaders {
		if v != "" {
			req.Header.Set(k, v)
		}
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return 0, nil, nil, fmt.Errorf("github: %s %s: %w", method, url, err)
	}
	defer resp.Body.Close()

	data, err = io.ReadAll(resp.Body)
	if err != nil {
		return 0, resp.Header, nil, fmt.Errorf("github: %s %s: read response body: %w", method, url, err)
	}

	return resp.StatusCode, resp.Header, data, nil
}
