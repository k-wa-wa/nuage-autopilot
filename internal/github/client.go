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
	token      string
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

// NewClient は token（GH_TOKEN の値）を使って Client を生成する。
// token が空文字列の場合、Authorization ヘッダを付与せずリクエストする
// （未認証のレート制限が適用される。GH_TOKEN 未設定は設定ミスとして呼び出し側で検知すべきだが、
// Client 自体はそれを強制しない）。
func NewClient(token string, opts ...Option) *Client {
	c := &Client{
		baseURL:    DefaultBaseURL,
		token:      token,
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
	var reqBody io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("github: encode request body: %w", err)
		}
		reqBody = bytes.NewReader(b)
	}

	req, err := http.NewRequestWithContext(ctx, method, url, reqBody)
	if err != nil {
		return nil, fmt.Errorf("github: build request: %w", err)
	}

	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", apiVersion)
	req.Header.Set("User-Agent", userAgent)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("github: %s %s: %w", method, url, err)
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("github: %s %s: read response body: %w", method, url, err)
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return resp.Header, &APIError{Method: method, Path: url, StatusCode: resp.StatusCode, Body: string(data)}
	}

	if out != nil && len(data) > 0 {
		if err := json.Unmarshal(data, out); err != nil {
			return resp.Header, fmt.Errorf("github: %s %s: decode response body: %w", method, url, err)
		}
	}

	return resp.Header, nil
}
