---
name: upload-github-image
description: GitHub の PR や Issue に埋め込むための画像ファイルを GitHub Contents API を経由してリポジトリにアップロードする手順。
---

# GitHub 画像アップロード手順

Agent が作成したスクリーンショット等の画像ファイルを GitHub 上にアップロードし、PR や Issue の Markdown に埋め込む際は以下の手順に従う。

## 適用場面
- PR や Issue に動作検証結果やデザインプレビューのスクリーンショットを添付する場面。
- 直接 Web UI からドラッグ＆ドロップアタッチメント API を使用できない環境。

## 手順

### 1. 画像の Base64 エンコード
対象の画像ファイルを Base64 エンコードする。

```bash
IMAGE_BASE64=$(base64 -w 0 path/to/image.png)
```

### 2. GitHub Contents API によるアップロード
`gh api` コマンドを使用し、リポジトリ内のアセット格納パス（例: `.github/assets/screenshots/`）に一意なファイル名（例: `filename_timestamp.png`）で画像をコミット・保存する。

```bash
gh api --method PUT /repos/{owner}/{repo}/contents/.github/assets/screenshots/filename.png \
  -f message="docs: add screenshot for PR" \
  -f content="$IMAGE_BASE64" \
  -f branch="<作業中ブランチ名>"
```

> [!IMPORTANT]
> - **ファイル名の重複防止**: 同一ファイル名で何度も更新すると GitHub Camo プロキシのキャッシュによって表示が遅延・破損する場合があるため、一意なファイル名（タイムスタンプ等を含む名）を使用するか、上書き時は `-f sha="$SHA"` を指定する。

### 3. PR / Issue Markdown への埋め込み
アップロード完了後、以下の形式で Markdown に埋め込む。

```markdown
![スクリーンショット](https://raw.githubusercontent.com/{owner}/{repo}/<作業中ブランチ名>/.github/assets/screenshots/filename.png)
```
