# ssh-gete

**AI エージェントの SSH 実行に、人間の承認をはさむゲートウェイ。**

`ssh-gete`（リポジトリ名 `ssh-gate-oss`）は、AI エージェント（Claude などの MCP クライアント）の SSH 越しのコマンド実行を **承認キューにためる**、ローカル完結のデスクトップアプリです。エージェントが `execute_command` を呼んでも、人間が GUI で承認するまで実行されません。Low リスクのコマンドでも自動実行しません（secure by default）。

SSH に加えて、**シリアルポート**の対話コンソールも内蔵しています（こちらは人間の直接操作専用）。

> `gete` は「ゲート（gate＝門番）」から。エージェントと実機の間に立つ承認関門です。

---

## なぜ必要か

一般的な MCP の SSH サーバーは、エージェントの指示をそのまま実行します。判断ミスやプロンプトインジェクション一発が、`rm -rf` や `systemctl stop` を本番で走らせ得ます。`ssh-gete` はその間に「人間が見て承認する画面」を1枚はさみます。

| | 素の SSH ツール | ssh-gete |
|---|---|---|
| エージェントの指示 | 即実行 | 承認キューに保留 |
| Low リスクのコマンド | 自動で実行 | それでも承認必須 |
| 誰が・何を・なぜ | 残りにくい | 全件 SQLite に記録 |
| 信頼できるエージェント | — | 承認省略を段階的に付与 |
| 通信範囲 | — | 既定 127.0.0.1 のみ |

---

## 主な機能

- **人間承認ゲートウェイ** — 要求は承認キューへ。GUI で確認し、必要ならコマンドを微修正して実行。
- **MCP サーバー内蔵** — JSON-RPC 2.0 over HTTP で `list_hosts` / `execute_command` / `request_command_execution` / `get_command_history` を提供。
- **リスク分類** — コマンドを High / Medium / Low に推定して表示（人間の判断補助）。
- **監査ログ** — 全要求・実行結果・所要時間を SQLite に記録。接続先を消しても履歴は残す。
- **ライブターミナル（SSH / シリアル）** — 承認フローとは別に、人間が直接操作する対話端末。接続先に種別（SSH/Serial）を持たせ、ターミナルはそれに従属する。
- **ローカル完結・最小保持** — 既定の待受は `127.0.0.1:8787`。SSH 鍵パスフレーズは保存せずメモリのみ。任意で Bearer 認証。

---

## ダウンロード

[GitHub Releases](../../releases) から各 OS のバイナリを入手できます。

- **macOS（Apple Silicon / Universal）**：`.app`（zip）。未署名のため初回は `.app` を右クリック →「開く」で許可。
- **Windows（x64）**：`.exe`（zip）。

紹介ページ（ランディングページ）は `web/`（`web/index.html`）にあります。

---

## ソースからビルド

必要環境：Go 1.25 以降、Node.js、[Wails v2](https://wails.io/)。

```bash
# Wails CLI（未導入なら）
go install github.com/wailsapp/wails/v2/cmd/wails@latest

# 開発（ホットリロード）
wails dev

# 配布ビルド（build/bin/ に生成）
wails build
```

SQLite ドライバはピュア Go の `modernc.org/sqlite` を使うため、cgo の C コンパイラは不要です。

```bash
go test ./...   # バックエンドのテスト
```

---

## エージェント（MCP クライアント）からの接続

アプリを起動し、「MCP待受設定」で待受を有効化します（既定 `http://127.0.0.1:8787/mcp`）。MCP クライアント側に、GUI が生成する `mcpServers` 設定（Bearer 有効時は `Authorization` ヘッダ付き）を貼り付けます。

提供ツール：

| ツール | 役割 |
|---|---|
| `list_hosts` | 登録済み接続先の一覧（`type` で SSH/Serial を判別） |
| `execute_command` | 実行要求を承認キューへ登録（シリアル接続先は実行不可） |
| `request_command_execution` | 同上 |
| `get_command_history` | 承認キューと履歴を取得 |

> シリアル接続先は対話 GUI 専用で、MCP からは実行できません。

---

## セキュリティ設計（要点）

- MCP 経由のコマンドはリスクに関係なく承認キュー行き（`AutoExecuteLowRisk` は UI から有効化不可）。
- 要求には `agent_name` 必須。承認省略は人間が明示付与したエージェントのみ。
- 出力上限・接続/コマンドのタイムアウト・SIGKILL。
- 既定はローカル待受。任意で Bearer 認証・ボディ上限・CORS 制限。
- 鍵パスフレーズは DB 非保存（メモリのみ）。トークンはマスク表示。
- ホスト鍵検証は既定オフ（`SSH_GETE_STRICT_HOSTKEY=1` で厳格化）。

詳細な設計・コード解説は [`docs/解説.md`](docs/%E8%A7%A3%E8%AA%AC.md) を参照してください。

---

## 開発元

[QuickIterate Co., Ltd.](https://quickiterate.com)

## ライセンス

[MIT](LICENSE)
