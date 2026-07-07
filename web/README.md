# web/ — ssh-gete ランディングページ

`ssh-gete` の紹介 LP（日本語・静的1ファイル）です。ビルド不要で、`index.html` をそのまま配信できます。

## 構成

```
web/
├── index.html                         # LP 本体（CSS/JS 埋め込み・単一ファイル）
├── downloads/
│   └── ssh-gete-macos-arm64.zip        # macOS (Apple Silicon) アプリ実体（同梱済み）
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

## ダウンロードボタンの状態

| OS | 状態 | リンク先 |
|---|---|---|
| macOS (arm64) | ✅ 実体同梱 | `downloads/ssh-gete-macos-arm64.zip` |
| Windows (x64) | ⏳ 未生成 | GitHub Releases（`index.html` の `#win-btn`） |

### なぜ Windows バイナリが未同梱か

このアプリは `mattn/go-sqlite3`（cgo）を使うため、Windows 版はクロスコンパイルに **mingw-w64** などのツールチェーンが要ります。macOS 上の素のビルド環境では生成できません。次のいずれかで用意します。

1. **GitHub Actions（推奨）**：Windows ランナー上で `wails build -platform windows/amd64` を実行し、成果物を Releases にアップロード。
2. **Windows 実機**：`wails build` で `ssh-gete.exe`（NSIS インストーラ）を生成。

### Windows バイナリを LP に差し込む手順

1. 生成した `ssh-gete-windows-amd64.zip`（または `.exe`）を `web/downloads/` に置く。
2. `index.html` の `#win-btn` を次のように差し替える。

```html
<a class="dlbtn" id="win-btn" href="downloads/ssh-gete-windows-amd64.zip" download>
  <svg ...>…</svg>
  ダウンロード
</a>
```

3. 直下のメタ表記（`x64 · GitHub Releases にて配布`）も「`x64 · .exe (zip)`」等へ更新。

## macOS zip の再生成

新しくビルドし直したら、次で zip を更新します（コード署名・リソースフォークを保つため `ditto` を使用）。

```bash
wails build
ditto -c -k --sequesterRsrc --keepParent \
  build/bin/ssh-gete.app web/downloads/ssh-gete-macos-arm64.zip
```

## 編集メモ

- 配色・コピーは `index.html` の `<style>` 冒頭の CSS 変数（`--brand` 等）とセクション本文を直接編集。
- スクリーンショットを入れるなら、ヒーローやターミナル節の擬似端末ブロックを実画像に差し替え可能。
- GitHub URL は現状 `https://github.com/yoshiyakato/ssh-mcp-server` を使用（公開リポジトリ名が変わったら一括置換）。
