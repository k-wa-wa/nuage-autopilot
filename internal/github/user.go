package github

import "context"

// CurrentUser は GH_TOKEN の持ち主（認証ユーザー）の Login を返す。
//
// nuage-autopilot 自身が投稿したコメントを判定する際、GitHub App のような
// 「投稿者の type が Bot になる」仕組みに頼らず、認証ユーザーの Login と
// 一致するかどうかで判定する。専用の Personal Access Token アカウント
// （例: secrets.env.example の GIT_AUTHOR_NAME=nuage-autopilot）を使う運用では
// 投稿者の type は "User" のままになるため、type だけでは bot 判定できないからである。
func (c *Client) CurrentUser(ctx context.Context) (string, error) {
	var user struct {
		Login string `json:"login"`
	}
	if err := c.request(ctx, "GET", "/user", nil, &user); err != nil {
		return "", err
	}
	return user.Login, nil
}
