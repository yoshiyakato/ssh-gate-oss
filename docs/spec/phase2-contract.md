# フェーズ2 インターフェース契約（マルチタブ・サイドバー・編集画面）

この文書は design-v1.md §5.0 / §5.1（第2〜4段）/ §5.2 / §5.4 / §6.1〜6.3 / §6.5 / §7 フェーズ2 を、7人（B1, B2, F1〜F5）が並列で実装できるように「名前・型・所有・呼び出し方向」まで決め切ったものです。実装後に Integrator が1人で統合します。仕様と食い違う点はここに理由つきで書き、ここに書いたものを正とします。

- 基準コミット: `a6f2706`（ブランチ `fix/v1-safety-multisession`）
- スコープ外: 接続プール（第5段）、TOFU/ホスト鍵（第3段）、上部サーバータブと `serverFilter`（第4段）、MCP long-poll の拡張、OnBeforeClose の確認ダイアログ、シリアルの `available`/`missing` のライブ判定（第4段）
- 全員共通: 周囲のコードに命名・コメント密度・イディオムを合わせる。バックエンドのメッセージは既存どおり日本語、UI 文言は JA/EN の両方を I18N に入れる。git commit / push はしない。自分の所有ファイル以外は編集しない（例外は本書で明示したものだけ）。

---

## 0. 所有ファイル一覧

| 担当 | 所有（新規/変更） | 他人のファイルへの例外編集 |
|---|---|---|
| **B1** | `terminal.go`、`events.go`（新規）、`terminal_test.go`（新規）、`sshtest_test.go`（shell/pty/KillAll の追加のみ。既存の exec 挙動は変えない） | `app.go` の3か所だけ（§1.6） |
| **B2** | `app.go`（B1 の3か所を除く）、`approval.go`（実行トラッカーの組み込み）、`app_sessions_test.go`（新規） | なし |
| **F1** | `frontend/src/terminal-tabs.js`（新規）、`frontend/src/terminal-tabs.test.js`（新規） | なし |
| **F2** | `frontend/src/sidebar.js`（新規）、`frontend/src/sidebar.test.js`（新規） | なし |
| **F3** | `frontend/src/editor.js`（新規）、`frontend/src/editor.test.js`（新規） | なし |
| **F4** | `frontend/src/model.js`（新規）、`frontend/src/model.test.js`（新規）、`frontend/package.json`・`package-lock.json`（vitest・jsdom の追加と `"test": "vitest run"`）、`frontend/vitest.config.js`（新規、`environment: 'jsdom'`） | なし |
| **F5** | `frontend/src/style.css` | なし |
| **Integrator**（並列作業の後） | `frontend/src/main.js`、`frontend/src/shell.js`（新規）、`frontend/src/shell.test.js`（新規）、`frontend/wailsjs/**`（`wails generate module` の生成物）、`.github/workflows/test.yml`（frontend ジョブに `npm test`） | 統合時の衝突解消に限り全ファイル |

触らないファイル: `mcp_server.go`（`a.terminalCounts()` の戻り型を維持するので変更不要）、`storage.go`、`ssh_exec.go`、`httpguard.go`、`main.go`。

---

## 1. Go 側

### 1.1 ロック規約（§5.0 の具体化）

- ロックは `a.mu`（B2）、`a.termMu`（B1）、`a.execMu`（B2）、出力合流用の `outputBuffer.mu`（B1、セッションごと）の4種類。**どの2つも同時に持たない。** `outputBuffer.mu` と `a.execMu` は末端ロックで、持っている間に他の関数を呼ばない。
- `a.termMu` を持ったまま `a.connectionByID` / `a.findConnection` / `a.emitEvent` / `a.sessionsChanged` を呼ばない（前2つは `a.mu` を取る）。必要な値はロックの外で先に取る。
- `emitEvent` と `sessionsChanged` は必ずロック解放後に呼ぶ。
- `ssh.Client` / シリアルポートを Close してよいのは、それを所有する `terminalSession` の `close` だけ。

### 1.2 公開 API（Wails バインド）— 正確なシグネチャ

B1（`terminal.go`）:

```go
func (a *App) StartTerminal(id string, connID string, cols int, rows int) ActionResult
func (a *App) SendTerminalInput(id string, data string) ActionResult
func (a *App) ResizeTerminal(id string, cols int, rows int) ActionResult
func (a *App) CloseTerminal(id string) ActionResult
func (a *App) CloseTerminalsByConnection(connID string) ActionResult
func (a *App) CloseAllTerminals() ActionResult
func (a *App) ListTerminalSessions() []TerminalInfo
func (a *App) ListSerialPorts() []string // 既存のまま
```

B2（`app.go`）:

```go
func (a *App) DisconnectConnection(id string) ActionResult // 既存の名前とシグネチャのまま、中身を拡張
func (a *App) GetInitialData() DashboardData                // 既存。フィールド追加
```

生成後の `frontend/wailsjs/go/main/App.d.ts` の該当行は次のとおりになる（Integrator が `wails generate module` 後に一致を確認する）。

```ts
export function CloseAllTerminals():Promise<main.ActionResult>;
export function CloseTerminal(arg1:string):Promise<main.ActionResult>;
export function CloseTerminalsByConnection(arg1:string):Promise<main.ActionResult>;
export function DisconnectConnection(arg1:string):Promise<main.ActionResult>;
export function ListTerminalSessions():Promise<Array<main.TerminalInfo>>;
export function ResizeTerminal(arg1:string,arg2:number,arg3:number):Promise<main.ActionResult>;
export function SendTerminalInput(arg1:string,arg2:string):Promise<main.ActionResult>;
export function StartTerminal(arg1:string,arg2:string,arg3:number,arg4:number):Promise<main.ActionResult>;
export function TestConnectionWithPassphrase(arg1:string,arg2:string):Promise<main.ActionResult>; // 既存。ターミナル用パスフレーズ入力でも使う
```

#### StartTerminal(id, connID, cols, rows)

1. `id` を検証する。`^TERM-[A-Za-z0-9-]{1,64}$` に合わなければ `Code:"invalid_id"`。
2. 接続先は **`a.connectionByID(connID)` だけ**で引く（名前や host では引かない）。無ければ `Code:"not_found"`。
3. `cols<=0 || rows<=0` は 80x24 とする。上限は `cols<=1000, rows<=1000` に丸める。
4. `termMu` の下で次を行う。
   - 同じ `id` が既にあれば `Code:"duplicate_id"`。
   - シリアルなら `normalizeSerialPort(runtime.GOOS, port)` をキーに予約表 `serialOwners` を見て、他のセッションが持っていれば `Code:"port_in_use"`（Message に相手の接続先名）。空いていれば予約する。
   - `status:"connecting"` のプレースホルダを `terms[id]` に入れる（キャンセル用の `context.CancelFunc` を持たせる）。
5. ロック解放後に `a.sessionsChanged()`。
6. dial / open をする（`ConnectTimeout` の ctx は上の cancel から派生させる）。SSH は `RequestPty("xterm-256color", rows, cols, modes)` を**引数のサイズで**呼ぶ。stdout/stderr を出力バッファに繋いでから `Shell()` を呼ぶ（MOTD を落とさないため。frontend は id を先に登録しているので届く）。
7. 失敗時: プレースホルダと予約を外し、`lastErr[connID]` に記録し、`sessionsChanged()`。`passphraseMissingError` なら `Code:"needs_passphrase"`、途中で `CloseTerminal(id)` された場合は `Code:"canceled"`、その他は `Code:"connect_failed"`（Message は既存どおり `explainSSHError`）。**失敗時は terminal:exit を送らない。**
8. 成功時: `status:"open"`、`startedAt` を確定、`lastErr[connID]` を消し、`sessionsChanged()`。

戻り値: `ActionResult{OK:true, Message:"web-01 に接続しました", SessionID:id, ID:connID}`。失敗時も `SessionID:id` を入れる。

#### SendTerminalInput(id, data)

- セッションごとの入力 chan（容量 64）に `[]byte(data)` を入れ、セッションごとに1本の goroutine が順に stdin へ書く。
- chan が詰まっていれば最大 `terminalInputTimeout`（既定 2s）待ち、超えたら `Code:"input_timeout"`。
- 1回の data の上限は 64KiB。超えたら `Code:"input_too_large"`（frontend は 16KiB 以下に分割して送るので、通常は起きない）。
- 未知の id や終了済みなら `Code:"no_session"`。成功時は `ActionResult{OK:true}`（Message は空）。
- stdin への書き込みに失敗したら、そのセッションを `reason:"io_error"` で閉じる。

#### ResizeTerminal(id, cols, rows)

- `1<=cols<=1000 && 1<=rows<=1000` でなければ `Code:"invalid_size"`。未知の id は `Code:"no_session"`。
- `connecting` の間は値を保存するだけにし、PTY 確保後に最新値で `WindowChange` する。シリアルでは何もせず OK を返す。

#### CloseTerminal(id) / CloseTerminalsByConnection(connID) / CloseAllTerminals()

- すべて `reason:"user"` で閉じる。未知の id でも `OK:true`（冪等）。
- `connecting` のものは cancel して dial を中断させる（StartTerminal 側が `canceled` を返す）。exit はこのとき送らない。
- `CloseTerminalsByConnection` の Message は `"%d 件のターミナルを閉じました"`、`ID:connID`。
- user で閉じたときは `lastErr[connID]` を消す。

#### ListTerminalSessions()

`a.listTerminalInfos()` を返す。nil ではなく空スライスを返す。

#### DisconnectConnection(id)（B2）

1. `a.connectionByID(id)`。無ければ `OK:false, Code:"not_found"`。
2. `k := a.closeTerminalsForConnection(conn.ID)`（B1、reason user）
3. `j := a.cancelExecsForConnection(conn.ID, errDisconnected)`（B2）
4. `a.sessionsChanged()`
5. 戻り値 `ActionResult{OK:true, ID:conn.ID, Message: fmt.Sprintf("%s を切断しました（ターミナル %d 件・実行中コマンド %d 件を終了）", conn.Name, k, j)}`

確認ダイアログは frontend が出す（§3.4）。

### 1.3 型と json タグ

B1（`terminal.go`）:

```go
// TerminalInfo is the frontend-visible view of one live terminal tab.
type TerminalInfo struct {
	ID        string `json:"id"`
	ConnID    string `json:"connId"`
	ConnName  string `json:"connName"`       // 一覧を作る時点の現在名（リネームに追従）。無ければ開いた時点の名前
	Kind      string `json:"kind"`           // "ssh" | "serial"
	Port      string `json:"port,omitempty"` // シリアルのみ。設定値そのまま（正規化前）
	Baud      int    `json:"baud,omitempty"` // シリアルのみ
	StartedAt string `json:"startedAt"`      // RFC3339。connecting の間は StartTerminal を呼んだ時刻
	Status    string `json:"status"`         // "connecting" | "open"
}
```

終了したセッションは即座に map から消えるので、`Status` に終了状態は無い（終了は terminal:exit で伝える）。

B2（`app.go`）:

```go
// SessionSummary is the live runtime state of one saved connection (§5.4
// connState). It is derived on every call and never persisted.
type SessionSummary struct {
	ConnID         string `json:"connId"`
	ConnName       string `json:"connName"`
	Kind           string `json:"kind"`                     // "ssh" | "serial"
	ConnState      string `json:"connState"`                // 下表
	Shells         int    `json:"shells"`                   // status=open のタブ数
	Connecting     int    `json:"connecting"`               // status=connecting のタブ数
	Running        int    `json:"running"`                  // 実行中のコマンド数（App のカウンタ）
	Pending        int    `json:"pending"`                  // 承認待ちの件数
	ConnectedSince string `json:"connectedSince,omitempty"` // RFC3339。open タブの最も古い startedAt
	LastError      string `json:"lastError,omitempty"`
	Label          string `json:"label,omitempty"`          // シリアルのみ "/dev/cu.usbserial-10 @115200"
}

type DashboardData struct {
	Connections   []Connection     `json:"connections"`
	Requests      []CommandRequest `json:"requests"`
	AgentPolicies []AgentPolicy    `json:"agentPolicies"`
	MCP           MCPSettings      `json:"mcp"`
	StorageError  string           `json:"storageError"` // 既存
	Sessions      []SessionSummary `json:"sessions"`     // 保存済みの全接続先（a.conns の順）
	Terminals     []TerminalInfo   `json:"terminals"`
	RunningTotal  int              `json:"runningTotal"`
	SessionsSeq   uint64           `json:"sessionsSeq"`
}

// sessionsPayload is the sessions:changed event body; the field names match
// DashboardData so the frontend can merge either one the same way.
type sessionsPayload struct {
	Sessions     []SessionSummary `json:"sessions"`
	Terminals    []TerminalInfo   `json:"terminals"`
	RunningTotal int              `json:"runningTotal"`
	SessionsSeq  uint64           `json:"sessionsSeq"`
}

type ActionResult struct {
	// 既存フィールドはそのまま
	SessionID string `json:"sessionId,omitempty"` // 追加は B1（§1.6）
}
```

`ConnState` の決め方（B2 の `a.sessionSummaries()`。上から順に最初に当たったもの）:

| 条件 | SSH | シリアル |
|---|---|---|
| `Shells > 0` | `connected` | `in_use` |
| `Connecting > 0` | `connecting` | `connecting` |
| `LastError != ""` | `error` | `error` |
| それ以外 | `disconnected` | `disconnected` |

- 「接続中」は §5.1 どおりタブの有無だけで決める。`Running > 0` でも `ConnState` は変えない（サイドバーは `Running>0` の行も出す。§2.4）。
- `idle` は第5段のみなので使わない。シリアルの `available` / `missing` は接続テストの結果（lastTest）で表示し、ライブ判定は第4段に回す。

`SessionsSeq` は B2 が `atomic.Uint64` で持ち、`GetInitialData` と `sessions:changed` を組み立てるたびに1増やした値を入れる。frontend は手元より小さい seq を捨てる。

### 1.4 イベント

| イベント | ペイロード（JSON） | 送信元 | 規則 |
|---|---|---|---|
| `terminal:data` | `{"id":"TERM-…","data":"<base64>","dropped":123}`（`dropped` は省略可） | B1 | 最初の1バイトが溜まってから最大 16ms、または未送信が 32KiB に達した時点で送る。1イベントの生バイトは最大 32KiB（溜まっていれば連続で複数送る）。未送信分は 4MiB を上限とし、超えたら**古い側**を捨てて捨てたバイト数を次のイベントの `dropped` に足す。終了時は残りを全部送ってから exit を送る |
| `terminal:exit` | `{"id":"TERM-…","reason":"user","exitCode":0,"message":"…"}` | B1 | `sync.Once` で1セッション1回。`reason` は `user`\|`remote_exit`\|`conn_lost`\|`device_removed`\|`io_error`。`exitCode` は `remote_exit` のときだけ入れる（`*int`、`omitempty`）。`message` はバックエンド既定どおり日本語の補足で、空でもよい |
| `sessions:changed` | `sessionsPayload` | B1 が `a.sessionsChanged()` を呼び、B2 の実装が組み立てて送る | タブの追加・状態変化・削除、実行の開始・終了、DisconnectConnection のたびに送る。間引きはしない |

Go の型（B1、`events.go`）:

```go
const (
	terminalDataEvent    = "terminal:data"   // terminal.go から移す
	terminalExitEvent    = "terminal:exit"
	sessionsChangedEvent = "sessions:changed"
)

type terminalDataPayload struct {
	ID      string `json:"id"`
	Data    string `json:"data"`
	Dropped int    `json:"dropped,omitempty"`
}

type terminalExitPayload struct {
	ID       string `json:"id"`
	Reason   string `json:"reason"`
	ExitCode *int   `json:"exitCode,omitempty"`
	Message  string `json:"message"`
}

// emitEvent sends a Wails event, or hands it to the test hook a.emit.
func (a *App) emitEvent(name string, payload any)

// sessionsChanged asks B2's builder (a.onSessionsChanged) to publish the
// current sessions snapshot. Safe to call with no hook installed.
func (a *App) sessionsChanged()
```

`emitEvent` は `a.emit != nil` ならそれを呼び、そうでなく `a.ctx != nil` なら `wruntime.EventsEmit(a.ctx, name, payload)`、どちらも無ければ捨てる。

`reason` の判定（B1）:

- 先に `user` が設定されていれば常に `user`。
- SSH: `session.Wait()` が nil なら `remote_exit`（exitCode 0）。`*ssh.ExitError` なら `remote_exit`（exitCode は `ExitStatus()`）。それ以外（`*ssh.ExitMissingError`、EOF、net エラー）は `conn_lost`。
- シリアル: 読み込みエラーのとき、`serial.GetPortsList()` に正規化後のポート名が無ければ `device_removed`、あれば `io_error`。
- `user` 以外で終わり、その接続先に open のタブが残っていなければ `lastErr[connID]` に記録する（`remote_exit` は exitCode 0 なら記録しない）。

### 1.5 B1 ↔ B2 の境界（B2 が呼ぶ B1 の内部関数）

依存の向きは **B2 → B1 の一方向**にする。B1 は B2 のシンボルを1つも参照しない（`sessionsChanged` はフック経由）。

B1 が `terminal.go` に定義するもの:

```go
// terminalState is embedded in App; every field is guarded by termMu.
type terminalState struct {
	termMu       sync.Mutex
	terms        map[string]*terminalSession // key: session id
	serialOwners map[string]string           // normalized port -> session id
	lastErr      map[string]string           // connID -> last abnormal end / failed start
}

// terminalRuntime aggregates the live tabs of one connection.
type terminalRuntime struct {
	Open       int
	Connecting int
	Since      time.Time // earliest startedAt among open tabs; zero if none
	LastError  string
}

func (a *App) terminalRuntime() map[string]terminalRuntime   // key: connID。タブも lastErr も無い接続先はキーなし
func (a *App) listTerminalInfos() []TerminalInfo            // startedAt 昇順、同時刻は id 昇順。空スライス可
func (a *App) terminalCounts() map[string]int               // 既存の名前と型を維持。値は Open のみ（mcp_server.go の list_hosts が使う）
func (a *App) closeTerminalsForConnection(connID string) int // reason user。閉じた件数（connecting を含む）を返す
func (a *App) serialPortInUse(connID string) bool           // testSerialConnection 用（B1 内部で使う）
func normalizeSerialPort(goos string, name string) string   // darwin: /dev/tty.X → /dev/cu.X。windows: `\\.\` を外して大文字。その他はそのまま
```

`terminalState` の map は `NewApp` で作れないので（NewApp は B2 の担当）、B1 が `func (s *terminalState) ensureMaps()` を持ち、`termMu` を取った直後に毎回呼ぶ（遅延初期化）。

B1 のテスト用パッケージ変数（`terminal.go`）:

```go
var (
	terminalFlushInterval = 16 * time.Millisecond
	terminalFlushBytes    = 32 * 1024
	terminalPendingCap    = 4 * 1024 * 1024
	terminalInputTimeout  = 2 * time.Second
)
```

B2 が `app.go` / `approval.go` に定義するもの:

```go
// App に追加するフィールド（B1 の3か所とは別の行）
appCtx      context.Context
appCancel   context.CancelFunc
execMu      sync.Mutex
execs       map[uint64]runningExec // execMu で保護
execSeq     uint64                 // execMu で保護
execWG      sync.WaitGroup
runningTotal atomic.Int64          // §6.1「App の atomic カウンタ」
sessionsSeq atomic.Uint64

type runningExec struct {
	connID string
	cancel context.CancelCauseFunc
}

var (
	errDisconnected = errors.New("接続を切断したため中止しました")
	errShuttingDown = errors.New("アプリの終了により中断しました")
	shutdownExecGrace = 5 * time.Second // テストで短くする
)

func (a *App) trackExec(parent context.Context, connID string) (context.Context, func()) // executeSSH の入口で使う
func (a *App) runningCounts() map[string]int
func (a *App) cancelExecsForConnection(connID string, cause error) int
func (a *App) sessionSummaries() []SessionSummary // a.mu を取って conns/requests を写す → 解放 → terminalRuntime() → runningCounts()
func (a *App) notifySessionsChanged()             // sessionsPayload を組み立てて emitEvent。NewApp で a.onSessionsChanged = a.notifySessionsChanged
```

B2 のその他の変更:

- `NewApp` で `appCtx, appCancel = context.WithCancel(context.Background())`、`execs` の初期化、`onSessionsChanged` の設定。
- `executeSSH(parent, …)` の先頭で `ctx, done := a.trackExec(parent, conn.ID); defer done()`。`trackExec` は開始時と終了時に `runningTotal` を増減し、ロック解放後に `sessionsChanged()` を呼ぶ。
- `approve` の `context.Background()` を `a.appCtx` に替える。
- `finishRequest` は cancel の cause を見て、`errShuttingDown` なら `interrupted`、`errDisconnected` なら `failed` / `auto_failed`（`ErrorKind:"canceled"`）にする。
- `GetInitialData` は `a.mu` を持ったまま `terminalRuntime()` を呼ばないように組み替える（`a.mu` で写す → 解放 → `sessionSummaries` / `listTerminalInfos` / `runningTotal` / `sessionsSeq.Add(1)`）。
- `shutdown` の順序を §5.0 どおりにする: `stopMCPServer` → `a.appCancel` 相当として全実行を `cancelExecs…(errShuttingDown)` → `execWG` を最大 `shutdownExecGrace` 待つ → `a.CloseAllTerminals()` → `store.Close()`。
- `SaveConnection` はタブを閉じない（今もそうなので維持。「保存するたびにタブが切れる」を起こさない）。

### 1.6 B1 が `app.go` に入れる3か所（他は B2）

1. `ActionResult` の末尾に `SessionID string \`json:"sessionId,omitempty"\`` を足す。
2. `App` 構造体の `termMu sync.Mutex` と `term *terminalSession` の2行を、次の3行に置き換える。
   ```go
   terminalState
   emit              func(name string, payload any) // test hook; nil means Wails runtime
   onSessionsChanged func()                         // set by NewApp
   ```
3. `shutdown` の `a.CloseTerminal()` を `a.CloseAllTerminals()` に置き換える（シグネチャ変更でコンパイルを壊さないため。順序の組み替えは B2）。

同じ作業ツリーで並行する場合、B1 はまず §1.3〜1.5 の型・関数の宣言（中身は仮でよい）と上の3か所を最初に書き、B2 はそれを前提に書く。別ツリーで作業する場合も、シンボル名と型は本書のものを使う。

### 1.7 terminalSession（B1 の内部。参考）

```go
type terminalSession struct {
	id, connID, connName, kind, port string
	baud      int
	startedAt time.Time
	status    string // "connecting" | "open"
	cols, rows int   // connecting 中に受けた最新のサイズ
	cancel    context.CancelFunc
	input     chan []byte
	resize    func(cols, rows int) error
	out       *outputBuffer // 16ms/32KiB の合流、4MiB 上限
	closeOnce sync.Once
	reason    string
	done      chan struct{}
	closer    func()
}
```

`close(reason, exitCode, message)` は Once の中で、reason の確定 → closer → `out` の最終 flush → `terms` から削除（termMu）→ 予約表の解除 → exit の送信 → `sessionsChanged()` の順に行う。

---

## 2. frontend 側

### 2.1 共通の決まり

- **モジュール同士で import しない。** 例外は全員が `model.js`（F4）を import してよいことだけ。モジュール間の呼び出しは Integrator が作る `ctx`（§2.2）を通す。
- **Wails バインディング**は、各モジュールが今の main.js と同じく直接 import する。
  ```js
  import { StartTerminal, SendTerminalInput } from '../wailsjs/go/main/App';
  import { EventsOn } from '../wailsjs/runtime/runtime';
  ```
  新しいバインディングは Integrator が `wails generate module` で作る。それまでの単体テストは `vi.mock('../wailsjs/go/main/App', …)` と `vi.mock('../wailsjs/runtime/runtime', …)` で差し替える。
- **I18N**: 各モジュールは自分の辞書断片を `export const <NAME>_I18N = { ja: {...}, en: {...} }` で出す。キーには担当ごとの接頭辞を付け、他人の接頭辞を使わない。Integrator が `mergeI18n`（F4）で既存の main.js の辞書と合成する。キーの重複や ja/en の不一致があれば `mergeI18n` が例外を投げる。

  | 担当 | export 名 | キー接頭辞 |
  |---|---|---|
  | F1 | `TERMINAL_I18N` | `tabs.` |
  | F2 | `SIDEBAR_I18N` | `side.` |
  | F3 | `EDITOR_I18N` | `edit.` |
  | F4 | `MODEL_I18N` | `state.` |
  | approvals.js（フェーズ3） | `APPROVALS_I18N` | `appr.` |
  | agents.js（フェーズ3） | `AGENTS_I18N` | `agents.` |
  | servertabs.js（フェーズ4） | `SERVERTABS_I18N` | `srvtab.` |
  | shortcuts.js（フェーズ4） | `SHORTCUTS_I18N` | `keys.` |
  | credentials.js（フェーズ5） | `CREDENTIALS_I18N` | `cred.` |
  | main.js（i18n.js の BASE_I18N）の追加分 | — | `srvfilter.`、`term.code.*`、`term.save.*`、`term.settingsChanged`（既存の `cmd.` に `cmd.discardEditOnFilter`） |

- **t()**: 各モジュールは `ctx.t` か引数の `t` を使う。シグネチャは `t(key: string, vars?: Record<string, string|number>) => string` で、テンプレート中の `{name}` を vars で置き換える（Integrator が main.js の `t` を拡張する。置き換え関数 `interpolate` は F4 が作る）。モジュールは `state.lang` を直接読まず、言語が必要なら `ctx.lang()` を使う。
- **data-action の名前空間**: F1 は `term-*`、F2 は `side-*`、F3 は `edit-*`。既存の main.js の action 名と重ならない。統合で増えたもの（§5 も参照）:
  - approvals.js: `approve-command`、`approve-bypass-agent`、`reject-command`、`revoke-bypass`、`copy-command`、`[data-select-request]`（フェーズ2の main.js の名前を引き継いだ）
  - agents.js: `agents-*`（copy-token / hide-token / copy-config / reload / reissue / revoke-token / revoke-bypass / revoke-all）と `[data-agents-form="issue"]`
  - servertabs.js: `srvtab-select`、`srvtab-sub-activate`、`srvtab-new-shell`（servertabs.js が自前のリスナで処理。main.js の click 振り分けは `[data-action^="srvtab-"]` を見たら何もせず return）
  - main.js: `srvfilter-show-all`、`restart-terminal`（設定変更後の開いているタブを閉じて開き直す）、既存の `delete-command-request` / `close-delete-command` / `confirm-delete-command` と設定画面の action
  - credentials.js: `cred-delete-backups`。ダイアログ内は `data-cred-action` / `data-cred-dismiss`
  - F3 の追加: `edit-reset-hostkey`、`edit-serial-line`（change だけ）
- **HTML のエスケープ**は `model.js` の `escapeHtml` を使う。
- 各モジュールは、自分のハンドラが処理したイベントなら `true` を返す（Integrator はそこで処理を止める）。

### 2.2 main.js の state と ctx（Integrator が作る。各モジュールはこの形を前提にする）

```js
const state = {
  lang: 'ja' | 'en',
  active: 'connections' | 'commands' | 'terminal' | 'settings',
  dataGen: 0,            // ユーザー操作のたびに ++。古いポーリング結果を捨てる
  sessionsSeq: 0,        // 適用済みの最大 sessionsSeq
  data: {
    connections: [], requests: [], agentPolicies: [], mcp: {}, storageError: '',
    sessions: [],        // SessionSummary[]
    terminals: [],       // TerminalInfo[]
    runningTotal: 0,
  },
  editor: createEditorState(), // F3 が中身を所有する（§2.5）
  selectedRequestId: '',
  dirty: { settings: false, approval: false }, // connections は editor に移る
  issuedToken: '',
};

const ctx = {
  state,                                  // 読み取りは自由。書き込みは自分の担当分（F3 は state.editor）だけ
  t,                                      // (key, vars?) => string
  lang: () => state.lang,
  toast: showToast,                       // (message) => void。再描画はしない
  renderPage,                             // () => void。#page-root だけを描き直す
  renderSidebar,                          // () => void。#sidebar-status だけを描き直す
  refresh: refreshData,                   // ({ silent }) => Promise<void>
  bumpGen: () => ++state.dataGen,         // バックエンドを変える操作の直前に呼ぶ
  navigate,                               // async (page) => boolean。編集中ガードを通る。拒否されたら false
  openTerminal: (connId) => openTerminalTab(connId),   // F1
  promptPassphrase: (conn) => promptPassphrase(conn, ctx), // F3
  showConnection,                         // async (connId) => void。サイドバーの行クリック用（§2.4）
  sessionFor: (connId) => state.data.sessions.find((s) => s.connId === connId),
};
```

**統合後（フェーズ3〜5）に増えた state と ctx**（ここが現行の正。詳細は §5）:

```js
state.data.agentTokens = [];      // AgentToken[]（秘密は含まない）。approvals.js が承認省略の可否に使う
state.data.keychainPending = 0;   // 起動時にキーチェーンへ移せなかったパスワードの件数
state.approvals = createApprovalsState(); // approvals.js が所有（実行先の照会キャッシュ）
state.serverFilter = '';          // '' = すべて、他は Connection.id。setServerFilter / reconcileServerFilter だけが書く
state.serverFilterReady = false;  // 最初の refresh 成功までは false（保存値を検証しない）
state.reconnectNeeded = new Set();// 設定変更（reconnect_required / serial_mode_not_applied）で再接続が要る connId

ctx.copyText(text);               // クリップボード（textarea の代替つき）
ctx.serverFilter();               // 読み取り専用
ctx.setServerFilter(connId, { source, render });
ctx.onSelectionChange(id, { previousId, reason }); // F3 → main.js（一覧 → タブ）。reason 'delete' は無視
ctx.onConnectionSaved(result);    // F3 → main.js（シリアルの適用結果のトースト）
ctx.platform();                   // 'darwin' | 'windows' | 'linux' | ''（Wails の Environment）
ctx.recoverCredential(conn, result, { password }); // needs_password / keyring_unavailable の共通ダイアログ。true なら再試行してよい
ctx.startErrorMessage(result);    // StartTerminal の失敗コードの表示文言（EN は固定文言）
ctx.terminalKeyHandler;           // shortcuts.js のハンドラ。F1 が全 xterm に attachCustomKeyEventHandler する
```

画面の骨格（Integrator の `shell.js`）は起動時に1回だけ `#app` に描き、以後は部分ごとに描き直す。骨格に必ず次の要素を置く。

```html
<aside class="app-sidebar …">… <nav id="sidebar-nav"></nav> <div class="sidebar-status" id="sidebar-status"></div></aside>
<main class="app-main">… <div id="server-tabs" class="server-tabs-bar"></div> <div id="shell-subtabs" class="shell-subtabs-bar" hidden></div> <div id="page-root"></div> <div id="terminal-dock"></div> …</main>
```

`#server-tabs` / `#shell-subtabs`（フェーズ4）は `#page-root` の外にあり、servertabs.js が data-key で差分描画する（5秒ごとの再描画でノードとフォーカスを保つ）。

`#terminal-dock` は描き直さない。F1 はここに `#terminal-stack` を1回だけ置く。

> 仕様では `#terminal-stack` を「`#app` の外に置く常設コンテナ」としています。その目的は「再描画で xterm が壊れないこと」なので、本契約では **`#app` の innerHTML を起動時以外に書き換えない**ことで同じ効果を得ます。`#terminal-dock` は `#page-root` の外にあり、どの render でも置き換わりません。body 直下に fixed で重ねる方式は、スクロールとサイズの追従が壊れやすいため採りません。

### 2.3 F1: `frontend/src/terminal-tabs.js`

モジュール内の状態（§5.2 の形）:

```js
const terminal = {
  tabs: new Map(), // id -> { connId, label, status, xterm, fit, onDataDisp, sendQueue, pendingInput, lastSize, needsFit, pane }
  order: [],       // id の表示順
  activeId: '',
};
// status: 'starting' | 'open' | 'exited'
```

export:

```js
export const TERMINAL_I18N;
export function initTerminalTabs(ctx): void;            // 起動時に1回。#terminal-dock に #terminal-stack を作る。EventsOn('terminal:data') と EventsOn('terminal:exit') をここで1回だけ登録する。ResizeObserver を #terminal-stack に付ける
export async function restoreTerminalTabs(): Promise<void>; // ListTerminalSessions() で、手元に無い id のタブを作る。各タブに t('tabs.restored') の注記を書く
export async function openTerminalTab(connId: string): Promise<{ ok: boolean, id: string, code?: string, message: string }>;
export async function closeTerminalTab(id: string): Promise<void>;       // CloseTerminal(id) を呼び、xterm を dispose してタブを消す
export async function reconnectTerminalTab(id: string): Promise<void>;   // exited のタブを同じ connId で開き直す（§下記）
export function activateTerminalTab(id: string): void;
export function activateConnection(connId: string): boolean;             // その接続先のタブがあれば最後に使ったものを active にして true
export function reconcileTerminalTabs(terminals: TerminalInfo[]): void;  // バックエンドに無い open のタブを exited にする（exit の取りこぼし対策）。starting は対象外
export function setTerminalPageVisible(visible: boolean): void;          // #terminal-stack の hidden を切り替える。表示にしたら active を fit する
export function renderTerminalPage(state, t): string;                   // #page-root 用の HTML（タブ列と「新しいシェル」）
export function handleTerminalClick(event, ctx): Promise<boolean>;
export function handleTerminalChange(event, ctx): boolean;
export function terminalTabCount(connId?: string): number;
export function getTerminalState(): { tabs, order, activeId };          // テストと Integrator の参照用。書き換えない
export function newTerminalId(): string;                                 // 'TERM-' + crypto.randomUUID()。無い環境では getRandomValues で同形式を作る
```

振る舞い:

- **openTerminalTab(connId)**: (1) `await ctx.navigate('terminal')` が false なら `{ok:false, code:'canceled'}`。(2) id を発番し、`tabs` に `status:'starting'` で入れてから pane と xterm を作り、active にして fit し、cols/rows を得る。(3) `StartTerminal(id, connId, cols, rows)`。(4) 失敗したらタブを消す。`code==='needs_passphrase'` のときは `ctx.promptPassphrase(conn)` → 入力があれば `TestConnectionWithPassphrase(connId, pass)` → 成功なら1回だけ開き直す。それ以外の失敗は `ctx.toast(result.message)`。(5) 成功しても、その間に exit が届いて `exited` になっていれば `open` に戻さない。
- **ラベル**: `${connName} #${n}`。n は接続先ごとに1から増やし、減らさない。
- **terminal:data**: 知らない id は捨てる。`dropped>0` なら先に `\r\n\x1b[2m[${t('tabs.dropped', {n})}]\x1b[0m\r\n` を書く。base64 は Uint8Array にして `xterm.write`。
- **terminal:exit**: `reason==='user'` ならタブを消す（既に消えていれば何もしない）。それ以外は `status:'exited'`、`xterm.options.disableStdin = true`、`\r\n\x1b[33m[${t('tabs.exit.'+reason)}${message ? ': '+message : ''}${exitCode != null ? ' ('+exitCode+')' : ''}]\x1b[0m\r\n` を書き、タブに「再接続」を出す。
- **reconnectTerminalTab(id)**: xterm（スクロールバック）を残したまま新しい id を発番してエントリのキーを付け替え、`order` の位置も保つ。区切り線を書いてから `StartTerminal` を呼ぶ。
- **送信キュー**: `xterm.onData` で受けた文字列を `pendingInput` に連結する。送信中でなければ `sendQueue = sendQueue.then(flush)` で直列に送る。1回の送信は 16KiB 文字以下に切る。`open` 以外では送らない。
- **リサイズ**: ResizeObserver（無い環境では window の resize）で active のタブだけを fit し、`{cols, rows}` が `lastSize` と違うときだけ `ResizeTerminal(id, cols, rows)` を送る。非表示のタブには `needsFit = true` を付け、active にしたときに fit する。`#terminal-stack` が hidden の間は fit しない。
- `ResizeObserver` と `crypto.randomUUID` が無い環境（jsdom）でも例外にしない。

DOM とクラス名（F5 がスタイルを書く）:

```html
<!-- #terminal-dock の中（F1 が1回だけ作る） -->
<div id="terminal-stack" class="terminal-stack" hidden>
  <div class="term-pane is-active" data-term-id="TERM-…"></div> <!-- xterm を open する先。タブ1つにつき1つ -->
</div>

<!-- renderTerminalPage が返す #page-root の中身 -->
<div class="term-toolbar">
  <div class="term-tabs" role="tablist">
    <div class="term-tab is-active|is-exited|is-starting" role="tab" aria-selected="true">
      <button type="button" data-action="term-activate" data-term-id="…">web-01 #1</button>
      <button type="button" class="term-tab-reconnect" data-action="term-reconnect" data-term-id="…">再接続</button> <!-- exited のときだけ -->
      <button type="button" class="term-tab-close" data-action="term-close" data-term-id="…" aria-label="…"><i class="bi bi-x"></i></button>
    </div>
  </div>
  <div class="term-new">
    <select class="form-select form-select-sm" data-action="term-new-select">…保存済みの接続先…</select>
    <button class="btn btn-sm btn-primary" type="button" data-action="term-new">新しいシェル</button>
  </div>
</div>
<div class="term-empty">…タブが無いとき…</div>
```

`.term-pane` は active 以外 `display:none`。

最低限の辞書キー（JA/EN）: `tabs.newShell`（新しいシェル / New shell）、`tabs.close`、`tabs.reconnect`（再接続 / Reconnect）、`tabs.empty`（開いているターミナルはありません / No open terminals）、`tabs.restored`（以前の出力は表示できません / Earlier output is not available）、`tabs.dropped`（出力が多すぎるため {n} バイトを省略しました / Skipped {n} bytes of output）、`tabs.starting`、`tabs.exit.user`、`tabs.exit.remote_exit`（セッションが終了しました / Session ended）、`tabs.exit.conn_lost`（接続が切れました / Connection lost）、`tabs.exit.device_removed`（デバイスが外れました / Device removed）、`tabs.exit.io_error`（入出力エラー / I/O error）、`tabs.selectTarget`。

### 2.4 F2: `frontend/src/sidebar.js`

```js
export const SIDEBAR_I18N;
export function renderSidebarStatus(data, { t, lang, now }): string; // data は state.data。#sidebar-status の innerHTML
export function sessionRowAriaLabel(row, t, lang): string;
export function handleSidebarClick(event, ctx): Promise<boolean>;
```

- §6.1 のワイヤーを描く。上から: 言語切替（`data-lang="ja|en"` のボタン。処理は main.js が既存のまま行う）、「SSHセッション」節、「エージェント（MCP）」節、`storageError` があるときだけ保存先エラー、最後に既存の `.company-name`。
- 行は `summarizeSessions(data.sessions, { now, limit: 5 })`（F4）の結果をそのまま使う。行は `<button type="button" class="side-session-row" data-action="side-open-conn" data-conn-id="…" aria-label="…">`。
- 記号・アイコン・文字の3つで状態を示す。記号とラベルキーは F4 の `connStateSymbol` / `connStateLabelKey`、アイコンは SSH が `bi-hdd-network`、シリアルが `bi-usb-symbol`。
- 行の文言: 接続中は `web-01 12分 シェル2 · 実行1`（0件の項目は省く）、エラーは `▲ router-01 接続エラー`、シリアルは `◆ {label} 8分`。
- 「他 N 件 →」は `data-action="side-more"` で `ctx.navigate('connections')`。「承認待ち N →」は `data-action="side-approvals"` で `ctx.navigate('commands')`。
- 行クリック（`side-open-conn`）は `ctx.showConnection(connId)`。Integrator は「F1 の `activateConnection(connId)` が true なら terminal 画面へ、false なら connections 画面でその接続先を選ぶ」を実装する。
- MCP 行の状態は `mcp.status` が現状まだ日本語（`起動中`/`停止中`/`エラー`）なので、F2 がこれを `side.mcp.listening` / `side.mcp.stopped` / `side.mcp.error` に写す。アドレスは `${mcp.listenAddress}:${mcp.port}`、エージェント数は `mcp.connectedAgents`、承認待ちは `data.requests` の `status==='pending'` の件数。
- aria-label: JA は `web-01、接続中、12分、シェル2、実行中1件`、EN は `web-01, Connected, 12m, 2 shells, 1 running`。0 件の項目は省く。
- 「現在の接続先」と「SSHセッション: <status>」の2行は出さない（廃止）。

クラス名: `.side-section`、`.side-section-title`、`.side-summary`、`.side-summary-error`、`.side-session-row`（`.is-error`、`.is-serial`）、`.side-symbol`、`.side-name`、`.side-meta`、`.side-more`、`.side-empty`、`.side-mcp-line`、`.side-storage-error`。

辞書キー（§6.2 の EN を含める）: `side.sessionsTitle`（SSHセッション / SSH sessions）、`side.summary`（接続中 {connected} / 登録 {saved} / {connected} connected / {saved} saved）、`side.errors`（エラー {n} / {n} error）、`side.shells`（シェル{n} / {n} shells）、`side.running`（実行{n} / {n} running）、`side.ariaRunning`（実行中{n}件 / {n} running）、`side.more`（他 {n} 件 / +{n} more）、`side.empty`（接続中のセッションはありません / No active sessions）、`side.agentsTitle`（エージェント（MCP） / Agents (MCP)）、`side.mcp.listening`（待受中 / Listening）、`side.mcp.stopped`、`side.mcp.error`、`side.activeAgents`（アクティブなエージェント {n}（直近5分） / Active agents {n} (last 5 min)）、`side.pending`（承認待ち {n} / Awaiting approval {n}）、`side.storageError`（保存先エラー：設定を保存できません / Storage error: changes cannot be saved）。

### 2.5 F3: `frontend/src/editor.js`

接続先管理画面（§6.5）の全部を持つ。一覧、編集フォーム、削除確認、破棄確認、切断確認、パスフレーズのモーダルを含む。

```js
export const EDITOR_I18N;
export function createEditorState(): EditorState;
export function renderConnectionsPage(state, ctx): string;         // #page-root 用
export function handleEditorClick(event, ctx): Promise<boolean>;
export function handleEditorInput(event, ctx): boolean;
export function handleEditorChange(event, ctx): Promise<boolean>;
export function handleEditorSubmit(event, ctx): Promise<boolean>;
export function isEditorDirty(state): boolean;                     // connectionDirtyState(...) !== 'saved'
export function editorHasFocus(): boolean;                         // #connection-form の中に focus がある
export async function confirmLeaveEditor(ctx): Promise<boolean>;   // dirty でなければ即 true。dirty なら3択ダイアログ
export async function selectConnection(connId, ctx): Promise<boolean>; // ガードを通して選択を移す
export function reconcileEditor(state): void;                      // データ更新後に呼ぶ（選択中が消えたら先頭へ。draft は残す）
export function promptPassphrase(conn, ctx): Promise<string | null>;
```

```js
// state.editor（F3 だけが書き換える）
{
  selectedId: '',   // 選択中の保存済み接続先の id。新規 draft を選んでいる間は ''
  draft: null,      // null | Connection と同じ形。draft.id === '' なら新規
  returnId: '',     // 新規 draft をキャンセルしたときに戻る選択
  test: { ok: null, message: '', target: '' },
  alert: '',        // 保存失敗の文言。フォーム内に出す
  serialPorts: [],
}
```

- 選択と draft は**配列の index ではなく id で持つ**。draft は1つだけ。最初の入力で保存済みの値を写して作り、以後の input/change でフォームの値を draft に取り込む。保存済みの値（`state.data.connections`）は書き換えない。
- 再描画は draft から描くので、ポーリング・トースト・言語切替で入力は消えない。Integrator は `editorHasFocus()` が true の間だけ connections 画面の `renderPage` を見送る（キャレット位置を守るため）。
- ヘッダのバッジ: 保存軸は `connectionDirtyState(draft, saved)`（F4）を `state.save.*` で、接続軸は `ctx.sessionFor(id)?.connState ?? 'disconnected'` を `state.conn.*` で出す。connected のときは経過時間（`formatElapsed`）も付ける。最終テストは `lastTestLabel`（F4）。
- 一覧: draft がある行に未保存の印 `●`（`.dirty-mark`）を付ける。新規 draft は一覧の末尾に仮の行として出す。
- **保存**: dirty のときだけ押せる。`ctx.bumpGen()` → `SaveConnection(draft)`。成功したら `selectedId = result.id`、`draft = null`、`alert = ''`、`await ctx.refresh({silent:true})`。失敗したら draft を残し `alert = result.message`。
- **キャンセル**: dirty のときだけ押せる。新規 draft なら draft を捨てて `selectedId = returnId`。既存なら draft を捨てる。
- **接続テスト**: 未保存の値（draft、無ければ保存済みの値）で `TestConnectionConfig(conn, '')`。`code==='needs_passphrase'` なら `promptPassphrase` → `TestConnectionConfig(conn, pass)`。`test` 欄だけを更新し、connState は変えない。
- **ターミナルを開く**: `saveState==='saved'` のときだけ押せる。押せないときは `title` に `t('edit.saveFirst')`（先に保存してください / Save changes first）。押したら `ctx.openTerminal(id)`。
- **接続を切断**: `connState` が `connected` / `in_use` / `connecting` のときだけ押せる。確認ダイアログに `t('edit.disconnectConfirm', {k: shells + connecting, j: running})`（ターミナル {k} タブと実行中コマンド {j} 件も終了します / This also closes {k} terminal tab(s) and stops {j} running command(s)）を出し、OK なら `ctx.bumpGen()` → `DisconnectConnection(id)` → toast → refresh。タブは exit(user) で F1 が消す。
- **削除**: 既存の確認モーダルを移す。タブが開いていれば `edit.deleteWithTabs`（開いているターミナル {k} タブも閉じます）を足す。
- **破棄確認**（`confirmLeaveEditor`、行の選択、「新規」）: 3択（破棄して移動 / 保存して移動 / キャンセル）。「保存して移動」は保存が成功したときだけ true を返す。
- **新規**: 新規 draft が既にあればそれを選ぶだけにする。
- 種別の切替、認証方式の切替、ポートの再スキャンは draft だけを変える（今の `state.data.connections[index]` を上書きする処理は廃止）。一覧に無いボーレートは option に足す。
- モーダルは `document.body` 直下の `#editor-modal-root`（無ければ作る）に描く。`#app` の再描画で消えない。`promptPassphrase` も同じ所に出し、閉じたら `null` を返す。
- `collectConnectionForm` の考え方（DOM に無い項目は draft の値を使う）は引き継ぐ。`lastTest*` と `credentialSet` は送らなくてよい（バックエンドが無視する）。パスワードは空のまま保存すると既存の値を維持する。

クラス名: `.editor-header`、`.badge-save-saved`、`.badge-save-modified`、`.badge-save-new`、`.badge-conn-connected`、`.badge-conn-connecting`、`.badge-conn-disconnected`、`.badge-conn-error`、`.badge-conn-in_use`、`.dirty-mark`、`.editor-alert`、`.editor-actions`、`#editor-modal-root`。

### 2.6 F4: `frontend/src/model.js`（DOM を触らない純関数だけ）

```js
export const MODEL_I18N; // §6.3 の表を state.* で持つ
export function escapeHtml(value): string;
export function interpolate(template: string, vars?: object): string;   // '{n}' → vars.n。未定義は残す
export function mergeI18n(...dicts): { ja: object, en: object };         // 重複キーと ja/en の不一致で throw
export function summarizeSessions(sessions, { now = Date.now(), limit = 5 } = {}): SessionsView;
export function connectionDirtyState(draft, saved): 'saved' | 'modified' | 'new';
export function editableSnapshot(conn): object;                         // 比較用に正規化した編集可能項目
export function saveStateLabelKey(code): string;                        // 'state.save.saved' など
export function connStateLabelKey(code): string;                        // 'state.conn.connected' など
export function connStateSymbol(code): '●' | '○' | '▲' | '◆';
export function requestStateLabelKey(code): string;                     // 'state.req.pending' など
export function formatElapsed(ms: number, lang: 'ja' | 'en'): string;
export function lastTestLabel(conn, t): string;                         // 最終テスト: 成功 10:32 / 失敗 10:32 / 未確認
```

`summarizeSessions` の戻り値:

```js
{
  connected: number, // connState が connected または in_use の件数
  saved: number,     // sessions.length
  errors: number,    // connState === 'error' の件数
  running: number,   // running の合計
  rows: [{ connId, name, kind, connState, symbol, shells, connecting, running, pending, elapsedMs, label }], // 最大 limit 件
  more: number,      // 表示対象のうち rows に入らなかった件数
}
```

- 表示対象は `connState` が `connected` / `in_use` / `connecting` / `error` の行、または `running > 0` の行。
- 並び順はエラー → `running>0` → `connectedSince` の新しい順 → name。`elapsedMs` は `now - Date.parse(connectedSince)`、無ければ `null`。
- 記号は connected → `●`、connecting と disconnected → `○`、error → `▲`、in_use → `◆`。

`connectionDirtyState(draft, saved)`:

- `draft == null` → `'saved'`。`draft.id === ''` → `'new'`。`saved` が無い（削除された）→ `'modified'`。
- 比較する項目: name, type, host, port, user, authMethod, credential, serialPort, baudRate, connectTimeout, commandTimeout, tags, description。文字列は trim、数値は `Number()`、tags は trim して空を除いた配列で比べる。
- 種別が Serial なら host/user/authMethod/credential は比べない。SSH なら serialPort/baudRate は比べない。
- パスワード認証では、保存済み側に値が返ってこないので「draft.credential が空でない」ことを変更とみなす。

`formatElapsed`: 1分未満は `1分未満` / `<1m`、1時間未満は `12分` / `12m`、24時間未満は `1時間5分` / `1h 5m`、それ以上は `2日` / `2d`。

`MODEL_I18N` のキー（§6.3 の文言そのまま）: `state.save.{saved,modified,new}`、`state.conn.{connected,connecting,idle,disconnected,error,in_use,available,missing}`、`state.test.{label,ok,failed,never}`、`state.req.{pending,running,succeeded,failed,rejected,expired,interrupted,auto_running,auto_succeeded,auto_failed}`。

`package.json` には `"test": "vitest run"` と、devDependencies に vite 8 と組める vitest と jsdom を足す。`vitest.config.js` は `test.environment = 'jsdom'`、`test.include = ['src/**/*.test.js']`。

### 2.7 F5: `frontend/src/style.css`

- サイドバーを flex にする: `.app-sidebar` を縦の flex、`.sidebar-wrapper` の `padding-bottom: 13.75rem` を外す、`.sidebar-status` の `position:absolute` を外して `margin-top:auto`。狭い画面向けの `@media (max-width: 991.98px)` の上書きもそれに合わせる。
- §2.3〜2.5 に挙げたクラス名のスタイル（`#terminal-stack`/`.terminal-stack`、`.term-pane`（active 以外は `display:none`、高さは今の `.xterm-host` と同じ `62vh` / `min-height:340px`）、`.term-pane .xterm { height:100% }`、`.term-toolbar`、`.term-tabs`、`.term-tab`（`.is-active`、`.is-exited`、`.is-starting`）、`.term-tab-close`、`.term-tab-reconnect`、`.term-new`、`.term-empty`、`.side-*`、`.badge-save-*`、`.badge-conn-*`、`.dirty-mark`、`.editor-alert`、`.editor-actions`、`#editor-modal-root`）。
- 状態は色だけに頼らない（記号と文字が必ず並ぶ前提で、色は補助）。
- 不要になる `.xterm-host` と `.status-line` / `.status-value` / `.status-badge` は Integrator の統合が終わるまで残す（main.js がまだ使っているため）。

---

## 3. Integrator が行うこと（並列作業の後に1人で）

1. B1・B2 の Go がビルドできることを確認し、`wails generate module` を実行する。`App.d.ts` が §1.2 の行と一致すること、`models.ts` に `TerminalInfo` と `SessionSummary` があることを確認する。
2. `main.js` から次を削除する: xterm と FitAddon の import、`term` / `fitAddon` / `termWired`、`ensureTerminal` / `mountTerminal` / `fitTerminal` / `b64ToBytes` / `teardownTerminal`、`EventsOn('terminal:data'|'terminal:exit')`、window の resize リスナ、`renderTerminal` / `terminalConnectionDetail`、`state.terminal`、`terminal-connect` / `terminal-disconnect` / `terminal-select-connection` の処理。
3. **画面遷移時の `teardownTerminal` を削除する**（nav クリックの `if (state.active === 'terminal' && target !== 'terminal') await teardownTerminal()`）。
4. **`protectTerminal` を撤廃する。** ターミナル画面でも `renderPage` してよい（xterm は `#terminal-dock` にあり、`#page-root` の外）。
5. `render()` を分割する: 起動時に1回だけ `renderSkeleton()`（`shell.js`）。以後は `renderHeader()`（タイトルとパンくず）、`renderNav()`（承認待ちバッジ）、`renderSidebar()`（`#sidebar-status.innerHTML = renderSidebarStatus(state.data, {t, lang, now: Date.now()})`）、`renderPage()`（`#page-root` だけ）。`renderAll()` はこの4つを呼ぶ。言語切替は `renderAll()`。
6. `renderPage()` の振り分け: connections → `renderConnectionsPage`（F3）、terminal → `renderTerminalPage`（F1）、commands / settings は既存の関数。描いた後に `setTerminalPageVisible(state.active === 'terminal')`。
7. `refreshData` を書き直す:
   - `inFlight` なら何もしない（重ねて投げない）。
   - 開始時に `const gen = state.dataGen` を控え、応答時に `state.dataGen !== gen` なら捨てる。
   - `sessions` / `terminals` / `runningTotal` は `next.sessionsSeq >= state.sessionsSeq` のときだけ取り込む。
   - 取り込み後に `reconcileEditor(state)` と `reconcileTerminalTabs(state.data.terminals)` を呼ぶ。
   - `renderSidebar()` と `renderNav()` は**常に**呼ぶ。`renderPage()` は、connections 画面で `editorHasFocus()` のとき、settings で `dirty.settings` のとき、commands で `dirty.approval` のときだけ見送る。
8. `EventsOn('sessions:changed', (p) => …)` を1回だけ登録する。seq が新しければ `state.data` に `sessions` / `terminals` / `runningTotal` を取り込み、`renderSidebar()`（ターミナル画面ならタブ列のため `renderPage()` も）を呼ぶ。
9. I18N を合成する: `const I18N = mergeI18n(BASE_I18N, TERMINAL_I18N, SIDEBAR_I18N, EDITOR_I18N, MODEL_I18N)`。`t(key, vars)` は `interpolate` を使う。不要になった `status.*` / `ssh.*` / `term.*` / `conn.*` のキーは、使っていないことを grep で確かめてから消す。
10. `ctx`（§2.2）を作り、`navigate(page)` には「connections から離れるときは `await confirmLeaveEditor(ctx)`」と「settings から離れるときは `issuedToken = ''`」を入れる。
11. click / input / change / submit を振り分ける。順序は `data-lang` → `#sidebar-status` 内なら `handleSidebarClick` → `[data-nav]`（`navigate`）→ 表示中の画面のモジュールのハンドラ → 既存の commands / settings の処理。
12. 接続先関連の state（`selectedConnection`、`dirty.connections`、`passphrase`、`deleteConnection`、`connectionTest`、`serialPorts`）と、その処理とモーダルを main.js から削除する（F3 に移った）。`shell()` 末尾の storageError のアラートは、サイドバーの表示（F2）に置き換えて削除する。
13. 起動順序: `renderSkeleton()` → `initTerminalTabs(ctx)` → `await refreshData()` → `await restoreTerminalTabs()` → `renderAll()` → `setInterval(refreshData({silent:true}), 5000)`。
14. `shell.js` の `renderSkeleton` に JA / EN のスナップショットテスト（`shell.test.js`）を書く。
15. `.github/workflows/test.yml` の frontend ジョブに `- run: npm test` を足す（`npm ci` と `npm run build` の間）。
16. `go vet ./...`、`go test -race -count=1 ./...`、`npm test`、`npm run build` が通ることを確認する。F5 が残した旧クラスのうち使われなくなったものを消す。

---

## 4. テスト計画

### 4.1 Go（すべて `-race`。並行系は `-count=20` でも回す）

`sshtest_test.go`（B1 の追加）:

- `pty-req`（サイズを記録）、`window-change`（記録）、`shell` に応じる。shell は開始直後に `welcome <user>\r\n`（MOTD 代わり）を書き、その後は行単位で受けて既存の `run` と同じコマンド表で応答する（`echo` / `exit n` でチャネルを閉じる / `yes`）。
- `KillAll()`: サーバー側の全 net.Conn を閉じる（listener は閉じない）。
- `PtySizes()`: 受けた pty-req / window-change を返す。

`terminal_test.go`（B1）。`app.emit` を差し替えて `(name, payload)` を記録する fake を使う:

| テスト | 確認すること |
|---|---|
| `TestTerminalOutputIsolatedByID` | 同じサーバーに2タブ、別のサーバーに1タブ。各タブに `echo A` / `echo B` / `echo C` を送り、terminal:data を id ごとに結合すると、それぞれ自分の文字列だけを含む |
| `TestTerminalMOTDNotLost` | StartTerminal が戻る前に届いた `welcome` が、その id の data に含まれる |
| `TestTerminalInitialPtySize` | `StartTerminal(id, c, 132, 40)` で pty-req が 132x40 になる。Resize で window-change が届く |
| `TestKillAllExitsOnlyThatServer` | サーバーA に2タブ、B に1タブを開き、A を KillAll。A の2タブにだけ `reason:"conn_lost"` の exit が1回ずつ届き、B のタブは ListTerminalSessions に残る |
| `TestCloseOneTabKeepsOthers` | 2タブのうち1つを CloseTerminal。exit は `user` で1回だけ、もう一方は入力を送ると応答する |
| `TestRemoteExitCode` | `exit 3` で `reason:"remote_exit", exitCode:3` |
| `TestDuplicateAndInvalidID` | 同じ id で2回 StartTerminal → `duplicate_id`。`foo` → `invalid_id` |
| `TestCloseWhileConnecting` | 無応答サーバー（`newSilentServer`）に StartTerminal 中に CloseTerminal → `canceled` で速やかに戻り、exit は送られない |
| `TestOutputCoalescing` | `terminalFlushInterval` を伸ばして `yes` を流す。各 data の生バイトは 32KiB 以下 |
| `TestOutputPendingCap` | emit をブロックさせ、`terminalPendingCap` を小さくして流す。再開後に `dropped>0` の data が届き、メモリが上限を超えない |
| `TestInputOrder` | 1000 回の SendTerminalInput（並行に投げない）がそのままの順でサーバーに届く |
| `TestExitOnceUnderRace` | KillAll と CloseTerminal を同時に起こしても exit は1回 |
| `TestNormalizeSerialPort` | 表駆動: `darwin,/dev/tty.usbserial-10 → /dev/cu.usbserial-10`、`windows,\\.\com3 → COM3`、`linux,/dev/ttyUSB0` は不変 |
| `TestSessionsChangedHook` | `onSessionsChanged` を数えるフックにすると、open と close で呼ばれる |

`app_sessions_test.go`（B2）:

| テスト | 確認すること |
|---|---|
| `TestDashboardSessionsShape` | 接続先3件（SSH2・シリアル1）のとき `sessions` が3件、タブが無ければ全部 `disconnected`、`terminals` は空で nil ではない（JSON で `[]`） |
| `TestConnStateFromTabs` | 1タブ開くと `connected`、`shells:1`、`connectedSince` が入る。閉じると `disconnected` |
| `TestConnStateErrorAfterConnLost` | KillAll 後は `error` と `lastError`。もう一度開くと `connected` に戻る |
| `TestRunningCounter` | `sleep 1` を実行している間は `running:1`、`runningTotal:1`。終われば 0 |
| `TestDisconnectClosesTabsAndCancelsExec` | 2タブと `sleep 5` の実行中に DisconnectConnection。Message に件数（2 と 1）、タブ2つに `reason:"user"` の exit、実行は1秒以内に `failed`・`error_kind:"canceled"` |
| `TestDisconnectDoesNotTouchOtherConnection` | 別の接続先のタブと実行は残る |
| `TestShutdownInterruptsRunning` | `shutdownExecGrace` を短くして shutdown すると、実行中の要求が `interrupted` になる |
| `TestSessionsSeqMonotonic` | GetInitialData と sessions:changed の seq が単調に増える |
| `TestSaveConnectionKeepsTabs` | タブを開いたまま SaveConnection（リネーム含む）してもタブが残り、TerminalInfo.connName が新しい名前になる |
| `TestGetInitialDataNoDeadlock` | GetInitialData とタブの開閉を並行に回しても `-race` で問題が出ず、デッドロックしない |

### 4.2 frontend（vitest + jsdom）

`model.test.js`（F4）。表駆動にする:

- `summarizeSessions`: 空 / 全部 disconnected（rows 0、connected 0）/ エラーが先頭に来る / `running>0` が次に来る / `connectedSince` の新しい順 / 6件で `rows.length===5, more===1` / シリアルの in_use を connected に数える / 未接続でも running がある行は出る。
- `connectionDirtyState`: draft null → saved / id 空 → new / 同値（数値の文字列 `"22"` と `22`、tags の空白違い）→ saved / name を変更 → modified / SSH で serialPort だけ違う → saved / パスワード認証で credential を入力 → modified / saved が無い → modified。
- `formatElapsed`（JA/EN の境界値: 59s、60s、59m、60m、23h59m、24h）、`interpolate`、`mergeI18n`（重複キーで throw、ja/en の不一致で throw）、`connStateSymbol`。

`sidebar.test.js`（F2）: `sessionRowAriaLabel` の JA/EN、`renderSidebarStatus` の JA/EN スナップショット（空のとき、6件で「他 1 件」、storageError あり）、記号とラベルが必ず両方出ること。

`terminal-tabs.test.js`（F1）。`@xterm/xterm`、`@xterm/addon-fit`、Wails バインディングと runtime を mock する:

- EventsOn は `initTerminalTabs` を2回呼んでも各イベント1回しか登録されない。
- 知らない id の terminal:data は捨てる。
- 送信キュー: StartTerminal 済みのタブで onData を3回続けて起こしても、`SendTerminalInput` は前の Promise が解決してから呼ばれ、内容の順序が保たれる。
- `exit(user)` でタブが消え、`exit(conn_lost)` では残って `exited` になる。
- StartTerminal の解決前に exit が届いたら `exited` のまま。
- `needs_passphrase` で `ctx.promptPassphrase` が呼ばれ、成功後に1回だけ再試行する。
- Resize は cols/rows が変わったときだけ送る。
- `restoreTerminalTabs` で ListTerminalSessions のぶんのタブができる。

`editor.test.js`（F3）: 入力 → `renderConnectionsPage` をし直しても値が残る / 保存成功で選択が返ってきた id に移り draft が消える / リネーム後に一覧の行が増えない / 新規を2回押しても draft は1つ / キャンセルで直前の選択に戻る / 「ターミナルを開く」は modified のとき disabled で title がある / 切断ダイアログに k と j が入る。

`shell.test.js`（Integrator）: `renderSkeleton` の JA / EN スナップショット。

### 4.3 §7 の受け入れ条件との対応

| 受け入れ条件 | 確かめるテスト |
|---|---|
| 2タブ＋別サーバー1タブで画面を移っても件数が変わらない | Go: `TestTerminalOutputIsolatedByID`（ListTerminalSessions の件数）、Integrator の手動確認（nav で teardown しない） |
| 1タブを閉じても他は生きている | `TestCloseOneTabKeepsOthers` |
| MOTD を取りこぼさない | `TestTerminalMOTDNotLost` |
| ターミナル画面でもサイドバーが更新され、xterm の DOM が同じ | Integrator: refreshData がサイドバーを常に描くこと、`#terminal-dock` を描き直さないこと（`terminal-tabs.test.js` で pane の要素が同一であることも確認する） |
| 入力中の値がトーストや言語切替で消えない | `editor.test.js` |
| リネームしても行が増えず、選択が追従する | `editor.test.js`、`TestSaveConnectionKeepsTabs` |

---

## 5. 統合時の追記（フェーズ3〜5、Integrator）

フェーズ3（approvals.js / agents.js）、フェーズ4（servertabs.js / shortcuts.js / serverview.js / serverfilter.js、シリアル）、フェーズ5のキーチェーン（credentials.js）を main.js に結線したときに、本書の名前に足したものです。§2.1・§2.2 の表にも反映済み。

| 項目 | 追加・変更 | 根拠 |
|---|---|---|
| モジュールの import | main.js 側の補助モジュール `serverview.js`（純関数）と `credentials.js`（共通ダイアログと設定画面の通知）は main.js だけが import する。F1〜F3 と approvals.js / agents.js は互いに import しない（`model.js` は例外のまま） | phase4-ui-integration §4.1 |
| 承認キュー | main.js から approvals.js に移した（`renderCommandsPage(root, ctx, view)`、`handleApprovalsClick(event, ctx, view)`、`handleApprovalsInput`）。`view` は `deriveServerView(state, getTerminalState())`。削除モーダルと `srvfilter-show-all` は main.js に残る | approvals.js の報告 |
| 設定画面 | `<form id="mcp-form">` の後ろに `#agents-root`（agents.js の `mountAgentsSection`）、先頭に `#keychain-notices`（credentials.js の `mountKeychainNotices`）。`pageIsBusy()` の settings は `dirty.settings || agentsSectionBusy()`。設定画面を離れるときに `forgetIssuedAgentToken()` | agents.js の報告 |
| ターミナル画面 | `#page-root` には active なタブの操作（再接続・閉じる・終了理由）と、「すべて」のときだけ `renderTerminalNewShell`。タブ列は `#shell-subtabs`。フェーズ2の `renderTerminalPage` は main.js から呼ばない。`#terminal-stack` は絞り込み先にタブが無ければ隠す（`view.stackVisible`） | phase4 §2.4 |
| F1 の追加 | `ctx.terminalKeyHandler` の取り付け、`setTerminalServerFilter` / `renderTerminalNewShell` / `renderTerminalEmpty`、タブの `exitReason`（device_removed の表示用）、`needs_password` / `keyring_unavailable` で `ctx.recoverCredential` を1回だけ呼んで再試行 | phase4 §6 C3、phase5 §8 |
| F3 の追加 | `selectConnection(id, ctx, { notify })`、`ctx.onSelectionChange`、`ctx.platform`、パスフレーズのモーダルに「キーチェーンに保存」（既定 OFF。チェック時は `TestConnectionWithPassphraseSave(id, pass, true)`）、接続テストの `needs_password` / `keyring_unavailable` で `ctx.recoverCredential`、保存の `keyring_unavailable` で入力済みのパスワードを「このセッションだけ使う」に回す | phase4 §2.3、phase5 §5.4 / §8 |
| 起動 | `renderSkeleton()` → `initTerminalTabs(ctx)`（`ctx.terminalKeyHandler` は設定済み）→ `renderAll()` → `setInterval(pollData, 5000)` → `await refreshData()`（最初の成功で `reconcileServerFilter({reason:'init'})` とタブの復元）→ `renderAll()`。main.js は import 時に起動する形のままにした（テストは `vi.resetModules()` で読み直す） | phase4 §6 C1 からの変更 |
| Wails の追加 API | `GetApprovalTarget`、`GetHostKeyInfo` / `ResetHostKey`、`ListAgentTokens` ほか（フェーズ3）、`UseCredentialForSession(id, secret)`、`TestConnectionWithPassphraseSave(id, passphrase, save)`、`PlaintextBackups()`、`DeletePlaintextBackups(paths)`（フェーズ5）。`DashboardData.keychainPending`、`TerminalInfo.serialMode` | `wails generate module` で再生成済み |

