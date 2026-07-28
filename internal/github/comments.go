package github

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"sort"
	"strings"
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
// 最新のページを追加で取得する。ループ上限の判定（looplimit.go）と dispatcher /
// 状態行の解釈（internal/cycle/transition.go）はいずれも「直近のコメント」を見て
// 判断するため、古いページだけでは最新の状態を見誤る。
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
