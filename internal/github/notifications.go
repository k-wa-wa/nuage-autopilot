package github

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
)

// GetNotifications は GET /notifications を条件付きリクエストで呼び出す
// （DESIGN.md 7.2 節）。
//
// since が空でなければ「?since=<since>」をクエリに付与し、それ以降に更新された
// スレッドのみを対象にする。ifModifiedSince / ifNoneMatch が空でなければ、
// 対応するリクエストヘッダ（If-Modified-Since / If-None-Match）を付与する。
//
// サーバーが 304 を返した場合（前回の呼び出しから何も変化していない場合）、
// notModified=true を返す。この場合 rate limit は消費されない。
//
// 戻り値の lastModified / etag は、次回呼び出し時にそのまま ifModifiedSince /
// ifNoneMatch として渡すための値である（200 の場合のみ意味のある値が入る）。
func (c *Client) GetNotifications(ctx context.Context, since, ifModifiedSince, ifNoneMatch string) (threads []NotificationThread, lastModified, etag string, notModified bool, err error) {
	path := "/notifications?all=false&participating=false"
	if since != "" {
		path += "&since=" + url.QueryEscape(since)
	}

	extraHeaders := map[string]string{
		"If-Modified-Since": ifModifiedSince,
		"If-None-Match":     ifNoneMatch,
	}

	status, header, data, err := c.doRequest(ctx, "GET", c.baseURL+path, nil, extraHeaders)
	if err != nil {
		return nil, "", "", false, err
	}

	if status == http.StatusNotModified {
		return nil, "", "", true, nil
	}
	if status < 200 || status >= 300 {
		return nil, "", "", false, &APIError{Method: "GET", Path: path, StatusCode: status, Body: string(data)}
	}

	if err := json.Unmarshal(data, &threads); err != nil {
		return nil, "", "", false, fmt.Errorf("github: GET %s: decode response body: %w", path, err)
	}

	return threads, header.Get("Last-Modified"), header.Get("ETag"), false, nil
}

// SetRepositorySubscription は指定されたリポジトリに対する Notification Subscription (Watch 設定) を変更する。
func (c *Client) SetRepositorySubscription(ctx context.Context, repo string, subscribed bool) error {
	path := fmt.Sprintf("/repos/%s/subscription", repo)
	body := map[string]bool{"subscribed": subscribed}

	status, _, data, err := c.doRequest(ctx, "PUT", c.baseURL+path, body, nil)
	if err != nil {
		return err
	}

	if status < 200 || status >= 300 {
		return &APIError{Method: "PUT", Path: path, StatusCode: status, Body: string(data)}
	}

	return nil
}
