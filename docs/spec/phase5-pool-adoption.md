# フェーズ5 プール組み込み手順書（terminal.go / ssh_exec.go の載せ替え）

この文書は design-v1.md §5.0（所有権・ロック規約）、§5.1「第5段（任意）：プール仕様」、§7 フェーズ5 と、docs/spec/phase2-contract.md（以下「P2契約」）を前提に、**新規ファイルだけで作ったプール（`ssh_pool.go`）を本体に組み込むときの差分方針**を決めるものです。プール本体の実装はこの文書の範囲外で、ここでは「本体側がプールに何を求め、どこをどう書き換え、どの順で切り替え、何を測ってから入れるか」を決めます。

- 前提の状態: P2契約が実装・統合済み（`terminalSession` は1タブ専用の `ssh.Client` を所有、`trackExec` / `cancelExecsForConnection` / `shutdown` の §5.0 順序が入っている）。基準コミット `a6f2706` の時点ではまだ単一タブなので、組み込みは **P2統合の後** に行う。
- フェーズ5 の位置づけ: §7 のとおり「任意。実測で必要になってから」。**§8 の計測で基準を満たさない限り、プールは既定 OFF のまま本体に入れない**。
- 名前の扱い: §1 の API 名は組み込み側から見た要求です。`ssh_pool.go` の実装と名前が食い違った場合は実装側の名前を正とし、本書の表を置換します（意味・所有権・エラー契約は本書を正とする）。

---

## 0. 変更範囲

| ファイル | 組み込み時の変更 | 担当の目安 |
|---|---|---|
| `ssh_pool.go`（新規、フェーズ5で作成済み） | 変更しない（不足があれば §1 の契約に合わせて実装側で直す） | プール担当 |
| `ssh_pool_metrics.go`（新規） | 計測（§8）。**プールより先に、プール OFF のまま入れる** | 計測担当 |
| `app.go` | `App` に `pool` / `poolMode` を追加、`NewApp` で生成、`shutdown` に手順4、`DisconnectConnection` に `pool.CloseConnection`、`SaveConnection` / `DeleteConnection` に drain / 即時切断 | B2 相当 |
| `terminal.go` | SSH タブの dial を `pool.Acquire` に置換、`closer` を `session.Close + lease.Release` に限定、`reason` 判定の変更 | B1 相当 |
| `ssh_exec.go` | `runSSHCommand` を「client を受け取る実行部」と「取得部」に分割、cancel 時に client を閉じない、再試行を NewSession 段階に限定 | B2 相当 |
| `approval.go` | `executeSSH` が `poolMode` で経路を選ぶだけ | B2 相当 |
| `mcp_server.go` | 変更なし（`list_hosts` のランタイム項目は P2 のまま。プール由来の値は出さない。§9-U5） | — |

触らない: `storage.go`、`httpguard.go`、frontend（状態の語彙は変えない。§6.3）。

---

## 1. 組み込み側がプールに求める公開 API（契約）

```go
// dial は関数注入。本番は openSSHClient を包む。テストは sshtest_test.go のサーバーに繋ぐ関数を渡す。
type sshDialFunc func(ctx context.Context, conn Connection) (*ssh.Client, error)

type leaseUse int   // leaseTerm | leaseExec
type poolOrigin int // originManual（人間がタブを開いた）| originAuto（MCP が開いた）

func newSSHPool(dial sshDialFunc, opts sshPoolOptions) *sshPool

// Acquire は connID の健全な client を1本貸し出す。必要なら dial する（singleflight）。
// ctx は「待つ側」の ctx。dial 自体は pool の ctx で走り、待つ側が諦めても完了すれば pool に入る。
func (p *sshPool) Acquire(ctx context.Context, conn Connection, use leaseUse, origin poolOrigin) (*sshLease, error)

// NewSession はチャネル用セマフォを取ってから client.NewSession を goroutine+ctx で包んで呼ぶ。
// OpenChannelError なら待つ/overflow、EOF・net エラーなら evict して1回だけ取り直す。
// 再試行してよいのはこの関数の中だけ（§5.1）。
func (l *sshLease) NewSession(ctx context.Context) (*ssh.Session, error)

// Release はセマフォと activeTerm/activeExec を返す。冪等。client は閉じない。
func (l *sshLease) Release()

// Broken は lease の client が pool によって死亡判定（keepalive 2連続失敗・Wait 復帰・evict）されたか。
// ターミナルの exit reason 判定に使う。
func (l *sshLease) Broken() bool

func (p *sshPool) Drain(connID string)            // fp 変更・保存時。使用中は使い終わってから閉じる
func (p *sshPool) CloseConnection(connID string) int // Delete と人間の「切断」。即時に閉じる。閉じた client 数
func (p *sshPool) CloseAll()                     // shutdown 手順4。以後の Acquire は errPoolClosed
func (p *sshPool) Stats() sshPoolStats           // §8.3 の事後計測用。ロックを取って写すだけ

var errPoolClosed = errors.New("接続プールは終了しています")
```

組み込み側が依存するエラー契約:

1. dial のエラーは `%w` で包んで返す。`errors.As(err, *passphraseMissingError)` と `explainSSHError` がそのまま効くこと（P2契約の `needs_passphrase` を壊さない）。
2. エラーはキャッシュしない（パスフレーズ入力直後の再試行が即成功すること）。
3. 待つ側の `ctx` が終わったら `ctx.Err()` を返す（P2契約の `canceled` 判定に使う）。
4. `Acquire` / `NewSession` / `Release` / `Drain` / `CloseConnection` / `CloseAll` は `pool.mu` を解放してから戻り、`a.mu`・`a.termMu`・`a.execMu` を一切取らない（§5.0「2つ同時に持たない」を、呼び出し側が任意のロック外から呼べる形で満たす）。
5. pool はコールバックで App を呼ばない。状態の変化は呼び出し側が `Broken()` や `Stats()` で引く（逆向きの依存を作らない）。

`App` 側に足すもの（`app.go`）:

```go
pool     *sshPool  // poolMode == poolOff のとき nil
poolMode poolMode  // 起動時に1回だけ決める。実行中に変えない（§7）

type poolMode int // poolOff | poolExec | poolAll
func parsePoolMode(v string) poolMode // SSH_GETE_POOL: "" / "off" → poolOff、"exec"、"all"。不明値は poolOff + log
func (a *App) dialSSH(ctx context.Context, conn Connection) (*ssh.Client, error) // 下記
```

`dialSSH` は今の `openSSHClient(ctx, conn, strictHostKey)` を包む注入用の関数で、`strictHostKey` と `KeyPassphrase` を **`a.mu` の外で先に読み取って** 呼ぶ。P0 の差し替え口 `a.dial` があればそれを `newSSHPool` に渡す。

---

## 2. 所有権の移動

| 資源 | P2（現在の契約） | フェーズ5（poolAll） |
|---|---|---|
| `ssh.Client`（ターミナル用） | `terminalSession` が所有。`closer` が `session.Close` と `client.Close` | **pool が所有**。`terminalSession` は `*sshLease` を持つだけ |
| `ssh.Client`（MCP 実行用） | `runSSHCommand` が所有（1コマンド1接続、defer Close） | **pool が所有**（origin auto） |
| `ssh.Session` | 各利用者 | 各利用者（変わらない） |
| チャネル枠（セマフォ） | なし | lease が借り、`Release` で返す |
| シリアルポート | `terminalSession` | `terminalSession`（変わらない。pool はシリアルを扱わない） |
| 接続テストの client | `testSSHConnection` が dial して即 Close | **変えない**（pool を通さない。§4.3） |

**ターミナルの closer は `session.Close` と `lease.Release` だけにする。** `client.Close` を呼んでよいのは pool だけ（§5.0）。この不変条件はコードレビューの観点にし、`grep -n "client.Close\|\.Close()" terminal.go ssh_exec.go` で pool 経路に client の Close が残っていないことを確認する（poolOff 経路の Close は残る）。

`terminalSession` の変更（P2契約 §1.7 に対して）:

```go
lease *sshLease // poolAll で開いた SSH タブだけ非 nil。poolOff と serial は nil
```

`closer` は開いたときのモードで確定させる（クロージャに捕まえる）。こうしておけば、同じプロセス内に poolOff で開いたタブと poolAll で開いたタブが混在しても（テストで `poolMode` を差し替えた場合など）、閉じ方を取り違えない。

---

## 3. terminal.go の差分方針

P2契約 §1.2 の `StartTerminal` の手順6（dial / open）だけを差し替え、1〜5・7・8 の手順と戻り値・Code は変えない。

### 3.1 開くとき

```go
// poolAll のときだけ。poolOff は P2 のコードをそのまま通す。
lease, err := a.pool.Acquire(dialCtx, conn, leaseTerm, originManual)
if err != nil { /* P2 の失敗処理（needs_passphrase / canceled / connect_failed） */ }
session, err := lease.NewSession(dialCtx)
if err != nil { lease.Release(); /* connect_failed */ }
// RequestPty → StdinPipe → Stdout/Stderr を出力バッファへ → Shell は P2 と同じ
// 途中で失敗したら session.Close(); lease.Release()。client.Close は呼ばない。
ts.lease = lease
ts.closer = func() { _ = session.Close(); lease.Release() }
```

- `dialCtx` は P2 のとおりプレースホルダの cancel から派生させる。`CloseTerminal(id)` で cancel されると `Acquire` は `ctx.Err()` で戻るが、走っている dial は pool の ctx で完了し、プールに idle として残る（origin manual なので5分回収の対象外。§9-U2）。
- `RequestPty` 以降の失敗（PTY 確保・Shell）は client の故障とは限らないので、`Release` だけで client を返す。client が本当に死んでいれば pool の keepalive / Wait 監視が拾う。

### 3.2 exit reason の判定

P2 では `session.Wait()` の戻りで「exit status あり → `remote_exit`、それ以外 → `conn_lost`」と判定していた。プール下では1本の client を複数タブが共有するので、次の順で判定する。

1. 既に `close("user", …)` が始まっていれば user（Once で先勝ち。P2 と同じ）。
2. `Wait` が `*ssh.ExitError` または nil → `remote_exit`（exitCode 付き）。
3. それ以外で `lease.Broken()` が true → `conn_lost`。
4. それ以外 → `conn_lost`（チャネルだけ落ちた場合。メッセージに「セッションが切断されました」）。

受け入れ条件「スリープ復帰を模した KillAll から1秒以内に切断を検知」は pool の `client.Wait()` 監視が満たし、その client の上の全セッションの `Wait` がまとめて戻ることで、そのサーバーのタブにだけ `terminal:exit {reason:"conn_lost"}` が届く（P2 のテスト「KillAll するとそのサーバーのタブにだけ exit が届く」をそのまま流用できる）。

### 3.3 閉じる系

- `CloseTerminal` / `CloseTerminalsByConnection` / `CloseAllTerminals` は **セッションを閉じて lease を返すだけ**。client は残る（次のタブが速く開く。これがプールの目的）。
- 「接続を切断（connID）」だけは client まで閉じる（§3.4 の `DisconnectConnection`）。

### 3.4 app.go 側（DisconnectConnection / Save / Delete）

P2契約 §1.2 の `DisconnectConnection` の手順に1つ足す。

1. `a.connectionByID(id)`
2. `k := a.closeTerminalsForConnection(conn.ID)`（lease が返る）
3. `j := a.cancelExecsForConnection(conn.ID, errDisconnected)`（exec の lease が返る）
4. **`if a.pool != nil { a.pool.CloseConnection(conn.ID) }`**（人間の切断は即時。2・3 で使用中が無くなってから呼ぶので、使用中の client を切ることは原則ない。3 の後に exec が残っていても、即時切断は仕様どおり）
5. `a.sessionsChanged()`

- `SaveConnection`: DB 反映後（§5.0 の順）、ロック解放後に `a.pool.Drain(conn.ID)`。fp が変わっていなければ pool 側で何もしない。開いているタブは切れない（P2 の「保存するたびにタブが切れる」を起こさない方針を維持。drain は使い終わってから閉じる）。
- `DeleteConnection`: タブを閉じ、実行を cancel したうえで `a.pool.CloseConnection(conn.ID)`（即時）。
- パスフレーズ入力（`sshPassphrases[connID]` の更新）: fp の対象（§9-U1）。入力後に `Drain`。

---

## 4. ssh_exec.go の差分方針（MCP 実行）

### 4.1 分割

```go
// 取得部（新規）。poolExec / poolAll のとき executeSSH から呼ぶ。
func runSSHCommandPooled(ctx context.Context, pool *sshPool, conn Connection, command string, outputCap int) (commandResult, error)

// 実行部（runSSHCommand から抜き出す）。client を閉じない。
func runOnSession(ctx context.Context, session *ssh.Session, command string, outputCap int) (commandResult, error)

// 既存（poolOff 用）。中身は openSSHClient → NewSession → runOnSession → client.Close に組み替えるだけで挙動は同じ。
func runSSHCommand(ctx context.Context, conn Connection, command string, outputCap int, strictHostKey bool) (commandResult, error)
```

`runSSHCommandPooled` の流れ:

1. `lease, err := pool.Acquire(ctx, conn, leaseExec, originAuto)`（失敗はそのまま返す。`classifyExecError` の分類は変わらない）
2. `defer lease.Release()`
3. `session, err := lease.NewSession(ctx)`（OpenChannelError の待ち/overflow、EOF の evict→取り直しは **この中だけ**）
4. `return runOnSession(ctx, session, command, outputCap)`

### 4.2 再試行の禁止（§5.1「Run の後は絶対に再実行しない」）

- `runOnSession` に入ったら、どんなエラーでも再試行しない。`Run` の途中で client が落ちたら `conn_lost` 相当のエラーで返し、`finishRequest` が failed にする。
- 実装上の担保: 再試行のループは `sshLease.NewSession` の中にしか書かない。`runSSHCommandPooled` に `for` を書かない。
- テスト: §10 の「NewSession 成功後に KillAll → execLog が1件のまま、結果は failed」。

### 4.3 cancel 時の挙動（重要な差分）

現在の `runSSHCommand` は `ctx.Done()` で `session.Signal(SIGKILL)` → `session.Close()` → **`client.Close()`** をしている。プール下では client は共有物なので、`runOnSession` では **`client.Close()` を呼ばない**。

- `Signal(SIGKILL)` → `session.Close()` → `done` を最大2秒待つ、までは同じ。
- 2秒待っても `Run` が戻らない（チャネルが詰まっている）場合は、その client を疑わしいとみなし `lease.Release()` の前に pool に drain を依頼する（`Drain` を connID 単位でなく client 単位で行う手段が要る。§9-U3）。即時 Close はしない（同じ client 上のタブを巻き込むため）。
- `errDisconnected`（人間の切断）で cancel された場合は、§3.4 の手順4で pool が即時に閉じるので、ここで特別なことはしない。

### 4.4 プールを通さないもの

- `testSSHConnection`（接続テスト）: 未保存の値でも動き、`lastTest` だけを書く仕様（§5.3）なので、pool に入れると「保存前の設定の client」が混ざる。**pool を通さず直接 dial して閉じる**。ただし §8 の dial 遅延計測には使う。
- シリアル: pool の対象外。

---

## 5. shutdown 順序（§5.0 の手順4）

組み込み後の `shutdown` は次のとおり（P2契約 §1.5 の順序に4を足す）。

1. `stopMCPServer`（MCP の受付停止。新しい exec の Acquire が来なくなる）
2. 実行中の exec を `errShuttingDown` で cancel し、`execWG` を最大 `shutdownExecGrace` 待つ → 「中断」を永続化（exec の lease はここで全部 Release される）
3. `a.CloseAllTerminals()`（タブの lease が全部 Release される。exit reason は user）
4. **`if a.pool != nil { a.pool.CloseAll() }`**
5. `store.Close()`

守ること:

- **pool の ctx を `appCtx` から派生させない。** 手順2で `appCancel` 相当の cancel が走るため、pool の ctx が `appCtx` の子だと、手順3より前に keepalive と client が落ち、タブの exit reason が `user` ではなく `conn_lost` になる（並行して `terminal:exit` が二重に飛ぶ競合も生む）。pool は `context.Background()` から自前の ctx を作り、`CloseAll` でだけ cancel する。
- `CloseAll` は「closed フラグを立てる（以後の Acquire は `errPoolClosed`）→ 走っている dial の ctx を cancel → 全 client を Close → keepalive / Wait 監視の goroutine を最大2秒待つ」の順。`pool.mu` は Close の前に解放する（Close は I/O を伴うため）。
- 手順3の後に `Release` されていない lease が残っていたら、それはリーク。`CloseAll` は残っていても閉じる（終了を止めない）が、`log.Printf("pool: %d leases outstanding at shutdown", n)` を出す。テストでは 0 を表明する。
- OnBeforeClose の確認ダイアログの「接続中」は P2 の定義（生きているタブ ≥1）のまま。idle の pooled client は確認の対象にしない（閉じても失うものがない）。

---

## 6. 状態の語彙（frontend は変えない）

- `connState=connected` の定義は §5.1 第2〜4段のまま「生きているタブが1つ以上」。idle の pooled client だけがある接続先は `connected` にしない（サイドバーで「接続中 0 タブ」という矛盾した表示を出さないため）。
- 「接続を切断」ボタンの有効条件も P2 のまま（connected のときだけ）。タブも exec も無いのに idle client が残っている場合は、5分回収（auto）か shutdown で消える。manual の idle client の扱いは §9-U2。

---

## 7. 段階的な切替フラグ

`SSH_GETE_POOL` 環境変数（P1 の `SSH_GETE_*` の env override と同じ流儀。テストでは `newTestApp` が隔離する）。

| 段階 | 値 | 中身 | 戻し方 |
|---|---|---|---|
| A. 計測 | （未設定）= `off` | pool は生成しない。`ssh_pool_metrics.go` の計測だけ有効（§8）。dial は今のまま | — |
| B. MCP のみ | `exec` | `executeSSH` だけ pool 経由（origin auto）。ターミナルは P2 のまま1タブ1接続 | 未設定に戻して再起動 |
| C. 全部 | `all` | ターミナルも pool 経由（origin manual） | `exec` か未設定に戻して再起動 |
| D. 既定化 | 既定を `all` に変更 | 1リリース様子を見る。`off` は残す | `SSH_GETE_POOL=off` |
| E. 撤去 | — | poolOff 経路（`runSSHCommand` の client 所有版、terminal.go の client 所有版）を削除 | — |

- **値は起動時に1回だけ読む。** 実行中に切り替える API は作らない。理由: 切替の瞬間に「client を所有するタブ」と「lease を持つタブ」の所有権が混ざり、どちらが Close するかの規約（§5.0）を実行時に検証できないため。`closer` を開いた時点のモードで固定（§2）しているので混在しても壊れはしないが、運用上の切替は再起動で行う。
- B を先にする理由: MCP の exec は短命で、Run 後の再試行禁止さえ守れば失敗時の影響が1コマンドに閉じる。ターミナルは長命で、keepalive の誤判定がユーザーの作業を落とすため後にする。
- 各段階の昇格条件: 直前の段階で §8.3 の事後指標が1週間基準内であること、`go test -race -count=20` が緑であること。
- 段階 A の計測は B 以降も動かし続ける（プールの効果を同じ物差しで比べるため）。

---

## 8. 「プールが必要」と判断する計測方法

フェーズ5 は「実測で必要になってから」なので、段階 A で次を測り、§8.2 の基準のどれかを満たしたときだけ段階 B に進む。満たさなければフェーズ5 は入れない（プールは複雑さの割に、1タブ1接続の正しさを失うリスクがある）。

### 8.1 計測ポイント

計測は `ssh_pool_metrics.go`（新規）に置き、dial を包むデコレータとして入れる。pool の有無に関係なく同じ場所で測れるよう、**注入する dial 関数そのものを包む**。

```go
func measuredDial(m *dialMetrics, dial sshDialFunc) sshDialFunc
```

| ID | 何を | どこで | 記録 |
|---|---|---|---|
| M1 | dial 全体の所要時間（TCP connect + 鍵交換 + 認証） | `measuredDial` の前後。経路ラベル `term` / `exec` / `test` を付ける | connID ごとのリングバッファ（直近 200 件）、p50 / p95 / max |
| M1a | 内訳: TCP connect と SSH ハンドシェイク | `openSSHClient` の `DialContext` 直後と `NewClientConn` 直後（ssh_exec.go に時刻を返す内部フックを1つ足す。段階 A で唯一の既存ファイル変更） | 同上 |
| M2 | dial 失敗の種類 | `measuredDial` のエラー分類: `handshake failed: EOF`（MaxStartups による切断の典型）、`connection refused`、timeout、認証失敗、passphrase | connID × 種類のカウンタ。直前の同時 dial 数も一緒に記録 |
| M3 | 同時接続数のピーク | dial 成功で +1、client Close で −1（`measuredDial` が返す client の Wait を監視） | connID ごとのピークと、ピーク時の内訳（タブ数・exec 数） |
| M4 | dial レート | M1 のタイムスタンプから 1分窓で算出 | connID ごとの最大 dial/分 |
| M5 | exec に占める dial の割合 | `executeSSH` の総所要時間と M1（exec ラベル） | 中央値 `dial_ms / total_ms` |
| M6 | dial ごとの人手介入 | `sshAuthMethods` で agent の Sign 呼び出しが `agent: refused`・長時間（> 2s）だった回数（FIDO キーのタッチや agent の confirm を示す） | connID ごとのカウンタ |
| M7 | NewSession の `*ssh.OpenChannelError` | 段階 A では1接続1セッションなので通常 0。段階 B 以降の MaxSessions 超過の検出用（§8.3） | connID × `Reason`（`Prohibited` / `ResourceShortage`）のカウンタ |

- 段階 A では「1 client に 1 session」なので、sshd の `MaxSessions`（既定 10、接続あたり）を超えることは構造上ない。段階 A で効いてくる上限は **`MaxStartups`（既定 `10:30:100`。認証前の同時接続）** で、これを M2 の `handshake failed: EOF` と M3 の同時 dial 数の相関で見る。`MaxSessions` 超過は段階 B 以降に M7 で見る。
- 出力先: `SSH_GETE_METRICS=1` のときだけ、`appConfigDir()/metrics.jsonl`（0600）に1分ごとに集計行を追記する。**記録するのは connID・経路ラベル・時間・件数・エラー種類だけ**。host / user / credential / passphrase / コマンド文字列 / トークンは書かない。
- 計測自体は `metrics.mu`（末端ロック）だけで保護し、持っている間に他を呼ばない（§5.0）。

### 8.2 導入の判断基準（段階 A → B）

計測期間は通常の使い方で 7 日。次のどれか1つを満たす接続先があれば、プールが必要と判断する。

| # | 条件 | 根拠 |
|---|---|---|
| T1 | M1（exec）の p95 が **1500ms 超** で、かつその接続先の MCP 実行が **1時間あたり 20 回以上** | エージェントの短いコマンドの体感がほぼ dial で決まる |
| T2 | M5 の中央値が **50% 超**（実行時間の半分以上が dial） | 同上。T1 より遅延の絶対値に依存しない |
| T3 | M2 の `handshake failed: EOF` が **1日 1 回以上**、かつそのとき同時 dial 数が **8 以上** | `MaxStartups` の既定 10 に近づいて sshd に切られている |
| T4 | M3 のピーク同時接続数が **8 以上**（タブ + exec） | §7 フェーズ5 の受け入れ条件（タブ7 + MCP4）の規模に実際に達している |
| T5 | M4 が **30 dial/分 超** | 接続ごとの認証ログ・侵入検知（fail2ban 等の運用側ルール）に引っかかる量 |
| T6 | M6 が **0 でない** | dial のたびに FIDO タッチや agent の確認が要り、接続の使い回し以外で解決できない |

- どれも満たさない場合: フェーズ5 は入れない。T1〜T6 の値を記録して「見送り」の判断を残す。
- T3・T4 だけを満たす場合: プールより先に「MCP の同時実行数の上限」（App 側のセマフォ）で足りないか検討する。プールは接続を減らすが、同じ client に詰め込むぶん `MaxSessions` の新しい上限にぶつかる。

### 8.3 組み込み後の事後指標（段階 B 以降。昇格・ロールバックの判断）

`sshPool.Stats()` と M1〜M7 で見る。

| 指標 | 基準 | 外れたとき |
|---|---|---|
| M7 の OpenChannelError（MaxSessions 超過） | 1日 5 回以下 | セマフォ既定 8 をサーバーの `MaxSessions` より小さくする（例 6）。それでも出るなら overflow 上限を見直す |
| セマフォ待ち時間 p95 | 200ms 以下 | overflow の2本目を早めに張る |
| overflow client 数 / 接続先 | 平均 1.2 以下 | 多いなら共有の効果が薄い。段階を戻すか上限を下げる |
| exec の dial 回数（M1 exec） | 段階 A の 20% 以下 | 減っていないなら fp の揺れで drain が多発している疑い（`Drain` 回数を確認） |
| keepalive による Close のうち、直後の再 dial が 1 秒以内に成功した割合 | 10% 以下 | 高いなら keepalive の誤判定。タイムアウト 10s / 2回連続の値を見直す |
| shutdown 時の未返却 lease | 常に 0 | リーク。段階を戻す |
| ターミナルの `conn_lost` 件数 | 段階 A と同程度 | 増えたなら共有 client の巻き添え切断。段階 C から B に戻す |

---

## 9. 未解決事項

- **U1 fp の材料が `Connection` だけでは足りない。** §5.1 の fp は「ホスト鍵ポリシー、agent のパス、known_hosts の世代」を含むが、`strictHostKey` は `a.mcp.StrictHostKey`、パスフレーズは `a.sshPassphrases`、agent のパスは環境変数にあり、注入する dial のシグネチャ `func(ctx, Connection)` には入らない。案: (a) `Acquire` に `fpExtra string` を渡す、(b) fp 関数も注入する、(c) `KeyPassphrase` を `Connection` に詰めて渡す今のやり方を踏襲し、strict と agent パスは `dialSSH` が `Connection` の非公開フィールドに入れる。プール担当と決める。
- **U2 manual の idle client を回収するか。** §5.1 は「auto は5分で回収」とだけ言う。人間がタブを全部閉じた後の manual client を無期限に持つと、サイドバーは「未接続」なのに TCP が張られたままになる。案: 最後のタブが閉じてから 15 分で回収（auto より長く）。
- **U3 client 単位の drain。** §4.3 の「cancel しても Run が戻らない client」を印付けする手段が `Drain(connID)` しかないと、同じ接続先の健全な client まで drain される。`lease.MarkSuspect()` のような client 単位の API が欲しい。
- **U4 origin の混在。** auto で張った client をターミナルが借りた場合、その client の origin を manual に格上げするか（5分回収で巻き込まないため）。案: 使用中（activeTerm > 0）は origin に関係なく回収しない、を pool 側の規約にする。
- **U5 MCP にプールの状態を見せるか。** `list_hosts` に「接続済み（プール）」を出すとエージェントが「接続を保つための空コマンド」を打つ誘因になるので、v1 では出さない方針にしたが未確定。
- **U6 TOFU（フェーズ3）との順序。** 長命の client はホスト鍵の再検証を受けない。known_hosts の世代を fp に入れれば drain されるが、「GUI でリセットした直後に既存 client を即時切るか、drain か」は §5.1 で「即時は Delete と人間の切断だけ」とされている。鍵のリセットを即時側に入れるかはユーザー判断（§8 の論点2と関係）。
- **U7 テスト用サーバーの MaxSessions。** 基準コミットの `sshtest_test.go` には maxSessions・acceptCount・KillAll がまだ無い（P0/P2 で追加予定）。§10 のテストはそれらを前提にしている。プール担当が追加した版と名前を合わせる。
- **U8 ターミナルと exec の帯域共有。** ターミナルと exec が同じ client を共有すると、ターミナルで大量出力中に exec の応答が TCP の共有で遅れる（チャネルのウィンドウ制御は別でも、帯域は共有）。§8.3 で exec 遅延が悪化したら、exec を term と別 client に分ける（`leaseUse` ごとに client を分ける）設定が要るかもしれない。

---

## 10. 組み込み時のテスト（すべて `-race`、並行系は `-count=20`）

テスト用 SSH サーバーは `sshtest_test.go` の既存のものを使い、dial は `newSSHPool(func(ctx, c) { return openSSHClient(ctx, c, false) }, opts)` で注入する。`keepaliveInterval=50ms`、`maxChannels=3`（§7 フェーズ5）。

| テスト | 確かめること |
|---|---|
| `TestPoolAdoptTerminalCloseKeepsClient` | タブ2つを開いて1つ閉じても acceptCount が増えず、もう1つは生きている。閉じたタブの closer が client を Close していない |
| `TestPoolAdoptDisconnectClosesClient` | `DisconnectConnection` でタブ・exec が終わり、`pool.Stats()` のその connID が 0 |
| `TestPoolAdoptExecNoRetryAfterRun` | NewSession 成功後に KillAll → execLog が1件のまま、結果は failed |
| `TestPoolAdoptExecCancelKeepsSharedClient` | 同じ connID にタブ1つ + `sleep 10` の exec。exec を cancel してもタブは生きている（§4.3） |
| `TestPoolAdoptOpenChannelErrorWaits` | `maxSessions=3` のサーバーにタブ7 + exec4 → 健全な client は閉じられず、全部が最終的に開く（待つか overflow） |
| `TestPoolAdoptKillAllConnLost` | KillAll から1秒以内に、そのサーバーのタブにだけ `terminal:exit {reason:"conn_lost"}` |
| `TestPoolAdoptShutdownOrder` | shutdown 中のタブの exit reason が全部 `user`（pool の ctx が appCtx の子でないこと）。CloseAll 時の未返却 lease が 0。CloseAll 後の Acquire が `errPoolClosed` |
| `TestPoolAdoptPassphraseNotCached` | 1回目 `needs_passphrase` → パスフレーズを入れて再試行で即成功 |
| `TestPoolAdoptModeOffUnchanged` | `SSH_GETE_POOL` 未設定で `a.pool == nil`、P2 の全テストが変更なしで緑 |
| `TestMeasuredDialRecordsNoSecrets` | metrics.jsonl に host・user・credential・コマンドが含まれない、ファイルが 0600 |
