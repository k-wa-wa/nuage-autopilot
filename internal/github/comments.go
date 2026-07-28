package github

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"time"
)

// linkHeaderRe は RFC 8288 の Link ヘッダの 1 要素（"<url>; rel=\"name\""）を
// パースする。GitHub API のページネーションはこの形式で次・最終ページの URL を返す。
var linkHeaderRe = regexp.MustCompile(`<([^>]+)>;\s*rel="([^"]+)"`)

// lastPageURL は Link ヘッダから rel="last" の URL を抽出する。無ければ ok=false。
func lastPageURL(h http.Header) (string, bool) {
	for _, part := range strings.Split(h.Get("Link"), ",") {
		m := linkHeaderRe.FindStringSubmatch(strings.TrimSpace(part))
		if len(m) == 3 && m[2] == "last" {
			return m[1], true
		}
	}
	return "", false
}

// ListComments は repo の number（Issue/PR 番号）に付いたコメントを一覧取得する。
// isPR が true の場合は、Issue の会話コメントに加えて PR レビューコメント（GET /pulls/{n}/reviews）も
// 取得・統合し、時系列順（昇順）にソートして返す。
//
// コメントが 100 件（1 ページ）を超える場合、Link ヘッダの rel="last" を辿って
// 最新のページを追加で取得する。イベント取り込み・予算判定はいずれも「直近の
// コメント」を見て判断するため、古いページだけでは最新の状態を見誤る。
func (c *Client) ListComments(ctx context.Context, repo string, number int, isPR bool) ([]Comment, error) {
	path := fmt.Sprintf("/repos/%s/issues/%d/comments?per_page=%d", repo, number, listPerPage)

	var raw []rawComment
	header, err := c.requestWithHeader(ctx, "GET", c.baseURL+path, nil, &raw)
	if err != nil {
		return nil, fmt.Errorf("list comments for %s#%d: %w", repo, number, err)
	}
	if lastURL, ok := lastPageURL(header); ok {
		var lastPage []rawComment
		if _, err := c.requestWithHeader(ctx, "GET", lastURL, nil, &lastPage); err != nil {
			return nil, fmt.Errorf("list comments (last page) for %s#%d: %w", repo, number, err)
		}
		raw = lastPage
	}

	comments := make([]Comment, 0, len(raw))
	for _, r := range raw {
		comments = append(comments, r.toComment())
	}

	// PR の場合のみレビューコメントを取得する。
	if isPR {
		reviewsPath := fmt.Sprintf("/repos/%s/pulls/%d/reviews?per_page=%d", repo, number, listPerPage)
		var rawReviews []rawReview
		if err := c.request(ctx, "GET", reviewsPath, nil, &rawReviews); err == nil {
			for _, r := range rawReviews {
				if r.Body != "" {
					comments = append(comments, r.toComment())
				}
			}
		} else {
			// レビュー取得の失敗は、レビュー結果が観測できなくなり dispatcher の判断ミスにつながるため
			// 404 以外のエラーは無視せず呼び出し元へ報告する。
			var apiErr *APIError
			if !errors.As(err, &apiErr) || apiErr.StatusCode != 404 {
				return nil, fmt.Errorf("list reviews for %s#%d: %w", repo, number, err)
			}
		}
	}

	sort.Slice(comments, func(i, j int) bool {
		return comments[i].CreatedAt.Before(comments[j].CreatedAt)
	})

	return comments, nil
}

// ListCommentsSince は repo の number（Issue/PR 番号）について、since より新しい
// 会話コメント（および isPR の場合は PR レビュー）を取得する。
//
// ListComments と異なり、全履歴ではなく差分のみを対象とする（internal/ingest が
// 1 分間隔のポーリングごとに「前回確認した時刻以降の新着」だけを取り出すために使う。
// DESIGN.md 7.2 節）。会話コメントは GitHub API の `since` クエリパラメータで
// サーバー側フィルタするが、PR レビュー一覧の取得エンドポイントには `since` が
// 無いため、全件取得してクライアント側で SubmittedAt > since を判定する。
// レビューは通常件数が少ないため、この非対称な扱いによるコストは軽微である。
//
// 差分取得のみを想定しているため、ListComments と異なり Link ヘッダを辿った
// 追加ページの取得は行わない（1 ページ 100 件を超える新着は通常起こらない）。
func (c *Client) ListCommentsSince(ctx context.Context, repo string, number int, since time.Time, isPR bool) ([]Comment, error) {
	path := fmt.Sprintf("/repos/%s/issues/%d/comments?since=%s&per_page=%d",
		repo, number, since.UTC().Format(time.RFC3339), listPerPage)

	var raw []rawComment
	if err := c.request(ctx, "GET", path, nil, &raw); err != nil {
		return nil, fmt.Errorf("list comments since %s for %s#%d: %w", since, repo, number, err)
	}

	comments := make([]Comment, 0, len(raw))
	for _, r := range raw {
		comments = append(comments, r.toComment())
	}

	if isPR {
		reviewsPath := fmt.Sprintf("/repos/%s/pulls/%d/reviews?per_page=%d", repo, number, listPerPage)
		var rawReviews []rawReview
		if err := c.request(ctx, "GET", reviewsPath, nil, &rawReviews); err == nil {
			for _, r := range rawReviews {
				if r.Body != "" && r.SubmittedAt.After(since) {
					comments = append(comments, r.toComment())
				}
			}
		} else {
			var apiErr *APIError
			if !errors.As(err, &apiErr) || apiErr.StatusCode != 404 {
				return nil, fmt.Errorf("list reviews for %s#%d: %w", repo, number, err)
			}
		}
	}

	sort.Slice(comments, func(i, j int) bool {
		return comments[i].CreatedAt.Before(comments[j].CreatedAt)
	})

	return comments, nil
}

// CreateComment は repo の number に対して本文 body のコメントを投稿する。
func (c *Client) CreateComment(ctx context.Context, repo string, number int, body string) error {
	path := fmt.Sprintf("/repos/%s/issues/%d/comments", repo, number)
	reqBody := struct {
		Body string `json:"body"`
	}{Body: body}

	if err := c.request(ctx, "POST", path, reqBody, nil); err != nil {
		return fmt.Errorf("create comment on %s#%d: %w", repo, number, err)
	}
	return nil
}
