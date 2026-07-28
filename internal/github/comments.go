package github

import (
	"context"
	"errors"
	"fmt"
	"sort"
)

// ListComments は repo の number（Issue/PR 番号）に付いたコメントを一覧取得する。
// isPR が true の場合は、Issue の会話コメントに加えて PR レビューコメント（GET /pulls/{n}/reviews）も
// 取得・統合し、時系列順（昇順）にソートして返す。
//
// TODO: 現在は先頭 1 ページ（最も古い 100 件）のみを取得している。
// コメントが 100 件を超えるアイテムでは最新のコメントが取得できず、
// ループ上限の判定（looplimit.go）と dispatcher の「直近コメント」が
// いずれも古い状態を見ることになる。完全な追従が必要になった場合は
// Link ヘッダの rel="last" を辿って最新ページを取得する必要がある。
func (c *Client) ListComments(ctx context.Context, repo string, number int, isPR bool) ([]Comment, error) {
	path := fmt.Sprintf("/repos/%s/issues/%d/comments?per_page=%d", repo, number, listPerPage)

	var raw []rawComment
	if err := c.request(ctx, "GET", path, nil, &raw); err != nil {
		return nil, fmt.Errorf("list comments for %s#%d: %w", repo, number, err)
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
