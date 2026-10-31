# web/ — ssh-gete ランディングページ

`ssh-gete` の紹介 LP（日本語・英語・静的1ファイル）です。ビルド不要で、`index.html` をそのまま配信できます。

## 構成

```
web/
├── index.html                         # LP 本体（CSS/JS 埋め込み・単一ファイル、JA/EN）
├── assets/                            # ロゴ・スクリーンショット・ケーブル画像
└── README.md
```

## ローカル確認

```bash
cd web
python3 -m http.server 8080
# → http://localhost:8080
```

> `file://` 直開きでも表示はできますが、ダウンロードや相対パスの挙動確認のためローカルサーバー経由を推奨します。

## 公開（GitHub Pages 例）

- リポジトリ Settings → Pages で `web/` を公開ディレクトリに指定、もしくは `gh-pages` ブランチへ `web/` の中身を配置。
- 独自ドメインを使う場合は `CNAME` を追加。

## ダウンロードボタンのリンク先

ボタンは GitHub Releases の最新版の成果物を直接指します（`https://github.com/yoshiyakato/ssh-gate-oss/releases/latest/download/<成果物名>`）。
成果物名は `.github/workflows/release.yml` の `matrix.artifact` と同じです。名前を変えたら `index.html` のリンクも直してください。

| 種類 | 成果物 |
|---|---|
| GUI 版 macOS（Universal） | `ssh-gate-mac-universal.zip` |
| GUI 版 Windows | `ssh-gate-win-x64.zip` / `ssh-gate-win-arm64.zip` |
| headless 版 Linux | `ssh-gate-linux-x64-headless.tar.gz` / `ssh-gate-linux-arm64-headless.tar.gz` |
| headless 版 macOS | `ssh-gate-mac-arm64-headless.zip` / `ssh-gate-mac-x64-headless.zip` |
| headless 版 Windows | `ssh-gate-win-x64-headless.zip` / `ssh-gate-win-arm64-headless.zip` |
| チェックサム | `SHA256SUMS.txt` |

`releases/latest` は「Latest」の印が付いたリリースを指します。これらの名前を持つリリース（v1.0.0 以降）が Latest になるまでは、リンクは 404 になります。

## 編集メモ

- 配色・コピーは `index.html` の `<style>` 冒頭の CSS 変数（`--brand` 等）とセクション本文を直接編集。
- スクリーンショットを入れるなら、ヒーローやターミナル節の擬似端末ブロックを実画像に差し替え可能。
- GitHub URL は `https://github.com/yoshiyakato/ssh-gate-oss` を使用（公開リポジトリ名が変わったら一括置換）。
