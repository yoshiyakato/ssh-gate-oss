# フェーズ6 設計：`--headless` 起動と Web 管理画面（REQ-015）

- 対象要件: `docs/spec/ssh-gate-spec.sqlite` の REQ-015（priority high / phase 6）
- 前提: v1 のフェーズ0〜4とキーチェーン（phase5-keychain.md）が入った 416cbf3
- 関連: design-v1.md §5.0（ロック規約と終了順序）・§5.3（MCP）、phase2-contract.md §1.4（イベント）、phase5-keychain.md §8（フォールバック）
- この文書は設計だけです。コードは含みません。

ユーザーの要望（REQ-015 の source）:

> 「linux / mac / windows 向けに --headless で起動できて web管理がめんで操作できる感じもできると とても使いやすい」
> 「設定ファイルを ベースに headless も構成するとすると 重たい？」

受け入れ条件（REQ-015）: `--headless` で GUI なしに起動し、ブラウザから承認・接続先編集・ターミナル・設定を操作できる。未認証の Web アクセスとクロスオリジン要求は拒否する。エージェントのトークンでは管理 API を呼べない。Linux headless ビルドが GTK なしで動く。

---

## 0. 現状（コードから確認した事実）

| 項目 | 事実 | 設計への影響 |
|---|---|---|
| `main.go` | `//go:embed all:frontend/dist` の `assets` と `wails.Run(...)`。`OnStartup: app.startup` / `OnShutdown: app.shutdown` / `Bind: app` | Wails を import しているのは `main.go` と `events.go` の2ファイルだけ |
| `events.go` | `emitEvent` は `a.emit`（テスト用フック）が nil でなければそれを呼び、なければ `a.ctx != nil` のとき `wruntime.EventsEmit(a.ctx, ...)` | `a.ctx` を使うのは `events.go` だけ（`app.go:534` で代入）。イベントは `terminal:data` / `terminal:exit` / `sessions:changed` の3種 |
| `*App` の公開メソッド | 46個。`frontend/wailsjs/go/main/App.js` の46関数と完全に一致 | 許可リストはこの46個（§3）。フェーズ7（REQ-016）の AI セッション8個を含む |
| 非公開のライフサイクル | `startup(ctx)`：シリアル監視と MCP 起動。`shutdown(ctx)`：§5.0 の順序（appCancel → cancelExecs → stopMCPServer → drainMCPCommands → CloseAllTerminals → wg.Wait → store.Close） | headless でもこの2つをそのまま呼ぶ |
| `NewApp()` | DB を開き、キーチェーン移行（`migrateKeychain(store, app.secrets)`）を行い、期限切れ掃除と secret GC の goroutine を起動する。`secrets: defaultSecretStore()` | 秘密情報の保存先は `NewApp` の**前に**決まっている必要がある（§7） |
| `SaveMCPSettings` | 保存の後に `restartMCPServer()`。`startMCPServerLocked` は既存サーバーの有無を見ない | startup の前に呼ぶと、startup が2回目の Listen をして失敗する。設定ファイルの反映には別の経路が要る（§1.5） |
| `httpguard.go` | `guardHTTP(settings, next)`：Host 検査（IP リテラルか localhost で、ポート一致。BaseURL の1ホストだけ追加で許す）、Origin の許可リスト、POST は `application/json` 必須、OPTIONS の処理 | Web 管理でもそのまま使い、外側に CSRF 層を足す（§6.4） |
| `frontend/src` が使う runtime | `Environment`（main.js、シリアルの OS 判定）と `EventsOn`（main.js：`sessions:changed`、terminal-tabs.js：`terminal:data` / `terminal:exit`） | ほかの runtime 関数は今は使われていない。それでも shim は runtime.js の全関数を用意する（§5.3） |
| ブラウザ API | `navigator.clipboard.writeText`（4か所）、`window.confirm`（5か所、承認の確認を含む）、`crypto.randomUUID`（タブ ID）、`localStorage` | いずれも**安全なコンテキスト**（https か 127.0.0.1 / localhost）が必要。LAN 公開で TLS を必須にする理由の1つ |
| frontend のポーリング | `setInterval(pollData, 5000)` で `GetInitialData` | 承認待ちの一覧はイベントではなくポーリングで更新される。SSE が切れても5秒で追いつく |
| `frontend/dist/index.html` | `<head>` にモジュールスクリプト1本と CSS 1本。インラインスクリプトなし | CSP で `script-src 'self'` にできる。shim は `<head>` の先頭に差し込む（§5.1） |
| cgo | `darwin_frameworks.go` は `darwin && cgo` のときだけ。`go.bug.st/serial` の本体は pure Go（cgo は `enumerator` サブパッケージだけで、使っていない）。SQLite は modernc。go-keyring は macOS で `security` コマンド、Linux で D-Bus（pure Go）、Windows で wincred（syscall） | `CGO_ENABLED=0 -tags headless` で全 OS をクロスビルドできる |
| 認証方式 | `AuthMethod` は `"SSH鍵認証"`（鍵ファイル。読めなければ ssh-agent）と `"パスワード保管"` | 設定ファイルでは `auth: key | password` に対応づける |
| データの場所 | `appConfigDir()`：`SSH_GETE_CONFIG_DIR` → `os.UserConfigDir()/ssh-gete` → `~/.ssh-gete` | `--data-dir` は `SSH_GETE_CONFIG_DIR` を設定するだけで済む |

並行作業の制約: 本体では別チームが `app.go`、`storage.go`、`terminal.go`、`frontend/src/main.js` などを修正している。このフェーズでは**既存ファイルの変更を次の4か所だけ**にする。

| 既存ファイル | 変更 | 行数の目安 |
|---|---|---|
| `main.go` | 先頭に `//go:build !headless`。`main()` の最初で `parseCLI` を呼び、`--headless` なら `os.Exit(runHeadless(...))` | +8 |
| `events.go` | `wruntime` の import を消し、`wruntime.EventsEmit(a.ctx, name, payload)` を `platformEmit(a.ctx, name, payload)` に置き換える | ±3 |
| `secret_store.go` | `defaultSecretStore()` の先頭に `if secretBackendFactory != nil { return secretBackendFactory() }` | +3 |
| `.github/workflows/release.yml`・`test.yml` | headless のジョブを追加（既存のジョブは変えない） | 追加だけ |

`app.go`・`storage.go`・`terminal.go`・`frontend/src/**` は変更しない。必要な処理は同じ package の新しいファイルから非公開のフィールドや関数を呼んで実現する。

---

## 1. 起動方法と設定ファイル

### 1.1 コマンドライン

GUI 版と headless 専用ビルドで同じフラグを受け付けます。

| フラグ | 意味 | 既定 |
|---|---|---|
| `--headless` | ウィンドウを出さず、MCP と Web 管理画面だけを動かす。headless 専用ビルドでは省略しても headless として動く | GUI 版では off |
| `--config <path>` | YAML の設定ファイル。環境変数 `SSH_GETE_CONFIG` でも指定できる。暗黙の探索はしない（どのファイルが効いているかを曖昧にしないため） | なし（DB だけで動く） |
| `--listen <addr>` | Web 管理画面の待受（`host:port`）。設定ファイルの `web.listen` より優先する | `127.0.0.1:8788` |
| `--data-dir <dir>` | DB・known_hosts・管理トークン・TLS 鍵・秘密情報ファイルを置く場所。内部では `SSH_GETE_CONFIG_DIR` を設定するだけ | `appConfigDir()` |
| `--tls-cert <path>` / `--tls-key <path>` | TLS の証明書と鍵（PEM） | なし |
| `--tls-self-signed` | 自己署名証明書を自動で作って使う（§6.6） | off |
| `--print-admin-token` | 管理トークンを標準出力に出して終了する（無ければ作る）。トークンを出すのはこのフラグを明示したときだけ | — |
| `--rotate-admin-token` | 管理トークンを作り直して終了する。動作中のプロセスがあれば、そのプロセスの全セッションも無効になる（§6.2） | — |
| `--check-config` | 設定ファイルを検証し、DB にどう反映されるか（作成・上書き・維持）を表示して終了する。DB には書かない | — |
| `--version` | 版を出して終了する | — |

規則:

- GUI 版（Wails あり）でフラグが1つも無ければ、今と同じく GUI で起動する。GUI 版は知らない引数を無視する（macOS が古い版で付ける `-psn_…` など）。`--headless` があるときだけ厳密に解析し、知らない引数があれば終了コード2で止める。
- 優先順位は「コマンドラインのフラグ > 環境変数（`SSH_GETE_*`）> 設定ファイル > DB > 既定値」とする。ただし DB と設定ファイルの関係は §1.4 の規則に従う。
- 終了コード: 0 正常、1 実行時エラー、2 設定・引数の誤り（起動拒否を含む）、3 同じデータディレクトリで別のプロセスが動作中。
- 単一インスタンス: headless はデータディレクトリの `ssh-gete.lock` を排他ロック（Unix は `flock`、Windows は `LockFileEx`）してから `NewApp` を呼ぶ。取れなければ終了コード3。GUI 版はロックを取らない（今の挙動を変えない）。GUI と headless を同時に動かすと MCP のポートも競合するので、起動時のログに注意を出す。
- headless では `SSH_GETE_NO_SEED=1` を設定してから `NewApp` を呼ぶ。サーバーにデモ用の接続先を作らないため。
- Windows の GUI 版は `windowsgui` サブシステムなので標準出力がない。`--headless` のときは `AttachConsole(ATTACH_PARENT_PROCESS)` で親のコンソールにつなぎ直す（`console_windows.go`）。常用には headless 専用ビルド（コンソールアプリ）を推奨する。
- Linux の GUI 版は GTK と WebKit を動的リンクするので、`--headless` を付けても GTK が無い環境では起動しない。そのために headless 専用ビルドを用意する（§2）。

起動時の標準出力（例。トークンそのものは出さない）:

```
ssh-gete 1.6.0 headless
data dir      : /var/lib/ssh-gete
config        : /etc/ssh-gete/config.yaml (3 created, 1 overridden, 5 kept)
secrets       : keychain (env bindings: 2)
MCP           : http://127.0.0.1:8787/mcp (bearer: on)
web admin     : https://192.0.2.10:8788/
TLS           : self-signed, SHA-256 3F:9A:…:C1
admin token   : /var/lib/ssh-gete/admin-token (mode 0600)  — 表示するには --print-admin-token
```

### 1.2 設定ファイルの形式

- YAML（version 1）。ライブラリは **`go.yaml.in/yaml/v3` v3.0.4**（YAML 組織が引き継いだ保守版。2025-06-29 公開）を推奨する。`gopkg.in/yaml.v3` v3.0.1（2022-05-27）は API が同じだが、アーカイブ済みのため避ける。どちらも公開から2週間以上たっている。
- `yaml.Decoder.KnownFields(true)` で知らないキーを誤りにする。とくに `password:` のような平文の秘密は**キーとして存在しない**ので、書くと起動を拒否する。
- 環境変数の展開（`${VAR}`）は、パスを表すキー（`*File`、`keyPath`、`tls.cert`、`tls.key`、`dataDir`）だけで行う。systemd の `${CREDENTIALS_DIRECTORY}` を使うため。ほかの値は展開しない。
- ファイルの権限: Unix で group か other に書き込み権があれば起動を拒否する（他人が接続先やポリシーを差し替えられるため）。読み取り権は警告だけにする（秘密を書けない形式なので）。

### 1.3 スキーマ

```yaml
version: 1

# 省略時は --data-dir / SSH_GETE_CONFIG_DIR / 既定の場所
dataDir: /var/lib/ssh-gete

# seed（既定）: 起動時の初期値。以後は Web の変更を優先（§1.4）
# enforce: 起動のたびに設定ファイルの内容で上書きする（GitOps 向け）
apply: seed

web:
  listen: 127.0.0.1:8788          # --listen が優先
  publicURL: ""                   # 例 https://gate.example.lan:8788。ホスト名でアクセスするとき、またはリバースプロキシの後ろに置くときに指定
  tls:
    mode: off                     # off | files | self-signed。127.0.0.1 以外では off は使えない
    cert: /etc/ssh-gete/tls.crt   # mode: files のとき
    key: /etc/ssh-gete/tls.key
  adminTokenFile: ""              # 省略時は <dataDir>/admin-token
  session:
    idleMinutes: 30               # 5〜720
    maxHours: 12                  # 1〜168
  maxSessions: 32

mcp:                              # 省略したキーは DB の値を使う
  enabled: true
  listenAddress: 127.0.0.1        # SSH_GETE_MCP_ADDR があればそちらが優先
  port: 8787
  bearerEnabled: true
  tokenEnv: SSH_GETE_TOKEN        # Bearer トークンを読む環境変数名。値そのものは書けない
  allowedOrigins: []
  requestTimeout: 30
  maxOutputKB: 256
  maxBodyKB: 256

policy:                           # MCPSettings のポリシー系フィールドに対応
  strictHostKey: true
  autoExecuteLowRisk: false
  requireApprovalSudo: true
  requireApprovalProd: true
  requireApprovalWriteOp: true
  defaultConnectTimeout: 10
  defaultCommandTimeout: 30

secrets:
  backend: keychain               # keychain（既定）| file | env | memory（§7）
  file: ""                        # backend: file のとき。省略時は <dataDir>/secrets.json

connections:
  - name: prod-web-1              # 照合のキー（§1.4）
    type: ssh                     # ssh | serial
    host: 10.0.0.5
    port: 22
    user: deploy
    auth: key                     # key（→ SSH鍵認証。鍵が読めなければ ssh-agent）| password（→ パスワード保管）
    keyPath: /etc/ssh-gete/keys/prod_ed25519
    passphraseEnv: ""             # 鍵のパスフレーズを読む環境変数
    passwordEnv: ""               # auth: password のとき。環境変数から読む
    passwordFile: ""              # または 0600 のファイルから読む（例 ${CREDENTIALS_DIRECTORY}/prod-web-1）
    tags: [prod, web]
    description: 本番 Web 1号機
    connectTimeout: 10
    commandTimeout: 30
    mcpFirstTrust: false
  - name: core-switch-console
    type: serial
    serialPort: /dev/ttyUSB0
    baudRate: 115200
    dataBits: 8
    parity: none
    stopBits: "1"

prune: false                      # true: 以前この設定ファイルが作り、今は書かれていない接続先を DB から消す
```

検証の規則（`validate()`）: `version` は 1 だけ。`connections[].name` は重複しない。`passwordEnv` と `passwordFile` は同時に書けない。`auth: password` で両方とも無いのは許す（Web 画面で後から入れる）。`web.listen` と `mcp` の待受が同じアドレスとポートなら誤り。`apply` は `seed` か `enforce`。シリアルの値は既存の `validateSerialSettings(serialGOOS, ...)` で検査する。

設定ファイルで扱わないもの（v1）: エージェントの承認バイパスとエージェント用トークン。バイパスを設定ファイルから与えられると、ファイルを書き換えられる人が承認を素通りさせられるため。これらは今までどおり画面からだけ操作する。

### 1.4 設定ファイルと DB が食い違ったときの規則

「設定ファイルは起動時の初期値。以後は Web 画面からの変更を DB に保存する」を、次の3通りの比較で実現します。

新しい表 `config_applied`（`config_apply.go` が `CREATE TABLE IF NOT EXISTS` で作る。`user_version` は上げない。`storage.go` と衝突させないため）:

```sql
CREATE TABLE IF NOT EXISTS config_applied (
  kind       TEXT NOT NULL,   -- 'connection' | 'mcp' | 'policy'
  key        TEXT NOT NULL,   -- 接続先は設定ファイルの name、mcp/policy は固定文字列
  conn_id    TEXT NOT NULL DEFAULT '',  -- 接続先のとき、作成・更新した行の ID
  hash       TEXT NOT NULL,   -- 設定ファイルの項目を正規化した JSON の sha256（書かれたキーだけ）
  applied_at TEXT NOT NULL,
  PRIMARY KEY (kind, key)
);
```

起動のたびに、設定ファイルの項目ごとに次を判定します（`apply: seed` のとき）。

| DB の状態 | 前回反映したハッシュ | 判定 | 動作 |
|---|---|---|---|
| 対応する行が無い | — | 新規 | 作る。ハッシュを記録する |
| 行がある | 今のハッシュと同じ | 設定ファイルは変わっていない | **DB を優先**（Web の変更を残す）。何もしない |
| 行がある | 違う、または記録が無い | 設定ファイルが変わった（または初めて管理下に入る） | **設定ファイルを優先**。設定ファイルに**書かれているキーだけ**上書きし、書かれていないキーは DB の値を残す。ハッシュを記録し、監査ログに `config_override` と上書きしたフィールド名を残す |

- 接続先の照合は、まず `config_applied.conn_id`、次に名前で行う。Web で名前を変えても、同じ項目として扱える（重複して作らない）。
- 設定ファイルから消えた項目は、既定では DB に残す（起動ログに出す）。`prune: true` のときだけ、`config_applied` に記録があるもの（＝設定ファイルが作ったもの）を削除する。Web で作った接続先は消さない。
- `apply: enforce` では、ハッシュが同じでも毎回設定ファイルの値で上書きする。Web で変えた値は次の起動で戻る（起動ログと監査ログに出す）。
- 秘密情報は DB にも `config_applied` にも入れない。環境変数やファイルとの対応（§7.3）は起動のたびにメモリ上で作り直す。
- `mcp` の待受は、環境変数 `SSH_GETE_MCP_ADDR` があればそちらを優先し、設定ファイルからは反映しない（`applyMCPEnv` の今の挙動を保つ）。トークンは `tokenEnv` が `SSH_GETE_TOKEN` 以外の名前なら、その値を `SSH_GETE_TOKEN` に入れてから `NewApp` を呼ぶ（`mcpEnvToken` の既存の仕組みに乗せる）。
- `web:` の設定は DB に入れない。設定ファイルとフラグだけで決まる。Web 画面から管理画面自身の待受を変えて締め出される事故を避けるため。
- `--check-config` は同じ判定をして、表（作成・上書き・維持・削除）を出すだけで DB には書かない。

### 1.5 反映の順序（`runHeadless` の中）

1. フラグの解析、設定ファイルの読み込みと検証（誤りなら終了コード2）
2. `SSH_GETE_CONFIG_DIR`、`SSH_GETE_NO_SEED`、`SSH_GETE_TOKEN` を設定する
3. 単一インスタンスのロック
4. 秘密情報の保存先を決めて `secretBackendFactory` に入れる（§7）。`NewApp` の中のキーチェーン移行が正しい保存先に向くよう、`NewApp` より前に行う
5. `app := NewApp()`
6. `applyConfig(app, cfg, envSecrets)`：接続先は `SaveConnection` を呼ぶ（検証・秘密の保存・競合判定を既存のコードに任せる）。MCP とポリシーは、`SaveMCPSettings` から再起動を除いた内部の手順（`sanitizeMCPSettings` → `validateListenSettings` → `store.saveMCPSettings` → `a.mcp` の更新）を `config_apply.go` で行う。startup の前なので MCP サーバーはまだ動いていない。この手順が `SaveMCPSettings` と同じ結果になることをテストで確かめる（§10）
7. 管理トークンを用意する（§6.2）。TLS を用意する（§6.6）。待受の安全条件を検査する（§6.1）。満たさなければ終了コード2
8. `app.startup(withWebHub(ctx, hub))`：シリアル監視と MCP の起動
9. Web 管理サーバーの Listen
10. シグナルを待つ（§8）

---

## 2. ビルド

### 2.1 2種類のビルド

| ビルド | コマンド | Wails | cgo | 用途 |
|---|---|---|---|---|
| GUI 版（今と同じ） | `wails build ...` | あり | macOS は使う | デスクトップ。`--headless` も受け付ける |
| headless 専用 | `CGO_ENABLED=0 go build -tags headless -trimpath -o ssh-gete .` | **import しない** | なし | Linux サーバー、コンテナ、macOS / Windows の常駐 |

headless 専用ビルドの対象: linux/amd64、linux/arm64、darwin/amd64、darwin/arm64、windows/amd64、windows/arm64。すべて1台の Linux からクロスビルドできる。

### 2.2 ファイルの分け方

```
main.go              //go:build !headless   既存。Wails の main。先頭で parseCLI → --headless なら runHeadless
main_headless.go     //go:build headless    新規。//go:embed all:frontend/dist の assets と main()
cli.go               （タグなし）           新規。parseCLI, CLIOptions
headless_run.go      （タグなし）           新規。runHeadless：GUI 版の --headless でも使う
events.go            （タグなし）           既存。wruntime の import を消し、platformEmit を呼ぶ
events_platform.go   （タグなし）           新規。platformEmit, withWebHub, webHubFrom
events_wails.go      //go:build !headless   新規。wailsEmit = wruntime.EventsEmit
events_headless.go   //go:build headless    新規。wailsEmit は何もしない
console_windows.go   //go:build windows && !headless   新規。attachParentConsole
console_other.go     //go:build !windows || headless   新規。attachParentConsole は何もしない
instance_lock_unix.go / instance_lock_windows.go       新規
darwin_frameworks.go //go:build darwin && cgo          既存のまま（headless 専用ビルドは CGO_ENABLED=0 なので入らない）
```

- `assets` 変数は `main.go`（!headless）と `main_headless.go`（headless）の両方で定義する。タグが排他なので二重定義にならず、同じファイル群を2回 embed することもない。`runHeadless` は `fs.Sub(assets, "frontend/dist")` を引数で受け取る。
- イベント: `emitEvent` は今までどおり、`a.emit`（テスト用フック）が nil で `a.ctx` が nil でなければ `platformEmit(a.ctx, …)` を呼ぶ。`platformEmit` は `ctx` に Web のイベントハブが入っていればハブに渡し、入っていなければ `wailsEmit`（GUI 版は Wails、headless 専用ビルドは何もしない）を呼ぶ。headless では `app.startup(withWebHub(ctx, hub))` とするので、`app.go` を変えずにイベントがハブへ流れる。
  - `a.emit` を本番で使う案は採らない。`NewApp` が起動する goroutine と、`NewApp` の後に書く `a.emit` の間でデータ競合になりうるため。`a.ctx` は今も `startup` で書かれているので、競合の条件は今と変わらない。
- 規約（統合時に CLAUDE.md / design-v1 に追記を提案）: **Wails の runtime を呼んでよいのは `main.go` と `*_wails.go` だけ**。今後 `OnBeforeClose` のダイアログなどを足すときも `*_wails.go` に置き、headless では何もしない実装を `*_headless.go` に置く。Wails の runtime 関数は Wails 以外の ctx を渡すと `log.Fatalf` で止まるため、GUI 版の `--headless` でも Wails の runtime を呼ばないことが重要。
- 依存の検査: `go list -deps -tags headless .` の結果に `github.com/wailsapp/` が1つも無いことを CI とテスト（`headless_build_test.go`、headless タグ付きのときだけ）で確かめる。
- `frontend/dist` は embed の対象なので、headless 専用ビルドでもビルドの前に必要。`frontend/wailsjs` は git 管理外の生成物で、生成に Wails CLI（Linux では WebKit の開発用ライブラリ）が要る。そこで release では、macOS のジョブで作った `frontend/dist` を artifact として受け渡し、Linux のジョブで全 OS の headless をクロスビルドする（§9.3）。

---

## 3. Web の呼び出し層：`POST /api/call/<Method>`

### 3.1 形

```
POST /api/call/SaveConnection
Content-Type: application/json
X-SSH-Gete-CSRF: 1
Cookie: <セッション>

[ {"id":"", "name":"prod-web-1", ...} ]
```

| 結果 | HTTP | 本文 |
|---|---|---|
| 成功 | 200 | `{"result": <戻り値>}`（戻り値が無いメソッドは `{"result": null}`） |
| Go の `error` が返った（`GetHostKeyInfo` の第2戻り値） | 200 | `{"error": "<err.Error()>"}`。shim は Wails と同じく、この文字列で Promise を reject する |
| 許可リストにないメソッド | 404 | `{"error":"unknown method"}`（存在するかどうかを区別しない） |
| 引数の数が合わない・型が合わない | 400 | `{"error":"received N arguments to method 'X', expected M"}` など（Wails と同じ文言） |
| 本文が大きすぎる | 413 | 既定 1 MiB（`SendTerminalInput` の貼り付けに足りる大きさ） |
| 未ログイン・期限切れ | 401 | `{"error":"login required"}`。shim は `/login?next=…` に移る |
| 終了処理中 | 503 | `{"error":"shutting down"}` |
| パニック | 500 | `{"error":"internal error"}`。スタックはログだけに出す |

### 3.2 実装（`web_rpc.go`）

- `reflect.ValueOf(app).MethodByName(name)` を、**許可リスト `webCallable` にある名前だけ**で呼ぶ。許可リストは起動時に `reflect.Type` と突き合わせ、存在しない名前があればパニックせず起動を拒否する（取り違えの早期発見）。
- 引数: 本文を `[]json.RawMessage` に読み、数が `NumIn()` と違えば 400。各引数は `reflect.New(paramType)` に `json.Unmarshal` する。Wails も `encoding/json` で同じように変換するので、`Connection` や `MCPSettings` は `frontend/wailsjs/go/models.ts` と同じ JSON の形になる（json タグがそのまま効く）。JS の `undefined` は `JSON.stringify` で `null` になり、ゼロ値になる（Wails と同じ）。
- 戻り値: 1個ならそれを `result` に、2個で最後が `error` なら §3.1 のとおり。`nil` のスライスは `null` になる（Wails と同じ。frontend はすでにこれを扱っている）。
- 応答には `Cache-Control: no-store` を付ける（`GenerateMCPToken` と `IssueAgentToken` はトークンを1回だけ返すため）。
- 同時実行: Wails も呼び出しごとに goroutine で動かすので、HTTP のハンドラーが並行に呼んでも前提は変わらない。1セッションあたり同時に 64 件まで（超えたら 429）。`SendTerminalInput` の順序は、frontend の `sendQueue` が1件ずつ await するので保たれる。
- App のメソッドには ctx が無い。ブラウザが切断しても呼び出しは最後まで走る（Wails と同じ）。
- 呼び出しの前後で監査ログに記録する（§6.9）。

### 3.3 許可リスト（`frontend/wailsjs/go/main/App.js` の46個 = `*App` の公開メソッドの全部）

| メソッド | 許可 | 監査 | 監査に残す引数 | 備考 |
|---|---|---|---|---|
| `GetInitialData` | ○ | — | — | 5秒ごとのポーリング |
| `GetMCPSettings` | ○ | — | — | |
| `ListAgentApprovalBypasses` / `ListAgentTokens` | ○ | — | — | |
| `ListTerminalSessions` / `ListSerialPorts` | ○ | — | — | シリアルは**サーバー側**のポート |
| `GetApprovalTarget(id)` | ○ | — | — | |
| `GetHostKeyInfo(connID)` | ○ | — | — | `(HostKeyInfo, error)` |
| `PlaintextBackups()` | ○ | — | — | サーバー上のパスを返す |
| `ApproveCommand(id, command)` | ○ | ○ | id、command の sha256 と長さ | 確認ダイアログは既存の UI のまま（`risk_increased` → `confirm` → `ApproveCommandConfirmed`） |
| `ApproveCommandConfirmed(id, command)` | ○ | ○ | 同上 | |
| `ApproveCommandAndBypassAgent(id, command)` | ○ | ○ | 同上 | バイパス付与の `confirm` も既存の UI のまま |
| `RejectCommand(id)` / `DeleteCommandRequest(id)` | ○ | ○ | id | |
| `SaveConnection(conn)` | ○ | ○ | id、name、type、host、変更したフィールド名 | `credential`（パスワード時）と `keyPassphrase` は残さない |
| `DeleteConnection(id)` / `DisconnectConnection(id)` | ○ | ○ | id | |
| `TestConnection(id)` | ○ | ○ | id | |
| `TestConnectionWithPassphrase(id, passphrase)` | ○ | ○ | id のみ | |
| `TestConnectionWithPassphraseSave(id, passphrase, save)` | ○ | ○ | id、save | |
| `TestConnectionConfig(conn, passphrase)` | ○ | ○ | name、host のみ | |
| `UseCredentialForSession(id, secret)` | ○ | ○ | id のみ | |
| `ResetHostKey(connID)` | ○ | ○ | connID | |
| `SaveMCPSettings(settings)` | ○ | ○ | 変更したフィールド名（`tokenInput` の値は残さない） | MCP の待受を LAN にするときの Bearer 必須は既存の `validateListenSettings` が守る |
| `GenerateMCPToken()` / `DeleteMCPSettings()` | ○ | ○ | — | 戻り値のトークンは監査にもログにも残さない |
| `IssueAgentToken(name)` / `RevokeAgentToken(name)` | ○ | ○ | name | 同上 |
| `IssueAgentTokenWithBypass(name, bypass)` | ○ | ○ | name（detail: bypass） | 同上。本体の監査修正で追加。管理者だけが呼べる操作なので許可する |
| `RevokeAgentApprovalBypass(name)` / `RevokeAllAgentApprovalBypasses()` | ○ | ○ | name | |
| `DeletePlaintextBackups(paths)` | ○ | ○ | paths の件数 | 既存の実装が「今 `PlaintextBackups` が返すパスだけ」を消すので、任意のファイルは消せない |
| `StartTerminal(id, connID, cols, rows)` | ○ | ○ | id、connID | |
| `CloseTerminal(id)` / `CloseTerminalsByConnection(connID)` / `CloseAllTerminals()` | ○ | ○ | id / connID | 閉じるときに、そのタブで送った入力のバイト数の合計も残す |
| `SendTerminalInput(id, data)` | ○ | 個別には残さない | — | 量が多い。バイト数だけ数えて Close の監査に含める |
| `ResizeTerminal(id, cols, rows)` | ○ | — | — | |
| `ListAISessions()` / `GetAISession(id)` / `ReadAISessionRaw(id, since)` / `GetAISessionLineContext(requestId)` / `GetAISessionSettings()` | ○ | — | — | AI セッション（REQ-016）の一覧と読み取り専用の閲覧。人が AI セッションに入力するメソッドは GUI にも Web にも無い |
| `CloseAISession(id)` / `CloseAllAISessions()` | ○ | ○ | id | 強制終了（`human_closed`）。AI セッション側の `closed_by` は GUI と同じ `gui` になり、誰が閉じたかは Web の監査に残る |
| `SaveAISessionSettings(settings)` | ○ | ○ | 変更したフィールド名 | 無効にすると開いている AI セッションをすべて閉じる |

許可しないもの（非公開なのでリフレクションでも呼べないが、表として明記する）:

| メソッド | 理由 |
|---|---|
| `startup` / `shutdown` / `drainMCPCommands` | ライフサイクル。headless のプロセスだけが呼ぶ |
| `startMCPServer` / `stopMCPServer` / `restartMCPServer` | 設定保存の経路（`SaveMCPSettings`）以外から動かさない |
| `authenticateMCP` / `lookupAgentToken` / `effectiveTokenLocked` | 認証の内部 |
| `saveConnection` / `testConnectionConfig` / `setAgentApprovalBypass*` / `saveAgentPolicy` / `cancelExecs` / `trackExec` / `beginExec` など | 公開メソッドの内部手順。ロック規約（§5.0）を前提にしている |
| `emitEvent` / `sessionsChanged` / `notifySessionsChanged` | イベントの送出 |

将来の公開メソッドへの備え: テスト `TestWebCallableCoversExportedMethods` で、`*App` の公開メソッドがすべて「許可」か「拒否（`webDenied` に理由つき）」のどちらかに分類されていることを確かめる。別チームが公開メソッドを足すと、このテストが落ちて分類を求める（既定で公開しない）。`frontend/wailsjs/go/main/App.js` があれば、その関数名の集合と許可リストが一致することも確かめる（無ければ skip）。

管理者の権限について: Web 管理にログインした人は、GUI を操作する人と同じ権限を持つ（サーバー上の鍵ファイルのパスを指定した接続テスト、サーバー側のシリアルポートの使用などを含む）。管理トークンはサーバーの OS ユーザーと同じ重さで扱う、と README に書く。

---

## 4. イベント層：`GET /api/events`（SSE）

### 4.1 配り方

- `platformEmit` → `eventHub.publish(name, payload)`。publish は**ブロックしない**。payload を1回だけ JSON にし、ハブのリングバッファに積み、接続中の全クライアントの送信キューに非ブロッキングで入れる。ハブの mutex は葉のロックで、保持中に他のロックを取らず、App にもコールバックしない（§5.0。`terminal:data` は出力バッファの送出 goroutine から呼ばれる）。
- 対象のイベント: 名前を問わず全部（今は `terminal:data`、`terminal:exit`、`sessions:changed`）。将来のイベントもそのまま流れる。
- 送る形（1イベント）:

```
id: <epoch>-<seq>
data: {"n":"terminal:data","d":{"id":"TERM-…","data":"<base64>"},"o":123456}

```

  - `epoch` はプロセスの起動ごとに変わる乱数、`seq` はハブ全体の通し番号。
  - `o` は `terminal:data` だけに付ける。そのターミナルについてサーバーが送った累計バイト数（`data` を復号した長さと `dropped` の合計）。再接続のときの欠落の検出に使う（§4.3）。
- 15秒ごとに `: ping` のコメント行を送り、プロキシやブラウザに接続を切らせない。
- 応答ヘッダ: `Content-Type: text/event-stream`、`Cache-Control: no-store`、`X-Accel-Buffering: no`。
- shim は `EventSource` ではなく **`fetch` の ReadableStream** で受ける。`EventSource` はカスタムヘッダを付けられず、CSRF の規則（§6.4）を全 API で同じにできないため。`Last-Event-ID` も fetch のヘッダで送る。

### 4.2 バックプレッシャー

| 項目 | 値 | 動作 |
|---|---|---|
| クライアントごとの送信キュー | 1024 件または 4 MiB（先に達した方） | 溢れたらそのクライアントを**切る**（キューを捨てて stream を閉じる）。他のクライアントやターミナルには影響させない |
| 書き込みの期限 | 1回の Write とフラッシュで 10 秒 | 超えたら切る（`http.ResponseController.SetWriteDeadline`） |
| リングバッファ（再接続用） | 4096 件または 8 MiB | 古い順に捨てる |
| 同時接続 | 1セッションあたり 8、全体で 64 | 超えたら一番古い stream を閉じる |

元の `terminal:data` の間引き（16ms / 32 KiB ごと、未送信 4 MiB を超えたら古い側を捨てて `dropped` に足す。phase2-contract §1.4）は Go 側で今までどおり効く。Web 層はその後ろで、遅いブラウザだけを切る。

### 4.3 再接続

1. shim は切断を検知したら、0.5 秒から倍々で最大 10 秒まで待って再接続する。そのとき最後に受け取った `id` を `Last-Event-ID` で送る。
2. サーバーは `epoch` が同じで、その `seq` の次がリングに残っていれば、そこから**再送**する。
3. 残っていない（`epoch` が違う＝再起動した、または古すぎる）ときは、まず `headless:resync` を1件送る。中身は次のとおり。
   - `sessions`：`a.sessionsSnapshot()` の結果（`sessionsSeq` は新しいので、frontend はそのまま採用する）
   - `offsets`：生きているターミナルごとの現在の累計バイト数
   - `exits`：直近10分・最大256件の `terminal:exit` の payload（ハブが覚えておく）
4. shim は `headless:resync` を受けて、次の順に**合成イベント**を自分の購読者に配る。frontend のコードは変えない。
   - ターミナルごとに、自分が知っている `o` と `offsets` の差があれば `terminal:data {id, data:"", dropped:差}`（frontend は「N バイトを省略しました」と表示する。terminal-tabs.js 466行）
   - 自分が知っていて `exits` にある id について `terminal:exit`
   - `sessions:changed`（`sessions` の中身）
5. 承認待ちなど要求の一覧は、もともと `GetInitialData` の5秒ポーリングで追いつく。
6. 401 が返ったら `/login?next=<今のパス>` に移る。

既知の制約: 平文の HTTP（127.0.0.1）では、ブラウザが同じホストとポートへの HTTP/1.1 の同時接続を6本に制限する。タブ1枚で SSE が1本を使うので、6枚以上のタブを開くと呼び出しが詰まる。TLS のときは Go の `net/http` が HTTP/2 を使うので問題にならない。v1 は README に「平文のときはタブを5枚まで」と書き、`BroadcastChannel` で1本の SSE を共有する案は後回しにする。

---

## 5. フロントの差し替え（frontend のソースは変えない）

### 5.1 index.html への差し込み

- Web サーバーが `/`（と `/index.html`）を返すとき、embed した `frontend/dist/index.html` の `<head>` の直後に `<script src="/headless-shim.js"></script>` を1行差し込む。差し込みは起動時に1回だけ行い、結果を保持する。`<head>` が見つからなければ起動を拒否する（ビルドの取り違えを早く見つける）。
- shim は classic のスクリプト（`type="module"` を付けない、`defer` も付けない）なので、パーサーがその場で同期実行する。Vite のバンドルは `type="module"` で遅延実行されるので、`window.go` と `window.runtime` は必ず先にできている。
- インラインのスクリプトは使わない。CSP は `script-src 'self'` のまま（§6.8）。
- shim・ログイン画面のファイルは `webadmin/` に置き、`web_embed.go` の `//go:embed webadmin` で埋め込む。`frontend/dist` には入れない（GUI 版の成果物を変えない）。

### 5.2 `window.go.main.App`

```js
window.go = { main: { App: new Proxy({}, { get: (_, name) =>
  typeof name === 'string' ? (...args) => call(name, args) : undefined }) } };
```

- `call(name, args)` は `fetch('/api/call/' + encodeURIComponent(name), {method:'POST', credentials:'same-origin', headers:{'Content-Type':'application/json','X-SSH-Gete-CSRF':'1'}, body: JSON.stringify(args)})`。
- 200 で `result` があれば resolve、`error` があればその**文字列**で reject（Wails v2 と同じ）。401 ならログイン画面へ。それ以外は `HTTP <status>: <error>` の文字列で reject。
- `then` という名前は `undefined` を返す（Proxy が thenable と誤認されないように）。

### 5.3 `window.runtime`（runtime.js が呼ぶ関数の全部）

| runtime.js の関数 | `window.runtime` で呼ぶ名前 | headless での実装 |
|---|---|---|
| `LogPrint` / `LogTrace` / `LogDebug` / `LogInfo` / `LogWarning` / `LogError` / `LogFatal` | 同名 | `console.log/debug/info/warn/error`。サーバーには送らない |
| `EventsOnMultiple(name, cb, max)` | `EventsOnMultiple` | ローカルの購読表に登録し、解除関数を返す。`max` が -1 なら無制限、正の数なら呼んだ回数で減らして0で外す（Wails の events.js と同じ） |
| `EventsOn(name, cb)` / `EventsOnce(name, cb)` | runtime.js の中で `EventsOnMultiple` を呼ぶ | 上と同じ。shim も `EventsOn` / `EventsOnce` を直接用意する |
| `EventsOff(name, ...more)` | `EventsOff` | その名前の購読をすべて外す |
| `EventsOffAll()` | `EventsOffAll` | 全部外す |
| `EventsEmit(name, ...data)` | `EventsEmit` | ローカルの購読者だけに配る（Go 側に受け手はいない）。サーバーには送らない |
| `WindowReload` / `WindowReloadApp` | 同名 | `location.reload()` |
| `WindowSetTitle(title)` | 同名 | `document.title = title` |
| `WindowSetAlwaysOnTop` / `WindowSetSystemDefaultTheme` / `WindowSetLightTheme` / `WindowSetDarkTheme` / `WindowCenter` / `WindowFullscreen` / `WindowUnfullscreen` / `WindowSetSize` / `WindowSetMaxSize` / `WindowSetMinSize` / `WindowSetPosition` / `WindowHide` / `WindowShow` / `WindowMaximise` / `WindowToggleMaximise` / `WindowUnmaximise` / `WindowMinimise` / `WindowUnminimise` / `WindowSetBackgroundColour` | 同名 | 何もしない |
| `WindowIsFullscreen` / `WindowIsMaximised` / `WindowIsMinimised` | 同名 | `Promise.resolve(false)` |
| `WindowIsNormal` | 同名 | `Promise.resolve(true)` |
| `WindowGetSize` | 同名 | `Promise.resolve({w: innerWidth, h: innerHeight})` |
| `WindowGetPosition` | 同名 | `Promise.resolve({x: 0, y: 0})` |
| `ScreenGetAll` | 同名 | `Promise.resolve([{isCurrent:true, isPrimary:true, width: screen.width, height: screen.height}])` |
| `BrowserOpenURL(url)` | 同名 | `http:` / `https:` のときだけ `window.open(url, '_blank', 'noopener,noreferrer')` |
| `Environment()` | 同名 | `GET /api/env` の `{buildType:"production", platform:<サーバーの GOOS>, arch:<GOARCH>}` を一度だけ取得して保持する。シリアルの可否は**サーバー**の OS で決まるので、ブラウザの OS ではなくサーバーの値を返す |
| `Quit()` | 同名 | 何もしない（`console.warn`）。ブラウザからサーバーを止めさせない |
| `Hide()` / `Show()` | 同名 | 何もしない |
| `ClipboardGetText()` | 同名 | `navigator.clipboard.readText()`（失敗したら `""`） |
| `ClipboardSetText(text)` | 同名 | `navigator.clipboard.writeText(text)` → `true` / `false` |
| `OnFileDrop` / `OnFileDropOff` / `ResolveFilePaths` | 同名 | 何もしない |
| `CanResolveFilePaths()` | 同名 | `false` |
| `InitializeNotifications` / `CleanupNotifications` / `RegisterNotificationCategory` / `RemoveNotificationCategory` / `RemoveAllPendingNotifications` / `RemovePendingNotification` / `RemoveAllDeliveredNotifications` / `RemoveDeliveredNotification` / `RemoveNotification` / `SendNotification` / `SendNotificationWithActions` | 同名 | `Promise.resolve()`（何もしない） |
| `IsNotificationAvailable` / `RequestNotificationAuthorization` / `CheckNotificationAuthorization` | 同名 | `Promise.resolve(false)` |

shim の追加 UI: 画面右下に小さな「headless」バッジ（ログアウト、接続状態の表示）を `document.body` に足す。frontend のソースは変えない。SSE が切れている間はバッジを警告色にする。サーバーが `headless:shutdown` を送ったら「サーバーが停止しました」と表示する。

テスト: `webadmin/headless-shim.test.mjs`（`node --test`。phase4-serial-patch.test.mjs と同じ流儀）で、runtime.js の `export function` の名前を正規表現で全部拾い、shim の `window.runtime` にすべてあることを確かめる。

---

### 5.4 キーボードショートカット（ブラウザ）

ブラウザは Cmd+T / Cmd+W / Ctrl+Shift+T / Ctrl+Shift+W とタブの切り替えを自分で使い、ページに渡さない（Cmd+W では管理画面のタブそのものが閉じる。レビューの指摘 7）。そこで `frontend/src/shortcuts.js` に `'browser'` の組み合わせを足し、`window.__sshGeteHeadless`（shim が定義する）があれば `main.js` はそれを使う。どの OS でも Alt+Shift（macOS では Option+Shift）＋ T（新しいシェル）/ W（タブを閉じる）/ ] [ か PageDown PageUp（次・前のタブ）/ 1..9 / → ← 0（サーバー）。Option でキーの文字が変わるので、キーは `event.code`（物理的な位置）で判定する。GUI 版の組み合わせは変えない。利用者向けの表は docs/headless.md §3。

## 6. 認証とセキュリティ（最重要）

### 6.1 待受の安全条件（満たさなければ起動を拒否。終了コード2）

| 待受 | TLS | ログイン | 判定 |
|---|---|---|---|
| ループバック（127.0.0.0/8、::1、localhost） | 任意 | **必須**（無効にする設定は用意しない） | 起動する |
| それ以外（0.0.0.0、::、LAN の IP） | `files` か `self-signed` が**必須** | 必須 | TLS が無ければ拒否。メッセージで `--tls-self-signed` か `--tls-cert/--tls-key` を案内する |
| ループバック＋`publicURL` が https | 任意（TLS はプロキシが終端） | 必須 | 起動する。Cookie に `Secure` を付ける |
| ループバック＋`publicURL` が http で、ホストがループバック以外 | — | — | 拒否（平文で LAN に出ることになるため） |

ループバックでもログインを必須にする理由: (1) 複数のユーザーがいる Linux では他のユーザーも 127.0.0.1 に届く。(2) SameSite はポートを区別しないので、同じ PC の別ポートのページ（開発サーバーなど）は「同じサイト」として Cookie を送れてしまう。Origin 検査とログインの両方で守る。

そのほかの起動拒否: Web の待受と MCP の待受が同じアドレスとポート。`SSH_GETE_TOKEN`（MCP の Bearer）が管理トークンと同じ値（秘密の使い回し）。管理トークンのファイルの権限が 0600 より広い（Unix）。

### 6.2 管理トークン

- 形式: `sgadm_` ＋ 32 バイトの乱数の base64url（43文字）。接頭辞で MCP のトークンと見分けられる（ログやチャットに貼られたときの判別、誤用の検出）。
- 初回起動で作り、`<dataDir>/admin-token`（または `web.adminTokenFile`）に **0600** で保存する（一時ファイルに書いて fsync し rename）。ディレクトリは 0700。Windows ではユーザープロファイル配下の ACL を継承する（0600 は意味を持たないので、データディレクトリをユーザー配下に置くことを README に書く）。
- 標準出力にはファイルの**パスだけ**を出す。トークンの値は `--print-admin-token` を明示したときだけ出す。ログには一切出さない。
- サーバーはメモリに sha256 だけを持ち、比較は `subtle.ConstantTimeCompare` で行う。
- 再発行: `--rotate-admin-token`（オフラインでも動作中でもよい）または `POST /api/admin/rotate-token`。動作中のサーバーはトークンファイルの mtime とハッシュを最大5秒ごとに確かめ、変わっていれば「世代」を進めて**全セッションを無効**にし、SSE をすべて閉じる。Web から再発行したときは、新しいトークンを応答で1回だけ返し、自分のセッションも無効にする（再ログインさせる）。

### 6.2.1 管理パスワード（任意）

- 既定では無い（トークンだけ）。利用者が選んだときだけ使う。トークンは常に復旧手段として使える。
- 保存: argon2id のハッシュ（PHC 形式の1行）を `<dataDir>/admin-password`（または `web.adminPasswordFile`）に **0600** で置く。平文はどこにも残さない。ファイルのパラメーターには上限を付け（書き換えによる過大なメモリ・時間の要求を拒否）、同時に計算するのは2件まで。
- 規則: 12 文字以上、1024 バイト以下。前後の空白、制御文字、`sgadm_` で始まる値、管理トークンと同じ値は拒否する。MCP の実効の Bearer（`SSH_GETE_TOKEN` か DB の値）と生きているエージェント用トークンと同じ値も拒否する（`POST /api/admin/password` と `--set-admin-password` の両方。レビューの指摘 4）。
- 逆向き: 動作中の `SaveMCPSettings` は、新しい Bearer が管理トークンか管理パスワードと同じなら保存しない（起動時の検査 §6.1 は管理トークンしか見ないため）。
- 設定: `ssh-gete --set-admin-password`（標準入力から。端末では確認の入力あり）、または `POST /api/admin/password {"current","password"}`（セッション必須。`current` は今のトークンかパスワード。`password` が空なら削除）。成功は 204。
- 設定・変更・削除のたびに世代を進めて**全セッションを無効**にする（自分も含む）。動作中のサーバーはファイルを最大5秒ごとに確かめ、外から変わっても同じく全セッションを無効にする。0600 より広い mode や壊れた中身なら、パスワードでのログインを止める（トークンは使える）。
- `GET /api/session` の応答に `passwordSet` を足す。起動時の表示にも設定の有無を出す。

### 6.3 ログインとセッション

- `GET /login`：`webadmin/login.html`（静的。インラインスクリプトなし）。`POST /api/login {"token":"…"}` または `{"password":"…"}`（どちらか一方。両方・どちらも無しは 400）を `fetch` で送る。ログイン画面は `sgadm_` で始まる入力をトークン、それ以外をパスワードとして送る（`guardHTTP` が POST に `application/json` を要求するので、フォームの送信は使わない）。
- 成功: 204 と Cookie、それにセッション鍵（応答ヘッダ `X-SSH-Gete-Session-Key`、32 バイトの乱数）。
  - セッション鍵は Cookie に入れない。ログイン画面がこのオリジンの localStorage（使えなければ sessionStorage）に置き、shim が `/api/*` のすべての要求に `X-SSH-Gete-Session: <鍵>` を付ける。サーバーは sha256(鍵) をセッションに持ち、`/api/*` では Cookie と鍵の両方が合うときだけ通す（合わなくてもセッションは消さない）。ページ（静的ファイル）は Cookie だけで配る。
  - 理由: ブラウザは Cookie をポートを区別せず同じホストの全サービスへ送る（SameSite もポートを見ない）。同じ PC の別ポートのサービス（開発サーバーなど）は Cookie を受け取って、Origin を偽った別のクライアントから再送できる。localStorage はポートを含むオリジンごとなので、そこからは読めない（レビューの指摘 5）。
  - TLS（または https の `publicURL`）のとき: `__Host-ssh-gete-admin=<id>; Path=/; HttpOnly; Secure; SameSite=Strict`
  - 平文のループバックのとき: `ssh-gete-admin-<port>=<id>; Path=/; HttpOnly; SameSite=Strict`（ポートを名前に入れて、別ポートの別インスタンスと衝突させない）
  - `Max-Age` は付けない（ブラウザを閉じたら消えるセッション Cookie）。期限はサーバー側で管理する。
- セッション ID は 32 バイトの乱数。サーバーは sha256(ID) をキーに `{作成時刻, 最終利用時刻, 接続元, トークンの世代}` をメモリに持つ。プロセスを再起動すると全員ログアウトになる（DB に持たないことで漏洩の面を減らす）。
- 期限: 最後の利用から `idleMinutes`（既定30分）、作成から `maxHours`（既定12時間）。認証つきの API 呼び出しと SSE の接続のたびに最終利用時刻を進める（ping では進めない）。上限 `maxSessions`（既定32）を超えたら最も古いものを消す。
- ログイン成功のたびに新しい ID を発行する（セッション固定の防止）。
- `POST /api/logout`：セッションを消し、Cookie を `Max-Age=0` で消し、そのセッションの SSE を閉じる。
- `GET /api/session`：`{authenticated, idleExpiresAt, expiresAt}`（shim のバッジ用）。

### 6.4 CSRF と Origin・Host の検査（`web_guard.go`）

すべての要求に、外側から次の順で適用します。

1. `securityHeaders`（§6.8）
2. `guardHTTP(webGuardSettings, …)` を**そのまま流用**する。`webGuardSettings` は `MCPSettings{Port: Web のポート, BaseURL: publicURL, AllowedOrigins: 自分のオリジン}` で作る。自分のオリジンは、`publicURL` があればそれ、無ければ `http(s)://127.0.0.1:<port>`、`http(s)://localhost:<port>`、`http(s)://[::1]:<port>`、待受の IP。これで Host の検査（DNS リバインディング対策）、許可していない Origin の拒否、POST の `application/json` 必須が効く。
3. `/api/*` だけに追加で:
   - `X-SSH-Gete-CSRF: 1` が無ければ 403。カスタムヘッダは別オリジンからはプリフライトが必要になり、プリフライトは 2 で拒否される。
   - GET 以外は `Origin` ヘッダが**必須**で、自分のオリジンと完全一致すること（ブラウザは POST の fetch に必ず Origin を付ける）。
   - `Sec-Fetch-Site` があれば `same-origin` だけを通す。
   - `Authorization` ヘッダがあれば 400（`authorization header is not accepted here`）。MCP の Bearer やエージェント用トークンを管理 API に持ち込ませない（§6.5）。
   - 本文の上限は 1 MiB（`http.MaxBytesReader`）。
4. 認証（`requireSession`）：`/login`、`/login-assets/*`、`/api/login` 以外はすべてセッションが必要。ページ（`/`）は未ログインなら `/login` へ 303、`/api/*` は 401。

CORS のヘッダは自分のオリジンにしか返らないので、別オリジンのページは管理 API の応答を読めない。

### 6.5 MCP の資格情報と管理の資格情報の分離

| 方向 | 防ぐ仕組み |
|---|---|
| MCP の Bearer / エージェント用トークンで管理 API を呼ぶ | 管理 API は Cookie のセッションしか見ない。`Authorization` ヘッダがあれば 400。ログインは管理トークンのハッシュとしか比較しない |
| 管理トークン / 管理の Cookie で MCP を呼ぶ | MCP は今までどおり `authenticateMCP` で MCP のトークンとエージェント用トークンだけを照合する。管理トークンとは値が違う（同じなら起動を拒否、§6.1）。MCP は Cookie を見ない |
| 待受の共有 | 別のポート（8787 と 8788）。同じなら起動を拒否 |

テストで4方向すべてを確かめる（§10）。

### 6.6 TLS

- `files`：指定の PEM を読む。鍵ファイルの権限が広ければ警告。証明書は起動時に読み、SIGHUP では読み直さない（v1）。
- `self-signed`：`<dataDir>/web-tls/cert.pem` と `key.pem`（0600）。ECDSA P-256、有効期間 397 日。SAN は待受の IP、`publicURL` のホスト、`os.Hostname()`、`localhost`、`127.0.0.1`、`::1`。既存の証明書が期限の30日前を過ぎたか、SAN が足りなければ作り直す。SHA-256 の指紋を起動時に標準出力へ出し、ブラウザの警告画面で照合できるようにする。
- `tls.Config{MinVersion: tls.VersionTLS12}`。HTTP/2 は Go の既定で有効。
- `Strict-Transport-Security: max-age=31536000` は、https の `publicURL` のホストがホスト名（`localhost`・`*.localhost`・IP 以外）のときに、そのホストあての要求にだけ付ける。HSTS はポートを区別しないので、`https://localhost:8788` に付けると同じ PC の `http://localhost:*`（開発サーバー、MCP の `http://localhost:8787`）まで 1 年間 https に書き換えられる（レビューの指摘 3）。IP にはブラウザが HSTS を適用しない。
- `publicURL` に既定のポート（https の `:443`、http の `:80`）が書かれていたら外して扱う。ブラウザは Host にも Origin にも既定のポートを書かないので、外さないと Host の検査とオリジンの一覧がどちらも一致しない（レビューの指摘 2）。Host の比較でも、`publicURL` の scheme の既定のポートは有無を問わない。

### 6.7 ログインの総当たり対策

トークンは 256 ビットなので総当たりは現実的でないが、多重の防御として次を入れます。

| 規則 | 値 |
|---|---|
| 失敗のたびの遅延 | 接続元ごとに、失敗回数 n に対して `min(250ms × 2^(n-1), 8s)`。遅延の間は応答を返さない。同時に遅延中にできるのは全体で 16 件まで（超えたら即 429） |
| 接続元のロック | 15分のうちに 10 回失敗したら、その接続元を 15 分ロック（429、`Retry-After`） |
| 全体のロック | 15分のうちに全体で 100 回失敗したら、**ループバック以外**からのログインを 15 分止める。ループバックからは止めない（管理者が締め出されないように） |
| 結果待ちの試行 | ログインの本文を読み終えてから枠を取り、照合の結果が出るまで「失敗するかもしれない1回」として数える。接続元では「直近の失敗＋結果待ち」が 10、全体では 100 に達したら、それ以上は照合せずに 429（`Retry-After: 1`）。遅延もこの数から計算する。同時に送られた推測が、失敗を記録する前に全部照合されるのを防ぐ（レビューの指摘 1）。本文を送らない接続は枠を取らない |
| 成功 | その接続元の失敗回数を消す（同時に走っている別の試行の枠と、その間に付いたロックは残す） |
| 接続元の判定 | `r.RemoteAddr` の IP。`X-Forwarded-For` は信用しない（プロキシの後ろでは全体のロックとして働く） |

失敗と成功、ロックの発動は監査ログに残す（送られたトークンの値は残さない）。

### 6.8 セキュリティ用のヘッダ（全応答）

```
Content-Security-Policy: default-src 'self'; script-src 'self'; style-src 'self' 'unsafe-inline'; img-src 'self' data:; font-src 'self' data:; connect-src 'self'; object-src 'none'; base-uri 'none'; form-action 'self'; frame-ancestors 'none'
X-Frame-Options: DENY
X-Content-Type-Options: nosniff
Referrer-Policy: no-referrer
Cross-Origin-Opener-Policy: same-origin
Cross-Origin-Resource-Policy: same-origin
Permissions-Policy: camera=(), microphone=(), geolocation=(), usb=(), serial=()
Cache-Control: no-store            （HTML、API、shim、ログイン画面）
Cache-Control: private, max-age=86400   （/assets/* のハッシュ付きファイルだけ。中身に秘密は無い）
```

`style-src 'unsafe-inline'` は xterm.js と AdminLTE が `<style>` 要素を差し込むために必要。スクリプトのインラインは許さない。実際にブラウザで CSP 違反が出ないことを §10 の手動確認で確かめる。

### 6.9 監査ログ

新しい表 `admin_audit`（`web_audit.go` が `CREATE TABLE IF NOT EXISTS` で作る。`user_version` は上げない）:

```sql
CREATE TABLE IF NOT EXISTS admin_audit (
  id         INTEGER PRIMARY KEY AUTOINCREMENT,
  at         TEXT NOT NULL,      -- RFC3339
  actor      TEXT NOT NULL,      -- 'web:<セッションIDのsha256の先頭8桁>' | 'config' | 'cli'
  remote     TEXT NOT NULL,      -- 接続元の IP
  action     TEXT NOT NULL,      -- 'call:<Method>' | 'login_ok' | 'login_fail' | 'login_locked' | 'logout' | 'token_rotate' | 'password_change' | 'config_override' | 'config_create' | 'config_prune'
  target     TEXT NOT NULL DEFAULT '',  -- id / connID / name など（§3.3 の「監査に残す引数」）
  detail     TEXT NOT NULL DEFAULT '',  -- 変更したフィールド名、command の sha256 と長さ、入力バイト数など。秘密は入れない
  ok         INTEGER NOT NULL,
  code       TEXT NOT NULL DEFAULT ''   -- ActionResult.Code
);
CREATE INDEX IF NOT EXISTS admin_audit_at ON admin_audit(at);
```

- 同じ内容を1行で標準エラーにも出す（journald で見られる）。
- 保持は最新 10 万件。起動時と1日1回、古いものを消す。
- 閲覧は `GET /api/admin/audit?limit=N`（セッション必須。最大 500）。既存の画面には出さない（frontend を変えないため）。
- 承認の記録との対応: `ApproveCommand` などの監査には要求の id を残すので、`command_requests` の記録と突き合わせられる。`command_requests.decided_by` に「Web から」を入れるのは `approval.go` の変更になるので、統合後の課題にする（§11）。
- 記録の書き込みは呼び出しの応答を遅らせない。バッファつきの chan（1024件）と1本の書き込み goroutine で行い、溢れたら件数をログに出して捨てる。DB への書き込みは `a.mu` などを持たずに行う。

### 6.10 その他

- `http.Server{ReadHeaderTimeout: 5s, ReadTimeout: 30s, IdleTimeout: 120s, MaxHeaderBytes: 64 KiB}`。SSE があるので `WriteTimeout` は 0 とし、書き込みの期限は stream ごとに付ける（§4.2）。
- エラー応答には内部のパスやスタックを含めない。
- `/api/env` はセッション必須（OS と CPU の種類も攻撃の手がかりになるため）。

---

## 7. 秘密情報

### 7.1 保存先の選択

| `secrets.backend` | 実装 | 書き込み | 読み出し | 使いどころ |
|---|---|---|---|---|
| `keychain`（既定） | 既存の `osSecretStore` | キーチェーン。使えなければ `ErrSecretUnavailable`（保存を拒否。**今の挙動**） | キーチェーン | macOS / Windows / デスクトップの Linux |
| `file` | 新しい `fileSecretStore` | `<dataDir>/secrets.json`（0600） | 同左 | キーチェーンの無い Linux サーバー。**明示したときだけ** |
| `env` | 新しい `envSecretStore`（下層は書き込みを拒否する実装） | 拒否（`ErrSecretUnavailable`） | 設定ファイルで対応づけた環境変数だけ | コンテナ、systemd の `LoadCredential` |
| `memory` | 既存の `memorySecretStore` | メモリ | メモリ | **テスト専用**。テストの外では `secrets.backend: memory` を誤りとして起動を拒否し、`SSH_GETE_KEYRING=memory` は警告して無視する（REQ-014。`memorySecretsAllowed = testing.Testing`） |

- 環境変数の対応づけ（`passwordEnv` / `passphraseEnv`）は、どの backend でも**上に重ねる**（`envSecretStore` が下層を包む）。対応づけた account の読み出しは環境変数を優先する。
- `SSH_GETE_KEYRING` 環境変数に `file` と `env` も足す。設定ファイルより優先する（緊急時のため）。`memory` はテストのバイナリの中でだけ効く。
- 選択は `NewApp` の**前に**決め、`secretBackendFactory` に入れる（§1.5）。`NewApp` の中のキーチェーン移行（`migrateKeychain`）は、選んだ保存先へ移す。`file` を選んだ環境では、DB の credential 列に残っていた平文のパスワードがファイルへ移る。
- キーチェーンが使えない環境で既定（`keychain`）のまま起動したら、起動ログに「パスワードは保存できません。鍵認証か ssh-agent を使うか、`secrets.backend: file` / `env` を選んでください」と出す（起動はする）。保存しようとすると既存の `keyring_unavailable` が返り、画面は「このセッションだけ使う」を出す（phase5-keychain §8 のまま）。

### 7.2 `fileSecretStore`

- 形式: `{"version":1,"items":{"conn/<id>/password":"…", …}}`。暗号化はしない（鍵を同じ場所に置くことになり、意味がないため）。ファイルを読める人は秘密を読める、と README に書く。
- 書き込み: プロセス内の mutex で直列化し、一時ファイル（0600、同じディレクトリ）に書いて fsync し rename、ディレクトリも fsync。
- 読み込み: 起動時に読み、以後はメモリの写しを使う。Unix で mode が 0600 より広ければ**読まずに** `ErrSecretUnavailable` を返し、ログで `chmod 600` を案内する（ssh の流儀）。
- `SecretStore` の約束（ctx の期限までに戻る、存在しない項目の Delete は誤りにしない、ロックを持たずに呼ばれる）を守る。ファイル操作は速いので goroutine で包まない。
- サイズの上限: 1項目 64 KiB、ファイル全体 8 MiB。超えたら `ErrSecretTooBig`。

### 7.3 `envSecretStore`

- `bind(account, envName)` で対応を登録する。`Get` は対応があれば `os.LookupEnv`（無ければ `ErrSecretNotFound`）、無ければ下層へ。
- `Set` は、対応がある account で、値が環境変数の値と**同じ**なら何もせず成功する（`storeSecretVerified` の読み返しも一致する）。違う値なら `ErrSecretUnavailable`（環境変数は書き換えられない）。対応が無い account は下層へ。
- `Delete` は対応がある account では何もしない（成功）。対応が無ければ下層へ。
- 設定ファイルからの反映（`applyConfig`）での使い方:
  1. 接続先を `SaveConnection` で保存する（`Credential` は空）。戻り値の `ID` を得る。
  2. `bind(passwordAccount(ID), passwordEnv)`（パスフレーズは `passphraseAccount(ID, keyPath)`）。
  3. 環境変数に値があれば、その値を `Credential` に入れてもう一度 `SaveConnection` を呼ぶ。`Set` は 2 の規則で何もせず成功し、DB には `credentialSet` だけが残る。
  - `passwordFile` は 0600 を確かめて読み、`backend` が `file` / `keychain` ならその値で保存する。`env` のときは保存できないので、メモリの `sessionSecrets` に入れる（再起動のたびに読み直す）。

### 7.4 Linux での推奨

鍵認証（`auth: key`、鍵ファイルはサービス用ユーザーだけが読める場所）か ssh-agent を基本にし、パスワードが必要なものだけ systemd の `LoadCredential=` ＋ `passwordFile: ${CREDENTIALS_DIRECTORY}/<name>`、またはコンテナの secret ＋ `passwordEnv` にする。

---

## 8. MCP との同居と終了処理

- headless でも MCP サーバー（既定 8787）は `app.startup` の中で今までどおり起動する。MCP の設定は Web 画面の既存の MCP 画面から `SaveMCPSettings` で変えられる（再起動も既存の経路）。
- Web 管理の待受は MCP と別の `http.Server`。ライフサイクルは `webServer` が持ち、`a.lifecycleMu` は使わない。

終了（SIGINT / SIGTERM。Windows では Ctrl+C と、Go が SIGTERM として届けるコンソールの閉鎖・ログオフ・シャットダウン）:

| 順 | 処理 | 期限 |
|---|---|---|
| 0 | 「停止します」をログに出す。以後2回目のシグナルが来たら即 `os.Exit(130)` | — |
| 1 | `web.StopIntake()`：新しい `/api/call` と `/api/login` に 503。全 SSE に `headless:shutdown` を送って閉じる | 即時 |
| 2 | `web.WaitCalls(5s)`：実行中の `/api/call` ハンドラー（App のメソッドを呼んでいるもの）を待つ | 5秒 |
| 3 | `web.Close(ctx)`：`http.Server.Shutdown`（2秒）、だめなら `Close` | 2秒 |
| 4 | 監査ログの書き込み goroutine を止める：chan を閉じ、書き終わるのを待つ（`store.Close` より前に行うため、ここで行う。監査に書くのは Web 層と設定の反映だけなので、以後に書き込みは来ない） | 1秒 |
| 5 | `app.shutdown(ctx)`：§5.0 の順序そのまま（appCancel → 実行中のコマンドの cancel → **MCP の受付を止める** → `drainMCPCommands`（最大 `shutdownExecGrace` × 2）→ `CloseAllTerminals` → `wg.Wait` → `store.Close`） | 既存のまま |
| 6 | インスタンスのロックを外して終了コード0 | — |

- 2 で待ちきれなかった呼び出しは、5 の appCancel で中断される（App のコマンド実行は `appCtx` に結びついている）。`store.Close` の後に戻ってきた呼び出しは DB のエラーを返すだけで、クラッシュはしない。
- systemd の `TimeoutStopSec=30` はこの合計に余裕を持たせた値。
- SIGHUP: v1 では設定を読み直さない（ログに出して無視）。設定を変えたら再起動する。

---

## 9. 配布

### 9.1 systemd の unit（`packaging/systemd/ssh-gete.service`）

```ini
[Unit]
Description=ssh-gete (headless SSH gateway for AI agents)
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
User=ssh-gete
Group=ssh-gete
ExecStart=/usr/local/bin/ssh-gete --headless --config /etc/ssh-gete/config.yaml --data-dir /var/lib/ssh-gete
StateDirectory=ssh-gete
StateDirectoryMode=0700
ConfigurationDirectory=ssh-gete
# 例: パスワードを systemd の credential で渡す（config では passwordFile: ${CREDENTIALS_DIRECTORY}/prod-web-1）
# LoadCredential=prod-web-1:/etc/ssh-gete/secrets/prod-web-1
# MCP の Bearer
# EnvironmentFile=/etc/ssh-gete/env   （SSH_GETE_TOKEN=... 0600）
Restart=on-failure
RestartSec=3
TimeoutStopSec=30
KillSignal=SIGTERM
UMask=0077

NoNewPrivileges=yes
ProtectSystem=strict
ProtectHome=yes
PrivateTmp=yes
PrivateDevices=no            # シリアルを使わないなら yes
# シリアルを使うとき
# SupplementaryGroups=dialout
# DeviceAllow=/dev/ttyUSB0 rw
ProtectKernelTunables=yes
ProtectKernelModules=yes
ProtectControlGroups=yes
RestrictAddressFamilies=AF_INET AF_INET6 AF_UNIX
RestrictNamespaces=yes
LockPersonality=yes
MemoryDenyWriteExecute=yes
SystemCallArchitectures=native
SystemCallFilter=@system-service
CapabilityBoundingSet=
ReadWritePaths=/var/lib/ssh-gete

[Install]
WantedBy=multi-user.target
```

- `ProtectHome=yes` なので、鍵は `/etc/ssh-gete/keys/`（`ssh-gete` ユーザーだけが読める 0600）に置く。ssh-agent を使う場合は `SSH_AUTH_SOCK` を渡し、`ProtectHome` を調整する。
- 初回の管理トークン: `sudo -u ssh-gete ssh-gete --print-admin-token --data-dir /var/lib/ssh-gete`。
- LAN から使う場合は `web.listen: 0.0.0.0:8788` と `tls.mode: self-signed`（または files）が必須（§6.1）。

### 9.2 Dockerfile（`packaging/docker/Dockerfile`）

`frontend/wailsjs` の生成に Wails CLI と WebKit の開発用ライブラリが要るため、イメージの中ではビルドせず、release の headless バイナリを入れます。

```dockerfile
# syntax=docker/dockerfile:1
# 使い方: release の ssh-gate-linux-<arch>-headless.tar.gz を展開した ssh-gete を
#   dist/linux-amd64/ssh-gete、dist/linux-arm64/ssh-gete に置いてから
#   docker buildx build --platform linux/amd64,linux/arm64 -f packaging/docker/Dockerfile .
FROM gcr.io/distroless/static-debian12:nonroot@sha256:<固定する>
ARG TARGETARCH
COPY --chmod=0755 dist/linux-${TARGETARCH}/ssh-gete /usr/local/bin/ssh-gete
# VOLUME より前に nonroot の 0700 で作る（無いと新しい名前付きボリュームが root:root 0755 になり、
# nonroot のプロセスが書けない。レビューの指摘 6）。distroless にはシェルが無いので空の
# /home/nonroot を別ステージからコピーする。
COPY --from=datadir --chown=65532:65532 --chmod=0700 /home/nonroot /data
ENV SSH_GETE_CONFIG_DIR=/data
VOLUME ["/data"]
EXPOSE 8787 8788
USER nonroot:nonroot
ENTRYPOINT ["/usr/local/bin/ssh-gete", "--headless", "--data-dir", "/data"]
CMD ["--config", "/config/config.yaml", "--listen", "0.0.0.0:8788", "--tls-self-signed"]
```

- コンテナの中では 127.0.0.1 に待ち受けてもホストから届かないので、既定の CMD を `0.0.0.0:8788` ＋自己署名にしている（§6.1 を満たす）。`-p 127.0.0.1:8788:8788` でホストのループバックにだけ公開することを README で推奨する。
- MCP も `mcp.listenAddress: 0.0.0.0` が必要になるので、既存の規則どおり Bearer が必須になる。
- キーチェーンは無いので `secrets.backend: env`（docker の secret を環境変数に）か `file`（`/data/secrets.json`）。
- distroless はシェルが無いので、`--print-admin-token` は `docker run --rm -v <vol>:/data <image> --print-admin-token` で読む（ENTRYPOINT にフラグを足す形）。
- ベースイメージは digest で固定する（release.yml の action を SHA で固定しているのと同じ方針）。
- CI（test.yml の `docker` ジョブ）で、linux/amd64 の headless バイナリからイメージを作り、新しい名前付きボリュームで `--print-admin-token` と起動（`/login` が返ること）を確かめる。

### 9.3 release.yml への追加案

既存の `build` ジョブ（GUI 版）は変えずに、次の2つを足す。

1. 既存の `build` の macOS 行に、`npm run build` の後で `frontend/dist` を `frontend-dist` という artifact で上げる手順を1つ足す（あるいは、別ジョブ `frontend` を macOS で動かす）。
2. 新しいジョブ `build-headless`（`needs: [test, build]`、`runs-on: ubuntu-latest`、`permissions: contents: read`）:
   - checkout（`persist-credentials: false`）、setup-go（`go-version-file: go.mod`）、`frontend-dist` を `frontend/dist` に download
   - matrix:

| artifact | GOOS/GOARCH | 梱包 |
|---|---|---|
| `ssh-gate-linux-x64-headless` | linux/amd64 | tar.gz（実行権を保つ） |
| `ssh-gate-linux-arm64-headless` | linux/arm64 | tar.gz |
| `ssh-gate-mac-arm64-headless` | darwin/arm64 | zip |
| `ssh-gate-mac-x64-headless` | darwin/amd64 | zip |
| `ssh-gate-win-x64-headless` | windows/amd64 | zip |
| `ssh-gate-win-arm64-headless` | windows/arm64 | zip |

   - ビルド: `CGO_ENABLED=0 GOOS=… GOARCH=… go build -tags headless -trimpath -ldflags "-s -w -X main.version=${GITHUB_REF_NAME}" -o ssh-gete[.exe] .`
   - 検査: `go list -deps -tags headless . | grep -q wailsapp && exit 1`
   - 梱包には `packaging/systemd/ssh-gete.service` と `packaging/headless.example.yaml` も入れる（Linux）。
   - macOS の headless は署名していないので、README に `xattr -d com.apple.quarantine ssh-gete` を書く。universal にする（lipo）のは macOS のランナーが要るので v1 では分ける。
3. `release` ジョブ: `needs: [build, build-headless]`。Checksums の対象を `*.zip *.tar.gz` に広げ、`files:` に `dist/*.tar.gz` を足す。

test.yml への追加: `headless` ジョブ（ubuntu）で `mkdir -p frontend/dist && touch frontend/dist/.keep` の後、`CGO_ENABLED=0 go vet -tags headless ./...`、`go test -tags headless -race ./...`（race は cgo が要るのでこの行だけ `CGO_ENABLED=1`）、6つの GOOS/GOARCH で `go build -tags headless`。既存のジョブ（タグなし）でも新しいファイルのテストが走る（`runHeadless` はタグなしなので GUI 版でもテストされる）。

---

## 10. テスト計画

方針: 既存の `testmain_test.go` の隔離（`SSH_GETE_CONFIG_DIR`・`HOME`・`SSH_GETE_KEYRING=memory` など）の下で動かす。本物の `~/Library/Application Support/ssh-gete` には触れない。すべて `-race`。並行系は `-count=20` でも回す。HTTP は `httptest` と実際の `net.Listen("127.0.0.1:0")` を使う。タグなしと `-tags headless` の両方で回す。

| ファイル | 主なテスト |
|---|---|
| `cli_test.go` | フラグの組み合わせ、GUI モードで知らない引数を無視すること、headless で知らない引数を拒否すること、優先順位（フラグ > 環境変数 > 設定ファイル） |
| `config_file_test.go` | 正常なファイル、知らないキー（`password:` を含む）の拒否、`${VAR}` の展開がパスのキーだけで起きること、group/other 書き込み可のファイルの拒否、検証の規則（名前の重複、待受の衝突、`passwordEnv` と `passwordFile` の同時指定） |
| `config_apply_test.go` | §1.4 の表の全行（新規・未変更なら DB 優先・変更なら設定ファイル優先で書かれたキーだけ上書き）、Web で名前を変えた後も重複しないこと、`prune` が設定ファイル由来だけを消すこと、`enforce`、`SSH_GETE_MCP_ADDR` があると待受を反映しないこと、MCP の反映が `SaveMCPSettings` と同じ DB の行になること（再起動を除く）、反映の後の `startup` で MCP が1回だけ Listen すること、`--check-config` が DB に書かないこと |
| `secret_store_file_test.go` | 往復、0600 で作ること、広い権限のファイルを読まないこと、rename の原子性（途中で失敗しても元が残る）、上限、`SecretStore` の約束（`secret_store_test.go` の共通テストを流用） |
| `secret_store_env_test.go` | bind した account の Get、同じ値の Set が成功し違う値が失敗すること、Delete が何もしないこと、bind していない account が下層に行くこと、`storeSecretVerified` が通ること |
| `secret_store_select_test.go` | backend ごとの選択、`SSH_GETE_KEYRING` の優先、`NewApp` の移行が選んだ保存先に向くこと |
| `web_rpc_test.go` | 許可リストの全メソッドを1回ずつ呼べること、許可リスト外が 404、引数の数と型の誤りが 400（Wails と同じ文言）、`(T, error)` の error が `{"error":…}`、パニックが 500、JSON の形が `models.ts` と一致すること（`Connection`・`MCPSettings`・`DashboardData` の往復）、`TestWebCallableCoversExportedMethods`、App.js との一致（あれば） |
| `web_events_test.go` | 全クライアントに届くこと、publish がブロックしないこと（止まったクライアントがいても publish が 1ms 以内に戻る）、遅いクライアントが切られて他は続くこと、`Last-Event-ID` での再送、リングから落ちた後の `headless:resync` の中身（offsets・exits・sessions）、`o` の累計の正しさ（`dropped` を含む）、ping、ログアウトで SSE が閉じること |
| `web_auth_test.go` | トークンの作成（0600、接頭辞、標準出力にパスだけ）、ログインの成功と失敗、定数時間の比較を使っていること、Cookie の属性（TLS のとき `__Host-`・`Secure`、平文のときポート付きの名前）、idle と最大の期限、ログイン時の ID の作り直し、上限、再発行で全セッションが無効になること（ファイルを外から書き換えた場合も） |
| `web_limiter_test.go` | 遅延の増え方、接続元のロック、全体のロックがループバックを止めないこと、成功で回数が消えること（時計は差し替え） |
| `web_guard_test.go` | 別の Host（DNS リバインディング）が 403、別の Origin が 403、POST に Origin が無いと 403、`X-SSH-Gete-CSRF` が無いと 403、`text/plain` の POST が 415、`Sec-Fetch-Site: cross-site` が 403、`Authorization` ヘッダが 400、本文の上限、全応答にセキュリティ用のヘッダがあること、未ログインのページが `/login` に、API が 401 になること |
| `web_separation_test.go` | MCP の Bearer・エージェント用トークンで管理 API が呼べないこと、管理トークン・管理の Cookie で MCP が 401 になること、`SSH_GETE_TOKEN` と管理トークンが同じなら起動を拒否すること |
| `web_tls_test.go` | 自己署名の作成と SAN、期限が近いときの作り直し、鍵の 0600、指紋の表示、非ループバックで TLS が無いと起動を拒否すること（§6.1 の表の全行） |
| `web_static_test.go` | shim の差し込み位置（`<head>` の直後、モジュールスクリプトより前）、`<head>` が無い index で起動を拒否すること、ハッシュ付きファイルのキャッシュヘッダ、`/headless-shim.js` がセッション必須で `/login-assets/` は不要なこと |
| `web_audit_test.go` | §3.3 の「監査」列どおりに記録されること、秘密（パスワード、パスフレーズ、トークン、`tokenInput`）が監査にもログにも出ないこと（既知の値を入れて全文を検索する）、保持件数、chan が溢れても応答が遅れないこと |
| `headless_run_test.go` | 実際に `runHeadless` を一時ディレクトリで動かし、ログイン → `GetInitialData` → `SaveConnection` → SSE で `sessions:changed` を受け取る、の一連の流れ。SIGTERM 相当（ctx の cancel）で §8 の順序（Web の受付停止 → 監査の flush → MCP の停止 → `store.Close`）になること。2回目のシグナル。インスタンスのロックで2つ目が終了コード3になること |
| `headless_build_test.go`（`//go:build headless`） | `go list -deps` に wailsapp が無いこと。`wailsEmit` が何もしないこと |
| `webadmin/headless-shim.test.mjs` | runtime.js の全 export が shim にあること、Proxy の呼び出しの形（URL・ヘッダ・本文）、reject が文字列であること、401 でログイン画面に移ること、`EventsOnMultiple` の回数、`EventsOff`、SSE の解析（行の分割、ping、`id`）、再接続の待ち時間、resync の合成イベント（dropped・exit・sessions の順） |

手動確認（リリース前に1回）: Linux（GTK なしの最小イメージ）で headless 専用ビルドが起動すること。Chrome・Firefox・Safari で、ログイン、承認（確認ダイアログが出ること）、接続先の編集、ターミナル（`yes` を流して重くならず、遅いタブだけ切れて再接続で「省略」が出ること）、MCP の設定、クリップボードのコピーが動き、CSP の違反がコンソールに出ないこと。自己署名で LAN から接続できること。macOS の GUI 版で `--headless` が動くこと。Windows の headless で Ctrl+C の終了が §8 どおりになること。

---

## 11. 並列実装の担当分け（ファイル単位）

既存ファイルを触るのは A（`main.go`・`events.go`）、F（`secret_store.go` の1行）、G（workflow）だけです。他はすべて新しいファイルです。

### A：起動・ビルドの分割・終了処理

| ファイル | 公開する（package 内で他の担当が使う）関数・型 |
|---|---|
| `cli.go` | `type CLIOptions struct{ Headless bool; ConfigPath, Listen, DataDir, TLSCert, TLSKey string; TLSSelfSigned, PrintAdminToken, RotateAdminToken, CheckConfig, ShowVersion bool }`、`func parseCLI(args []string, gui bool) (CLIOptions, error)` |
| `headless_run.go` | `func runHeadless(opts CLIOptions, dist fs.FS) int`、`func headlessShutdown(app *App, web *webServer, audit *adminAuditWriter, ctx context.Context)` |
| `main.go`（既存・変更） | `main()` の先頭で `parseCLI(os.Args[1:], true)` → `--headless` なら `attachParentConsole()` と `os.Exit(runHeadless(opts, distFS))` |
| `main_headless.go` | `var assets embed.FS`（`//go:embed all:frontend/dist`）、`func main()` |
| `events.go`（既存・変更） | `wruntime.EventsEmit` → `platformEmit` |
| `events_platform.go` | `type eventSink interface{ publish(name string, payload any) }`、`func withWebHub(ctx context.Context, s eventSink) context.Context`、`func webHubFrom(ctx context.Context) eventSink`、`func platformEmit(ctx context.Context, name string, payload any)` |
| `events_wails.go` / `events_headless.go` | `func wailsEmit(ctx context.Context, name string, payload any)` |
| `console_windows.go` / `console_other.go` | `func attachParentConsole()` |
| `instance_lock_unix.go` / `instance_lock_windows.go` | `func acquireInstanceLock(dir string) (release func(), err error)`、`var errInstanceLocked` |
| テスト | `cli_test.go`、`headless_run_test.go`、`headless_build_test.go` |

### B：設定ファイル

| ファイル | 公開する関数・型 |
|---|---|
| `config_file.go` | `type FileConfig struct{ Version int; DataDir, Apply string; Web WebConfig; MCP *MCPConfig; Policy *PolicyConfig; Secrets SecretsConfig; Connections []ConnectionConfig; Prune bool }`、`type WebConfig struct{ Listen, PublicURL, AdminTokenFile string; TLS WebTLSConfig; Session SessionConfig; MaxSessions int }`、`type WebTLSConfig struct{ Mode, Cert, Key string }`、`type SessionConfig struct{ IdleMinutes, MaxHours int }`、`type MCPConfig`、`type PolicyConfig`、`type SecretsConfig struct{ Backend, File string }`、`type ConnectionConfig`、`func loadConfigFile(path string) (*FileConfig, error)`、`func (c *FileConfig) validate() error`、`func resolveWebConfig(c *FileConfig, opts CLIOptions) (WebConfig, error)`、`func (cc ConnectionConfig) toConnection() Connection` |
| `config_apply.go` | `type ConfigApplyReport struct{ Created, Overridden, Kept, Pruned []string }`、`func applyConfig(a *App, c *FileConfig, env *envSecretStore, audit auditSink) (ConfigApplyReport, error)`、`func planConfig(a *App, c *FileConfig) (ConfigApplyReport, error)`、`func ensureConfigAppliedTable(s *Store) error`、`func (a *App) applyMCPSettingsNoRestart(s MCPSettings) error` |
| 例 | `packaging/headless.example.yaml` |
| テスト | `config_file_test.go`、`config_apply_test.go` |
| 依存 | `go.yaml.in/yaml/v3 v3.0.4` を go.mod に追加（統合担当が `go mod tidy`） |

### C：Web サーバー・認証・TLS・監査

| ファイル | 公開する関数・型 |
|---|---|
| `web_server.go` | `type webServer struct`、`func newWebServer(app *App, cfg WebConfig, dataDir string, dist fs.FS, hub *eventHub, auth *adminAuth, audit auditSink) (*webServer, error)`、`func (s *webServer) Start() error`、`func (s *webServer) StopIntake()`、`func (s *webServer) WaitCalls(timeout time.Duration) bool`、`func (s *webServer) Close(ctx context.Context) error`、`func (s *webServer) URL() string`、`func validateWebListen(cfg WebConfig, mcp MCPSettings, mcpToken, adminTokenHash string) error` |
| `web_auth.go` | `func loadOrCreateAdminToken(path string) (token string, created bool, err error)`、`func rotateAdminToken(path string) (string, error)`、`type adminAuth struct`、`func newAdminAuth(tokenPath string, cfg SessionConfig, secure bool, port int, now func() time.Time) (*adminAuth, error)`、`func (a *adminAuth) handleLogin(w http.ResponseWriter, r *http.Request)`、`func (a *adminAuth) handleLogout(w http.ResponseWriter, r *http.Request)`、`func (a *adminAuth) handleRotate(w http.ResponseWriter, r *http.Request)`、`func (a *adminAuth) requireSession(next http.Handler) http.Handler`、`func sessionFrom(ctx context.Context) (adminSession, bool)`、`func (a *adminAuth) onInvalidate(f func(sessionID string))` |
| `web_limiter.go` | `type loginLimiter struct`、`func newLoginLimiter(now func() time.Time) *loginLimiter`、`func (l *loginLimiter) check(remote string) (delay time.Duration, err error)`、`func (l *loginLimiter) fail(remote string)`、`func (l *loginLimiter) succeed(remote string)` |
| `web_guard.go` | `func webGuard(cfg WebConfig, port int, next http.Handler) http.Handler`（内部で `guardHTTP` を使う）、`func securityHeaders(next http.Handler) http.Handler`、`func webOrigins(cfg WebConfig, port int, tls bool) []string` |
| `web_tls.go` | `func webTLSConfig(cfg WebConfig, dataDir string, now time.Time) (*tls.Config, string /*fingerprint*/, error)`、`func loadOrCreateSelfSigned(dir string, hosts []string, now time.Time) (tls.Certificate, string, error)` |
| `web_static.go` | `func newStaticHandler(dist fs.FS, admin fs.FS) (http.Handler, error)`、`func injectShim(index []byte) ([]byte, error)` |
| `web_audit.go` | `type adminAuditEntry struct{ At, Actor, Remote, Action, Target, Detail string; OK bool; Code string }`、`type auditSink interface{ record(e adminAuditEntry) }`、`func ensureAdminAuditTable(s *Store) error`、`type adminAuditWriter struct`、`func newAdminAuditWriter(s *Store) *adminAuditWriter`、`func (w *adminAuditWriter) record(e adminAuditEntry)`、`func (w *adminAuditWriter) Close(timeout time.Duration)`、`func listAdminAudit(s *Store, limit int) ([]adminAuditEntry, error)` |
| テスト | `web_auth_test.go`、`web_limiter_test.go`、`web_guard_test.go`、`web_separation_test.go`、`web_tls_test.go`、`web_static_test.go`、`web_audit_test.go` |

### D：呼び出し層とイベント層

| ファイル | 公開する関数・型 |
|---|---|
| `web_rpc.go` | `type callSpec struct{ Audit bool; Target func(args []json.RawMessage) string; Detail func(args []json.RawMessage) string }`、`var webCallable map[string]callSpec`、`var webDenied map[string]string`、`func checkWebCallable(t reflect.Type) error`、`func callAppMethod(app *App, name string, body []byte) (result any, callErr error, status int)`、`func newRPCHandler(app *App, audit auditSink) http.Handler`、`func handleEnv(w http.ResponseWriter, r *http.Request)` |
| `web_events.go` | `type hubConfig struct{ ClientQueue, ClientBytes, RingEvents, RingBytes, PerSession, Total int; WriteTimeout, Ping time.Duration }`、`type eventHub struct`、`func newEventHub(cfg hubConfig, snapshot func() sessionsPayload) *eventHub`、`func (h *eventHub) publish(name string, payload any)`（`eventSink` を満たす）、`func (h *eventHub) ServeHTTP(w http.ResponseWriter, r *http.Request)`、`func (h *eventHub) closeSession(sessionID string)`、`func (h *eventHub) shutdown()` |
| テスト | `web_rpc_test.go`、`web_events_test.go` |

### E：shim とログイン画面

| ファイル | 公開するもの |
|---|---|
| `webadmin/headless-shim.js` | `window.go.main.App`（Proxy）、`window.runtime`（§5.3 の全関数）、SSE の受信と再接続、resync の合成イベント、右下のバッジ |
| `webadmin/login.html`・`webadmin/login.js`・`webadmin/login.css` | `/login` の画面。`POST /api/login` を fetch で送り、成功したら `next`（同じオリジンのパスだけ）に移る。日本語と英語 |
| `web_embed.go` | `//go:embed webadmin` の `var webAdminFiles embed.FS` |
| テスト | `webadmin/headless-shim.test.mjs`、`webadmin/login.test.mjs`（`node --test`） |

### F：秘密情報

| ファイル | 公開する関数・型 |
|---|---|
| `secret_store_file.go` | `type fileSecretStore struct`、`func newFileSecretStore(path string) (*fileSecretStore, error)`、`Get` / `Set` / `Delete`（`SecretStore`） |
| `secret_store_env.go` | `type envSecretStore struct`、`func newEnvSecretStore(lower SecretStore, lookup func(string) (string, bool)) *envSecretStore`、`func (e *envSecretStore) bind(account, envName string)`、`func (e *envSecretStore) bound() int`、`Get` / `Set` / `Delete`、`type denySecretStore struct{}`（`env` の下層） |
| `secret_store_select.go` | `var secretBackendFactory func() SecretStore`、`func selectSecretBackend(cfg SecretsConfig, dataDir string) (SecretStore, *envSecretStore, error)`、`func installSecretBackend(s SecretStore)` |
| `secret_store.go`（既存・1行） | `defaultSecretStore()` の先頭で `secretBackendFactory` を見る |
| テスト | `secret_store_file_test.go`、`secret_store_env_test.go`、`secret_store_select_test.go` |

### G：配布

| ファイル | 内容 |
|---|---|
| `packaging/systemd/ssh-gete.service` | §9.1 |
| `packaging/docker/Dockerfile`・`packaging/docker/README.md` | §9.2 |
| `.github/workflows/release.yml`（追加だけ） | §9.3 の `frontend-dist` の受け渡し、`build-headless`、release の対象 |
| `.github/workflows/test.yml`（追加だけ） | `headless` ジョブ |
| `README.md`（節を1つ追加） | headless の使い方、管理トークン、LAN 公開の条件、平文のときのタブ数の制約、秘密の保存先の選び方 |

### 担当どうしの依存と順序

- 型の先出し: B の `WebConfig` / `SessionConfig` / `SecretsConfig`、C の `auditSink` / `adminAuditEntry`、A の `eventSink` は、最初の半日で空の実装として先に入れ、他の担当はそれを前提に進める。
- A は B・C・D・F の関数を呼ぶだけ（中身を知らない）。A のテストでは偽物を差し込めるよう、`runHeadless` の内部を `headlessDeps` の構造体経由で組み立てる。
- E は Go のコードに依存しない（HTTP の約束 §3.1・§4.1・§6.3 だけに依存する）。
- 統合担当（1人、並列作業の後）: go.mod（yaml の追加と `go mod tidy`）、`runHeadless` の結線、全テストを両方のタグで実行、本体（`/Users/katouyoshiya/Documents/ssh-gate-oss`）のリスク判定・app.go などの修正とのマージ（このブランチで触る既存ファイルは §0 の4つだけなので、衝突はそこに限られる）、ssh-gate-spec.sqlite の REQ-015 の状態の更新、CLAUDE.md / design-v1 に §2.2 の「Wails の runtime は `*_wails.go` だけ」の規約を追記。

---

## 12. 後回しにすること・ユーザーが判断すべき点

後回し（v1 ではしない）:

1. `command_requests.decided_by` に「Web から（セッション）」を入れる（`approval.go` の変更になる）。v1 は `admin_audit` で突き合わせる。
2. 監査ログを見る画面（frontend の変更になる）。v1 は `GET /api/admin/audit` とログ。
3. 複数の管理ユーザー、権限の分離（閲覧だけの利用者など）。v1 は管理トークン1つ。
4. SIGHUP での設定の読み直し、TLS 証明書の読み直し。
5. 平文の HTTP で SSE を1本に束ねる（`BroadcastChannel`）。
6. Windows サービスとしての登録（v1 はタスクスケジューラか NSSM を README で案内）。macOS の launchd の plist の例。
7. macOS headless の universal バイナリと署名。

ユーザーに確認したい点:

1. ループバックでもログインを必須にした（§6.1）。ローカルだけで使うときにログインを省ける設定が欲しいか。
2. 設定ファイルの既定の反映を `seed`（Web の変更を優先）にした。サーバー運用では `enforce` を既定にした方がよいか。
3. 設定ファイルでエージェントの承認バイパスやエージェント用トークンを扱わないことにした（§1.3）。
4. Docker の既定の CMD を `0.0.0.0` ＋自己署名にした（§9.2）。
