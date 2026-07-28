package github

import (
	"context"
	"fmt"
	"net/http"
)

// GetCheckState は対象コミット（sha または ref）の Check Runs 状態を集約して返す。
// GitHub Actions は Check Runs API 経由で結果を報告するため本判定で検出できる。
// （※外部 CI など Commit Statuses API を使うサービスは未参照であるが、現在対象の全リポジトリは GitHub Actions 運用のためこれで十分である）
// 返り値: "success", "failure", "pending", "none" (チェックランなし)
func (c *Client) GetCheckState(ctx context.Context, repo, ref string) (string, error) {
	if ref == "" {
		return "none", nil
	}

	path := fmt.Sprintf("/repos/%s/commits/%s/check-runs?per_page=%d", repo, ref, listPerPage)
	var resp CheckRunsResponse
	if err := c.request(ctx, http.MethodGet, path, nil, &resp); err != nil {
		return "", err
	}

	if resp.TotalCount == 0 {
		return "none", nil
	}

	hasPending := false
	hasFailure := false
	for _, cr := range resp.CheckRuns {
		if cr.Status != "completed" {
			hasPending = true
		} else if cr.Conclusion != "success" && cr.Conclusion != "neutral" && cr.Conclusion != "skipped" {
			hasFailure = true
		}
	}

	if hasFailure {
		return "failure", nil
	}
	if hasPending {
		return "pending", nil
	}
	return "success", nil
}
