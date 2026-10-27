# ssh-gate-oss 改訂設計 v1（委員会裁定版）

## 1. 委員会の総評

- 方向性は v0 のまま妥当です。ただし v0 の B1（接続プール）は、タイムアウトの段階設計、MaxSessions の扱い、refcount と drain、所有権の4点が未定義でした。このまま実装すると「1台が半死の状態になるだけで全体がハングする」「タブを1つ閉じると他のタブも落ちる」「保存するたびに作業中のタブが切れる」の3つが起きます。
- マルチセッションを妨げている本当の原因は v0 に書かれていませんでした。①画面を移るとターミナルが閉じる（main.js:1592 の `teardownTerminal`）、②ターミナル画面にいる間は再描画が止まる（main.js:595 の `protectTerminal`）、③編集中の値が DOM にしかなく、選択が配列 index で管理されている、の3点です。
- そこで段階を組み替えます。**プールを後に回し、まず「1タブ＝専用 ssh.Client」でマルチタブを出します。**「接続中」は「その接続先に開いているタブが1つ以上ある」と定義します。これだけで、ユーザーが言う「保存と接続の管理が曖昧」の大部分は解消します。
- セキュリティ（CSRF・Origin・承認の CAS・承認省略の risk 上限）と永続化（不変ID・user_version・旧 JSON の再取り込み停止）は土台なので、第1段で先に入れます。
- MCP 側は「request_id で状態を取れること」「結果を機械的に判定できること」が多重化の前提です。get_request_status は v0 では将来扱いでしたが、今回の必須にします。

## 2. 採択した主要変更点（v0 からの差分）

| # | 変更 | 根拠になった委員 |
|---|---|---|
| 1 | **段階を組み替える。** 第2段はプールなしで、1タブに専用 Client を持たせて多重化する。プール・keepalive・チャネル数の上限は第5段（任意）に回す | scope-skeptic、pool-lifecycle、go-concurrency |
| 2 | 画面遷移でセッションを閉じない。xterm は `#terminal-stack`（`#app` の外に置く常設コンテナ）で保持する。閉じるのはタブの×、「接続を切断」、接続先の削除、アプリ終了の4つだけ | terminal-tabs、frontend-state、ux-sidebar、scope-skeptic、serial |
| 3 | `render()` を分割する（renderSidebarStatus / renderServerTabs / renderPage）。サイドバーとバッジは protect 中でも更新する | frontend-state、ux-sidebar、terminal-tabs、testing |
| 4 | frontend が sessionId を発番し、`StartTerminal(id, connId, cols, rows)` を呼ぶ。初回出力の取りこぼしと、初期 PTY が 80x24 固定になる問題を同時に解消する | terminal-tabs、go-concurrency、testing、frontend-state |
| 5 | **不変ID を採用する。** `connections.id`（UUID）を足し、プール・ターミナル・パスフレーズ・CommandRequest のキーを id にする。Name は UNIQUE な表示属性にする。リネームは1文の UPDATE で済む | persistence、go-concurrency、frontend-state |
| 6 | `PRAGMA user_version` によるスキーマ版管理を入れる。旧 JSON の移行は一度きりにし、移行後は `*.migrated` にリネームする | persistence、security |
| 7 | 状態を3軸に分ける。保存軸 saveState（frontend が算出）、接続軸 connState（バックエンドのライブ値。DB には保存しない）、疎通テスト（状態ではなく履歴として lastTest に記録） | ux-sidebar、frontend-state、scope-skeptic |
| 8 | ステータスを英字コード化する（CommandRequest.state / Connection runtime）。日本語は I18N で表示する。既存の日本語値は移行マップで変換する | ux-sidebar、agent-ux |
| 9 | 承認を有限状態機械 `transitionRequest` にする。遷移は DB の CAS（`UPDATE … WHERE status=?` の RowsAffected==1）で判定する。「実行中」を実行前に永続化し、起動時に「中断」へ回収する | approval-audit、go-concurrency、persistence、ops-limits |
| 10 | 承認省略に上限を設ける。risk 上限（既定 Medium まで、High は常にキュー）、RequireApproval* の実装、取り消し UI、期限 | approval-audit、security |
| 11 | Origin 付きのリクエストは既定で 403 にし、`localhost:*` の既定値は撤廃する。Host は完全一致かつポートも一致を見る。非 loopback で待ち受けるときは Bearer を必須にする。これらを共通ミドルウェア `guardHTTP` で mux 全体にかける | security、mcp-spec |
| 12 | MCP の土台を作る。protocolVersion の交渉、ping、202 の扱い、ツール内エラーの isError、structuredContent、annotations、r.Context() によるキャンセル伝播 | mcp-spec、agent-ux |
| 13 | `get_request_status`（long-poll と複数 ID）を新設する。`list_sessions` は作らず、list_hosts にランタイム項目を統合する | agent-ux（mcp-spec の名称案は不要になる） |
| 14 | ハンドシェイクに期限を設ける。SetDeadline と ctx 監視の goroutine を使い、Dialer.KeepAlive も設定する | ssh-protocol、pool-lifecycle、ops-limits |
| 15 | credential を書き込み専用にする（`credentialSet` bool だけを返す）。keyPath と password は別のフィールドにする | security |
| 16 | サーバータブは**グローバルなフィルタ1本**にする。ターミナル画面だけ、そのサーバー内のシェルをサブタブで出す | scope-skeptic（E の確定） |
| 17 | シリアルはプールの対象外で、タブと1対1にする。疎通テストではポートを開かない。アプリ内に予約表を持つ | serial、cross-platform |
| 18 | テストの基盤を作る。差し替え口（dial / hostKey / emit / clock）、in-process の sshtest.Server、CI（test.yml を3 OS の matrix で -race）、vitest | testing |

## 3. 却下・後回しにした提案

| 提案 | 裁定 | 理由 |
|---|---|---|
| 第2段からの SSH 接続プール（v0 B1） | **後回し（第5段、任意）** | 今の「1コマンド1接続」でも並行実行はできています。プールがあるせいで初めて必要になる仕組み（drain、refcount、MaxSessions の分類、origin、アイドル回収、ゾンビ dial）が多く、ユーザーの要望には不要です。仕様は §5.1 に正しく書いておきます |
| `list_sessions` / `list_ssh_connections` の新設 | 却下 | list_hosts に統合します（agent-ux）。2回呼ぶ必要と情報の重複をなくすためです |
| `session_reused` フィールド | 却下 | プールがない段階では意味がありません |
| 監査用のハッシュチェーン `audit_events` | 後回し | 第1段では論理削除、監査対象の削除禁止、decided_by/at だけを入れます |
| OS キーチェーン（go-keyring / DPAPI） | 後回し（第5段） | macOS の未署名ビルドで毎回許可ダイアログが出る問題の UX が未設計です。当面は 0600 と書き込み専用で対応します |
| Mcp-Session-Id の発行と SSE | 却下（v1 では見送り） | ステートレスな Streamable HTTP にし、GET は 405＋Allow を返すと明記します。エージェント数は「直近5分のアクティブ数」と表記します |
| 冪等キー `client_request_id` | 採択（第3段） | 再送による二重実行を防ぐうえで安価で効果が大きいです |
| バックエンドのスクロールバック用リングバッファ | 後回し | 第2段は frontend で xterm を生かし続けることで代替できます。WebView をリロードした場合は「以前の出力は表示できません」と出して既存セッションを再アタッチします |
| シリアルの DataBits/Parity/StopBits | 採択（第4段）。RTS/CTS は「非対応」と明記 | go.bug.st/serial に API がないためです |
| Windows の %LOCALAPPDATA% への移設 | 後回し | 既存ユーザーの移行が必要になるためです |
| 画面ごとに独自のサーバータブを持たせる案 | 却下 | 1900行の render を全面的に書き直すことになるためです（scope-skeptic） |
| 承認時のコマンド修正 UI | **論点③へ** | `#approval-command` 要素が存在せず（main.js:1793）、修正機能は実質無効です。提供するかどうかはユーザーの判断が要ります |

## 4. 確定したバグ一覧（重要度順）

| 重要度 | 場所 | 内容 | 修正方法 |
|---|---|---|---|
| Critical | mcp_server.go:131-150 handleMCP | CSRF と DNS rebinding。Origin・Host・Content-Type を検査しておらず、OPTIONS は無条件に `ACAO: http://localhost` を返す | `guardHTTP` を mux 全体にかける。Origin 付きは 403（`null` も 403）。Host は {localhost,127.0.0.1,[::1]}＋明示設定で、ポートも一致を見る。Content-Type は `mime.ParseMediaType` で照合する |
| Critical | app.go:496-519 ApproveCommand | status を確認しておらず、実行中も「承認待ち」のまま。二重クリックや再起動後に二重実行される | `transitionRequest`（DB の CAS）で「実行中」を永続化してから実行する。起動時に「中断」へ回収する。ボタンは押下中 disabled にする |
| High | ssh_exec.go:122-132 openSSHClient | NewClientConn に期限がない。`ClientConfig.Timeout` は ssh.Dial でしか効かず、バナーを返さない相手だと無期限にハングする | `netConn.SetDeadline(now+timeout)`、ctx.Done で Close する監視 goroutine を置き、成功したら Deadline を解除する。`Dialer.KeepAlive=15s` |
| High | ssh_exec.go:76-89 runSSHCommand | タイムアウト時、copy goroutine が書き込み中の cappedBuffer を String() で読むデータ競合 | cappedBuffer に Mutex を持たせる。Close 後に `<-done` を最大2秒待ってから読む |
| High | app.go:521-539 ApproveCommandAndBypassAgent | 承認省略を先に永続化しており、承認が失敗しても省略だけ残る | CAS が成功してから付与する。テストで「拒否済みの ID では policies が変わらない」を確認する |
| High | app.go:546-556 RejectCommand | status を確認せず、実行済みでも「拒否」で上書きする | `transitionRequest(pending→rejected)` 経由にする |
| High | mcp_server.go:274-286,324-335 / app.go:122-125 | 承認省略が High リスクにも効く。RequireApprovalSudo/Prod/WriteOp はどこからも参照されていない | `needsHumanApproval(conn,risk,cmd,policy)` を実装し、High は常にキューに入れる |
| High | main.js:1588-1594 / 1214 | 画面遷移で CloseTerminal と dispose が走り、セッションが切れる | 遷移時の teardown を削除し、xterm は `#terminal-stack` で保持する |
| High | main.js:578-586,1564-1573 | 編集中の値が DOM にしかなく、render のたびに戻る。showToast は3.2秒後にも render を呼ぶ | `state.draft` を導入し、showToast は renderToast だけを差し替える |
| High | app.go:714-733 setConnectionStatus / storage.go:154 | 行全体を upsert しており、削除済みの行が復活したり、古い設定に巻き戻ったりする | 列単位の UPDATE（`WHERE id=?`）にする。status 列は upsert の対象から外す |
| High | app.go:879-915 migrateLegacyData | 起動のたびに旧 JSON を再取り込みする。全件削除後や DeleteMCPSettings 後に、平文パスワードやトークンが復活する | user_version で一度きりにし、成功後は `*.migrated` にリネームする |
| High | app.go:372-377 SaveConnection | store==nil でも「保存しました」と返す | OK=false を返す。起動時の失敗は `storageError` としてサイドバーに常時表示する |
| High | mcp_server.go:102-119 / app.go:447 | Serve goroutine が `a.mcpServer` を再読み込みしており、nil deref や旧 listener の取り違えが起きる。無ロックの競合もある | srv をローカル変数で捕捉し、lifecycleMu で直列化する。Shutdown 後に listener.Close() する |
| High | app.go:668-686 executeCommandAndLog | 自動実行は完了後にしか記録されず、クラッシュすると監査が消える | 実行前に `auto_running` で記録する |
| High | mcp_server.go:203-208 | protocolVersion を無検証でそのまま返している。ヘッダは固定値 | supportedVersions を定義して交渉する。ヘッダが不正なら 400 を返す |
| High | mcp_server.go:276-285,325-334 | 自動実行の失敗に isError が付かない。exit_code がない | `toolResult(v,isError)` にし、`*ssh.ExitError` から exit_code を取る |
| Medium | app.go:1076-1079 applyMCPEnv → SaveMCPSettings | 環境変数 SSH_GETE_TOKEN が DB に平文で保存される | 上書き値は `mcpEnvOverride` に分けて持ち、保存しない。GUI に「環境変数で上書き中」と表示する |
| Medium | mcp_server.go:137 rememberMCPAgent | 認証前に記録しているうえ、無制限に増える（メモリ DoS） | 認証通過後に記録する。5分で削除し、上限は256件にする |
| Medium | mcp_server.go:145 | Bearer の比較が定数時間ではない。401 に WWW-Authenticate がない | `subtle.ConstantTimeCompare` を使い、ヘッダを付ける |
| Medium | mcp_server.go:81-88 /audit/events | Bearer OFF のとき全 stdout を返し、件数も無制限 | GET のみにし、Origin 付きは拒否、Bearer ON なら認証必須、limit を設ける |
| Medium | mcp_server.go:163-166,24 | notification と response の 202 判定が不完全。parse error 時に id:null が出ない。batch を受けると -32700 になる | RawMessage で受ける。`[` で始まれば -32600。ID は omitempty を外す |
| Medium | mcp_server.go:201-229 | ping に対応していない | 空の result で即答する |
| Medium | mcp_server.go:260-272,308-323 | host not found などの業務エラーを JSON-RPC エラーで返している | isError:true と code、候補の Name を返す |
| Medium | mcp_server.go:364-366,43-51 | execute_command の説明が実際の挙動と違う。timeout_seconds/force_queue は未使用 | 説明を実挙動に合わせる。force_queue と timeout_seconds を実装する |
| Medium | ssh_exec.go:115-120,181-190 | HostKeyAlgorithms を設定していないため、known_hosts に別種の鍵があると不一致と誤判定する | 既知の鍵種別に限定する（`KeyError.Want` から取る） |
| Medium | ssh_exec.go:156-179 | パスフレーズ付きの鍵では agent が全鍵を提示し、MaxAuthTries を超える。agent ソケットも保持し続ける | `.pub` か PassphraseMissingError.PublicKey で Signers を絞る。ハンドシェイク後に agentConn.Close |
| Medium | app.go:293-298 TestConnectionConfig | シリアルでも Host/User を必須チェックしており、SSH を試す | 種別の分岐を先に置き、シリアルは開かずにテストする（GetPortsList と予約表で判定） |
| Medium | app.go:703-711 findConnection / app.go:806-830 | Serial に切り替えても host/credential が DB に残る。Host 一致で幽霊行に当たる | Host 一致からシリアルを除外し、credential を消す |
| Medium | app.go:496-512 | ApproveCommand にシリアルのガードがない | executeSSH の入口で Serial なら即エラーにする |
| Medium | ssh_agent_windows.go:20-26 | Unix 形式の SSH_AUTH_SOCK だと既定パイプにフォールバックしない | パイプ形式のときだけ採用し、それ以外は既定パイプにする。タイムアウトも付ける |
| Medium | ssh_exec.go:189-201 expandPath | `~\` や `%USERPROFILE%`、引用符付きのパスを展開しない | 展開処理を拡張する |
| Medium | storage.go:21-35 | DB 本体と -wal/-shm が 0644。`.` へのフォールバックもある | 事前に 0600 で作成し、起動時に Chmod する。`.` へのフォールバックは廃止する |
| Medium | app.go:266-274 GetInitialData | credential を平文でポーリングごとに返す。全 stdout も毎回転送する | credentialSet だけを返す。履歴は要約にし、本文は GetCommandRequest(id) で取る |
| Medium | storage.go:234-257 | 501件目より古い承認待ちが読み込まれない。DB は無制限に増える | 承認待ちと実行中は全件読み込み、それ以外は最新 N 件にする。保持ポリシーとインデックスを追加する |
| Medium | main.js:1526-1527 collectConnectionForm | 古い status/lastChecked を送って上書きする | 送らない。バックエンドも無視する |
| Medium | main.js:1628,1901 | ポートの再スキャンや種別切替で、フォームの値（名前を含む）が state.data.connections を上書きする | draft 方式にする（id で識別） |
| Medium | main.js:1661-1663,1836-1841 | 「新規」が消える。行を選ぶと確認なしで編集を破棄する。新規を2回押すと同名の行ができる | draft を使い、破棄の確認ダイアログを出す。新規の draft は1つだけにする |
| Medium | main.js:588-603 | ポーリングと操作が競合し、一瞬古い状態に戻る | dataGen による世代番号と inFlight フラグ |
| Medium | main.js:308,534-539,612 | EN 辞書に「承認待ち」が残っている。件数を日本語文字列の一致で数えている | コード化する |
| Medium | main.js:529-532 / app.go:301-311 | Error も「切断中」と表示する。Disconnect は status を書き換えるだけ | 語彙を整理する（§6） |
| Medium | style.css:44-69 | ステータス欄が absolute 配置で、固定の padding-bottom を前提にしている | flex 配置にし、`margin-top:auto` を使う |
| Medium | main.js:474,1402-1405 | 一覧にないボーレートは保存し直すと 9600 になる | 現在値を option に追加する |
| Medium（推定。Wails の実装は未確認） | main.js:1179-1181 | SendTerminalInput を非同期で並行呼び出ししており、入力順が保証されない | タブ ID ごとに送信キューを持ち、promise チェーンで直列化する |
| Medium | main.js:1196-1202,1239 | window の resize にしか反応しない | ResizeObserver を使い、サイズが変わったときだけ送る |
| Low | ssh_exec.go:41-46 cappedBuffer | max<=0 のとき常に truncated になる。ちょうど上限のときの誤表示もある | 実際に切り捨てたときだけ truncated フラグを立てる |
| Low | mcp_server.go:95 | MaxHeaderBytes に MaxBodyKB を流用している | 64KB 固定にする |
| Low | v0 #8 queueCommand | args.Host（IP のこともある）を記録している | connection_id と表示名のスナップショットを記録する |
| Low | v0 #11 | RequestedAt/LastChecked が時刻のみ | created_at（ミリ秒）を正とし、v2 の移行で RFC3339 に変換する |
| Low | v0 #14 | SSH_GETE_MCP_ADDR が IPv6 に未対応 | net.SplitHostPort を使う |

除外または修正した主張は次のとおりです。

- v0 #6「agentConn を Client の寿命まで保持する」は誤りとして撤回しました。agent が必要なのは認証の間だけなので、ハンドシェイクが終わったら閉じます。
- v0 #10「シリアルのテストはポートの open/close で行う」も誤りとして撤回しました。DTR によって基板がリセットされるうえ、自分のアプリが使用中のときは EBUSY になります。
- 「Wails の呼び出しは1件ごとに goroutine」はコードを確認できていないため推定とし、Medium に据え置きました。

## 5. 改訂アーキテクチャ

### 5.0 所有権とロックの規約（全段に共通）

- `ssh.Client` を Close してよいのは、その所有者だけです。第2〜4段ではターミナルのセッションが、第5段ではプールが所有者です。
- `a.mu`、`termMu`、`pool.mu` のうち2つを同時に持ってはいけません。コールバック（status 更新や exit の通知）はロックを解放してから呼びます。
- 書き込みは「DB に書けたらメモリに反映する」順にします。Store には `writeMu` を持たせて書き込みを直列化します。
- App には `appCtx`/`appCancel` と、実行中の処理を数える `wg` を持たせます。終了処理の順序は次のとおりです。
  1. MCP の受付を止める
  2. 実行中のコマンドを最大 N 秒待ち、終わらなければ cancel して「中断」を永続化する
  3. CloseAllTerminals
  4. （第5段のみ）pool.CloseAll
  5. store.Close
- Wails の OnBeforeClose では、接続中・実行中・承認待ちがあれば確認ダイアログを出します。

### 5.1 接続の管理

**第2〜4段（プールなし）**
- `terminalSession{id, connID, kind, client(専用), session, stdin, closer(Once), reason, startedAt}` とします。
- 「接続中」の定義は「その connID に生きているタブが1つ以上ある」です。
- MCP の実行は今と同じく1コマンド1接続のままにします。ただし handshake の期限、`r.Context()` の伝播、`appCtx` からの派生を入れます。
- 「接続を切断（connID）」は、その接続先のタブをすべて閉じ、実行中の MCP コマンドを cancel します（確認ダイアログを出します）。

**第5段（任意）：プール仕様。委員の指摘を反映した正しい形**
- 構造は `pool.entries[connID] → {clients []*pooledClient, gen, inflight *dialCall}` です。pooledClient は次を持ちます。
  - client
  - fp（host/port/user/auth/credential のハッシュに、ホスト鍵ポリシー、agent のパス、known_hosts の世代を含めたもの）
  - activeTerm と activeExec
  - チャネル用のセマフォ（既定は8。exec はそのうち最大4本）
  - origin（manual か auto）
  - lastAliveAt、stale、hostKeyFP
- dial は `pool.mu` の外で、pool が持つ ctx を使って行います。待つ側は自分の ctx と dial の完了を select します。dial が終わったときに gen が変わっていれば、マップには入れずに Close します。エラーはキャッシュしません。
- `NewSession` は goroutine と ctx で包みます。エラーの種類で扱いを分けます。
  - `*ssh.OpenChannelError` のとき：接続は生きているとみなします。セマフォで待つか、同じ接続先に overflow 用の2本目を張ります。
  - EOF や net エラーのとき：ポインタ比較の CAS で evict し、singleflight 経由で再接続します。
- 再試行してよいのは NewSession の段階までです。Run の後は絶対に再実行しません。
- keepalive は goroutine で送り、10秒でタイムアウトさせます。2回続けて失敗したら Close します。これとは別に `client.Wait()` の監視で切断を即時に検知します。貸し出し時には生存確認をします（45秒以上経っていたら同期で ping）。スリープ復帰を検知したら全接続を検査します。
- fp が変わったときは stale を付けて drain します（使用中のものは使い終わってから閉じます）。即時に切るのは Delete と人間の「切断」だけです。auto（エージェントが開いた接続）は、使われずに5分経ったら回収します。

### 5.2 ターミナル多重化

| API | 内容 |
|---|---|
| `StartTerminal(id, connId, cols, rows) ActionResult{OK,Message,SessionID}` | id は frontend が発番します（`TERM-<uuid>`）。シリアルは予約表（正規化したポート名 → sessionId）を `termMu` の下で予約してから open します |
| `SendTerminalInput(id, data)` | backend 側でも、セッションごとに1本の goroutine が chan から読んで stdin に書く形にします |
| `ResizeTerminal(id, cols, rows)` / `CloseTerminal(id)` / `CloseTerminalsByConnection(connId)` / `CloseAllTerminals()` / `ListTerminalSessions() []TerminalInfo{id,connId,connName,kind,port,baud,startedAt,status}` | |
| イベント `terminal:data {id, data}` | 16ms または 32KB ごとにまとめて送ります。未送信分の上限は 4MB で、超えたら古い分を捨てて表示を間引きます |
| イベント `terminal:exit {id, reason: user\|remote_exit\|conn_lost\|device_removed\|io_error, exitCode?, message}` | sync.Once で1回だけ送ります。user のときはタブを閉じます。それ以外はタブを読み取り専用にして「再接続」を出します |

- シリアルのポート名は正規化します。darwin では tty.X を cu.X に寄せ、Windows では `\\.\` を外して大文字にします。
- frontend の状態は `terminal.tabs: Map<id,{connId,label,status,xterm,fit,onDataDisp,sendQueue,lastSize,needsFit}>`、`order`、`activeId` です。EventsOn は1回だけ登録し、知らない id のデータは捨てます（id を先に発番するので本来は届きません）。
- 起動時には `ListTerminalSessions()` で、生きているセッションのタブを作り直します。

### 5.3 MCP ツール仕様（v1）

共通の決まりは次のとおりです。

- ステートレスな Streamable HTTP で、GET には 405 と `Allow: POST, OPTIONS` を返します。
- 対応する版は 2025-06-18 と 2025-03-26 です。
- ツール内のエラーは `isError:true` とし、text と structuredContent の両方で返します。
- すべてのツールに title、annotations、outputSchema を付け、inputSchema には `additionalProperties:false` を指定します。
- エラーのコードは `host_not_found`、`ambiguous_host`、`serial_not_supported`、`server_busy`、`unauthorized_bypass` です。host_not_found と ambiguous_host のときは、候補の接続先名を最大10件返します。

| ツール | 入力 | 出力 |
|---|---|---|
| `list_hosts`（readOnly） | `{connected_only?:bool, tag?:string}` | `[{name, type:"ssh"\|"serial", executable:bool, tags[], connected:bool, connected_since?, open_terminals:int, inflight:int, pending_approvals:int}]`。シリアルの行は name、type、executable=false、in_use だけを返します。host、port、credential、指紋は返しません |
| `execute_command`（destructive, openWorld） | `{host, command, agent_name, reason?, client_request_id?, force_queue?:bool, timeout_seconds?:int}` | `{request_id, connection, state, risk, requires_approval, auto_executed, exit_code?, stdout?, stderr?, stdout_truncated, stderr_truncated, timed_out, error_kind?, duration_ms?, next_action?:"call get_request_status"}` |
| `request_command_execution` | execute_command と同じ | execute_command の**別名**（deprecated と明記します）。出力の形も同じです |
| `get_request_status`（readOnly） | `{request_ids: string[1..20], wait_seconds?:0-60, include_output?:bool}` | `[{request_id, connection, state, risk, requested_command, executed_command, modified_by_human, decided_by?, decided_at?, reject_reason?, exit_code?, error_kind?, stdout?, stderr?, expires_at}]`。サーバーは状態が変わるか wait_seconds が経つまで応答を保留します（long-poll。sync.Cond で起こします） |
| `get_command_history`（readOnly） | `{host?, state?, limit?=20(上限500), all_agents?:bool}` | stdout を省いた要約です。既定では呼び出し元のエージェントに絞ります。all_agents は Bearer が有効なときだけ使えます |

列挙値は次のとおりです。

- `state`：pending、running、succeeded、failed、rejected、expired、interrupted、auto_running、auto_succeeded、auto_failed
- `error_kind`：connect、auth、hostkey、hostkey_unknown、timeout、exit_nonzero、canceled

挙動の詳細は次のとおりです。

- 同期で応答する上限は RequestTimeout です。これを超えたら `{state:"running", request_id}` を返し、実行は続けます。
- 同時実行の上限を超えたときは、同期では `server_busy` と `retry_after_ms` を返します。
- HTTP が切断されたらキャンセルとみなします（notifications/cancelled は扱いません）。
- 同じ agent_name と client_request_id の組が24時間以内にあれば、新しく作らずに既存の request_id を返します。

### 5.4 状態モデル（保存と接続を分ける）

| 軸 | 値 | 出どころ |
|---|---|---|
| saveState | `saved` / `modified` / `new` | frontend が draft と保存済みの値を比較して算出します |
| connState | `connected` / `connecting` / `idle`（第5段のみ）/ `disconnected` / `error`。シリアルは `in_use` / `available` / `missing` | バックエンドのライブ値です。DB には保存しません |
| lastTest | `{ok, at, message}` | DB の列単位 UPDATE で記録します。connState には影響しません |
| storage | `ok` / `error` | 起動時の openStore の結果です |

承認状態の遷移は次のとおりです。すべて `transitionRequest` の DB CAS を通します。

```
pending → running → succeeded | failed | interrupted
pending → rejected | expired | withdrawn
(bypass) auto_running → auto_succeeded | auto_failed | interrupted
起動時：running / auto_running → interrupted（再承認できない）
```

### 5.5 データモデル（user_version を v2 にする）

- **connections**：`id TEXT PK(UUID)`、`name TEXT UNIQUE`、host、port、user、`auth_method`、`key_path`、`password`（書き込み専用。password 認証のときだけ値を持つ）、`connect_timeout`、`command_timeout`、`max_output_kb`、`tags`、`serial_port`、`baud`、`data_bits`、`parity`、`stop_bits`、`last_test_ok`、`last_test_at`、`last_error`、`sort_order`
- **command_requests**：id、`connection_id`（NULL 可）、`host_name`（当時の名前のスナップショット）、`target_fp`（host/port/user）、`requested_command`、`executed_command`、`original_risk`、`approved_risk`、`state`（コード）、`requested_by`、`client_request_id`、`decided_by`、`decided_at`、`started_at`、`finished_at`、`expires_at`、`exit_code`、`error_kind`、stdout、stderr、`stdout_truncated`、`reject_reason`、`deleted_at`、`created_at INTEGER ms`。インデックスは `(created_at DESC)`、`(state)`、`(requested_by, client_request_id)`
- **agent_policies**：name、bypass、`max_risk`（既定 medium）、`scope_conn_ids`、`granted_at`、`granted_by`、`expires_at`（既定8時間）。第3段で `agent_tokens(name, token_hash, bypass)` を追加します
- **schema_meta**：`legacy_migrated`
- **移行 v1→v2**：先に `VACUUM INTO ssh-gete.sqlite.bak-v1` で退避し、1つのトランザクションの中で次を行います。
  - テーブルを作り直し、id を採番する
  - connection_id は host と name が一致するもので埋める
  - 日本語の status をコードに変換する
  - requested_at は created_at から変換する
  - user_version が既知の値より大きい DB では書き込みを拒否する
- パスフレーズのキャッシュは `(connId, keyPath のハッシュ)` をキーにします。

## 6. 左下サイドバーの確定表記と、接続先の編集画面

### 6.1 サイドバーのワイヤー（JA）

```
[ JA | EN ]
── SSHセッション ──────────────
接続中 2 / 登録 5          エラー 1
● web-01   12分  シェル2 · 実行1
● db-stg    3分  シェル1
▲ router-01  接続エラー
◆ /dev/cu.usbserial-10 @115200  8分     ← シリアル（使用中）
  他 3 件 →
（空のとき）接続中のセッションはありません
── エージェント（MCP）────────
MCP ● 待受中 127.0.0.1:8787
アクティブなエージェント 2（直近5分）
承認待ち 3 →
⚠ 保存先エラー：設定を保存できません   ← storage=error のときだけ出す
```

- 並び順はエラー、実行中あり、接続時刻の新しい順です。最大5行で、それを超える分は「他 N 件」にします。
- 状態は記号（●○▲◆）、アイコン（bi-hdd-network / bi-usb-symbol）、文字の3つで示し、色だけに頼りません。
- 行をクリックしたとき、タブがあればそのサーバーのターミナルタブへ、なければ接続先管理画面でその接続先を選びます。aria-label は「web-01、接続中、12分、シェル2、実行中1件」のようにします。
- 「現在の接続先」と「SSHセッション: <status>」の2行は廃止します。
- 実行中の件数は App の atomic カウンタから出すので、プールがなくても表示できます。

### 6.2 EN 文言

`SSH sessions` / `2 connected / 5 saved` / `1 error` / `12m · 2 shells · 1 running` / `Connection error` / `In use` / `+3 more` / `No active sessions` / `Agents (MCP)` / `MCP ● Listening 127.0.0.1:8787` / `Active agents 2 (last 5 min)` / `Awaiting approval 3` / `Storage error: changes cannot be saved`

語彙のルールとして、「接続」という語は SSH とシリアルにだけ使います。エージェントには「アクティブ」を使います。「切断中」は使わず、「未接続」に統一します。

### 6.3 状態ラベルの辞書

| コード | JA | EN |
|---|---|---|
| saved / modified / new | 保存済み / 未保存の変更あり / 未保存（新規） | Saved / Unsaved changes / Not saved yet |
| connected / connecting / idle / disconnected / error | 接続中 / 接続処理中… / 待機中 / 未接続 / 接続エラー | Connected / Connecting… / Idle / Not connected / Connection error |
| serial in_use / available / missing | 使用中 / 利用可能 / 見つかりません | In use / Available / Not found |
| lastTest | 最終テスト: 成功 10:32 / 失敗 10:32 / 未確認 | Last test: OK 10:32 / Failed 10:32 / Not checked |
| pending / running / succeeded / failed / rejected / expired / interrupted | 承認待ち / 実行中 / 完了 / 失敗 / 拒否 / 期限切れ / 中断（結果不明） | Awaiting approval / Running / Completed / Failed / Rejected / Expired / Interrupted |
| auto_running / auto_succeeded / auto_failed | 自動実行中 / 自動実行済み / 自動実行失敗 | Auto-running / Auto-completed / Auto-failed |

### 6.4 画面上部のグローバルなサーバータブ（要件 E の確定）

```
[すべて] [● web-01 ⧉2 ⏳1] [○ db-stg] [▲ router-01] [◆ usb-serial] …
```

- バッジは接続状態、シェル数（⧉）、承認待ちの件数（⏳）の3つです。
- 選んだタブは `state.serverFilter`（connId）に保存し、全画面で共有します。
- 承認キューと履歴は、選んだサーバーで絞り込みます。どの接続先にも一致しない古いレコードは「すべて」に「不明な接続先」として残し、非表示にはしません。
- 接続先管理画面では、タブを選ぶと一覧の選択が連動します。
- ターミナル画面では、そのサーバーのシェルをサブタブ `web-01 #1 | #2 | ＋新しいシェル` で出します。「すべて」のときは全タブを並べ、`web-01 #2` のようにラベルを付けます。
- ショートカットは、macOS では Cmd、Windows では Ctrl+Shift を組み合わせます。`attachCustomKeyEventHandler` で横取りし、シェルには送りません。

### 6.5 接続先の編集画面（保存と接続のUI）

```
┌ web-01 ────────────── [保存済み|未保存の変更あり|未保存（新規）] [接続中 12分|未接続|接続エラー] ┐
│ 名前 / ホスト / ポート / ユーザー                                                            │
│ 認証方式 (鍵 | パスワード | agent)                                                          │
│   鍵: 鍵パス [~/.ssh/id_ed25519]                                                          │
│   パスワード: [••••••] ✓設定済み [クリア]   ← 空欄のまま保存すると既存の値を維持                    │
│ ホスト鍵: SHA256:xxxx（記録 2026-10-01） [ホスト鍵をリセット]                                    │
│ 最終テスト: 失敗 10:32 — 認証に失敗しました                                                    │
├───────────────────────────────────────────────────────────────────────────────────────────┤
│ [保存] [キャンセル]   |  [接続テスト] [ターミナルを開く] [接続を切断]                                 │
└───────────────────────────────────────────────────────────────────────────────────────────┘
```

- **保存**：isDirty のときだけ押せます。成功したら、返ってきた正規の id と name を選択状態にして draft を捨てます。失敗したら draft を残し、フォーム内にアラートを出します。
- **キャンセル**：新規の draft なら仮の行を消し、直前の選択に戻ります。既存の行なら保存済みの値に戻します。isDirty でなければ押せません。
- **接続テスト**：未保存の値でも実行できます。lastTest の欄だけを更新し、connState は変えません。シリアルではポートを開かずに、available / in_use / missing のどれかを返します。
- **ターミナルを開く**：saveState=saved のときだけ押せます。それ以外のときはツールチップで「先に保存してください / Save changes first」と出します。
- **接続を切断**：connected のときだけ押せます。確認ダイアログに「ターミナル {k} タブと実行中コマンド {j} 件も終了します」と出します。
- isDirty の状態で別の行・新規・別の画面へ移ろうとしたら、「未保存の変更を破棄しますか？［破棄して移動／保存して移動／キャンセル］」と確認します。一覧の行には未保存の印「●」を付けます。
- パスフレーズは、接続テストかターミナルを開く操作で `needsPassphrase:true` が返ったときに、共通のモーダルで入力させます。メッセージ文字列で判定するのはやめます。
- 「新規」は、未保存の新規 draft がすでにあればそれを選ぶだけにします。

## 7. 段階的な実装計画

### フェーズ0：テスト基盤（約1日）
- **変更ファイル**：`.github/workflows/test.yml`（ubuntu/macos/windows の matrix で `go vet`、`go test -race -count=1`。frontend は `npm ci && npm test && npm run build`）、`internal/sshtest/server.go`、mcp_server_test.go のヘルパー、`frontend/package.json`（vitest、jsdom）、`frontend/src/model.js`
- **入れる差し替え口**：App の `emit` / `dial` / `hostKeyCallback` / `now`、`SSH_GETE_NO_SEED`、`serveMCP(listener)`
- **受け入れ条件**：CI が緑であること。newTestApp が HOME、SSH_AUTH_SOCK、SSH_GETE_* を隔離していること。req.Host を `127.0.0.1:8787` に設定済みであること
- **テスト**：sshtest.Server が acceptCount、execLog、maxSessions、KillAll、HostKey の差し替えを持つこと

### フェーズ1：安全性・永続化・語彙（土台）
- **変更ファイル**：mcp_server.go（guardHTTP、OPTIONS、定数時間の比較、rememberMCPAgent の位置、audit、ping、版の交渉、202、isError、説明文）、app.go（transitionRequest、Approve/Reject/BypassAgent、needsHumanApproval、承認省略の取り消し API、env override、store==nil、起動時の interrupted 回収、mcpServer の lifecycleMu、appCtx と wg、shutdown の順序）、storage.go（user_version v2、id、列単位の UPDATE、0600、旧 JSON の一度きり移行、保持ポリシー）、ssh_exec.go（handshake の期限、cappedBuffer、HostKeyAlgorithms、agent の Close、expandPath）、ssh_agent_windows.go、main.js（状態コードの I18N、「接続」ボタンを「接続テスト」に改名し status を書かない、管理画面の「切断」ボタンを削除）
- **受け入れ条件**：
  - Origin 付き、`Origin: null`、Host が `evil`、Content-Type が `text/plain` の各要求が /mcp と /audit で 403 になる
  - 非 loopback で Bearer が OFF なら起動を拒否する
  - 二重承認で execLog がちょうど1件になる。Approve と Reject の競合では、最終状態が一貫している
  - 拒否済みの ID に BypassAgent を呼んでも policies が変わらない
  - High のコマンドは承認省略でもキューに入る
  - 旧 JSON があり、Delete した後に再ロードすると、トークンが空で Enabled=false になる
  - 「実行中」の状態で再起動すると interrupted になる
  - バナーを返さないサーバーでも ConnectTimeout で戻る
  - 出力し続けるコマンドがタイムアウトしても、-race で競合が出ない
- **テスト**：上記を表駆動テストで書き、すべて `-race` 付き・並行系は `-count=20` で回します。

### フェーズ2：マルチタブ（プールなし）とサイドバー
- **変更ファイル**：terminal.go（terms map、frontend が発番する id、セッションごとの入力 goroutine、出力の合流、exit の reason と Once、シリアルの予約表、CloseAll/ByConnection）、app.go（DashboardData.sessions と `storageError`、atomic の実行中カウンタ、`sessions:changed` イベント）、`wails generate module`、main.js（`#terminal-stack`、tabs Map、遷移時の teardown 削除、ResizeObserver、送信キュー、renderSidebarStatus/renderPage の分割、draft と editor.selectedId を id にする、dataGen、6.5 の UI 一式）、style.css（flex 化）
- **受け入れ条件**：
  - 同じサーバーに2タブ、別のサーバーに1タブを開いて画面を移っても、ListTerminalSessions の件数が変わらない
  - 1タブを閉じても他のタブは生きている
  - MOTD を取りこぼさない
  - ターミナル画面にいる間も、サイドバーの数値がポーリングで更新され、xterm の DOM ノードは同じものが使われ続ける
  - 入力中の値がトーストや言語切替で消えない
  - リネームしても行が増えず、選択が追従する
- **テスト**：fake emit を使い、id ごとに出力が混ざらないこと。KillAll すると、そのサーバーのタブにだけ exit が届くこと。vitest で summarizeSessions と connectionDirtyState の表駆動テスト、shell() のスナップショット（JA と EN）。

### フェーズ3：MCP の充実と認証の強化
- **変更ファイル**：mcp_server.go（get_request_status の long-poll、出力の統一、structuredContent、annotations、outputSchema、client_request_id、force_queue、timeout_seconds、r.Context() の伝播、get_command_history の limit とエージェント絞り込み、list_hosts のランタイム項目）、app.go と storage.go（agent_tokens、期限切れ、承認省略の一覧と取り消し UI）、TOFU（専用の known_hosts を 0600 で置く。`knownhosts.Normalize`、Want による判定、GUI 起点のときだけ記録、指紋の表示とリセット）、main.js（承認画面に解決済みの実行先と fp の変化を出す、承認省略の確認ダイアログ）
- **受け入れ条件**：
  - キューに積んだ後、get_request_status で wait_seconds 以内に結果が取れる
  - 同じ client_request_id を再送しても実行は1回だけ
  - 未知のホストに MCP から実行すると hostkey_unknown のエラーになる
  - 鍵が不一致なら常に拒否する
  - 共有トークンだけでは承認省略を名乗れない
- **テスト**：in-process のサーバーで HostKey を差し替えて検証します。long-poll はタイムアウトと状態変化の両方で戻ることを確認します。

### フェーズ4：サーバータブ（E）とシリアルの仕上げ
- **変更ファイル**：main.js（serverFilter、上部タブ、サブタブ、バッジ、ショートカット）、terminal.go（シリアルの setMode をその場で適用、device_removed の判定）、app.go（Serial に変えたら credential を消す、Host 一致から除外）、storage.go（シリアルの通信パラメータ列）
- **受け入れ条件**：タブを切り替えると全画面が絞り込まれ、不明な接続先は「すべて」に残る。USB を抜くとタブに「デバイスが外れました［再接続］」と出る。cu と tty の二重 open を拒否する。シリアルの接続先では MCP の実行・承認・読取がいずれも動かない。
- **テスト**：model.js の filterByServer、ポート名の正規化（表駆動）、シリアル関連のガードの単体テスト。

### フェーズ5（任意。実測で必要になってから）：接続プールとキーチェーン
- §5.1 の仕様どおりに `ssh_pool.go` を作ります。MCP の実行とターミナルの両方を pool の上に載せ、所有権をプールに移します（ターミナルの closer は session.Close と release だけにします）。
- **受け入れ条件**：タブ7つと MCP 4本を同時に動かしても、健全な client は閉じられない（OpenChannelError なら待つか overflow する）。Run の後は再試行しない（execLog が1件のまま）。fp が変わったら drain する。auto の接続は5分で回収される。スリープ復帰を模した KillAll から1秒以内に切断を検知する。
- **テスト**：keepaliveInterval=50ms、maxChannels=3 の注入テスト。

## 8. ユーザー判断が必要な論点

1. **Bearer が OFF のままの既存ユーザーで、承認省略をどう扱うか**
   - (a) 次のリリースで停止する。赤いバナーとワンクリックでのトークン発行を同時に出す。（委員会の推奨）
   - (b) 1リリースは警告だけにして動かし続け、次のリリースで停止する。
   - (c) Bearer OFF でも Low リスクに限って承認省略を許す。
2. **TOFU 導入後、GUI で一度も接続したことのないホストへ MCP から実行した場合**
   - (a) hostkey_unknown で失敗させ、GUI での確認を必須にする。（安全。ただしアップデート直後は既存の運用が止まる）
   - (b) アップデート時に、既存の接続先だけ初回の鍵を自動で記録する猶予期間（例：7日）を設ける。
   - (c) 接続先ごとに「MCP からの初回信頼を許可」というチェックボックスを設ける。（既定は OFF）
3. **承認時にコマンドを修正する機能**
   - (a) 提供する。textarea を置き、元のコマンドとの差分と再分類した risk を表示する。risk が上がった場合は再確認させる。
   - (b) 提供しない。Approve の引数から command を外し、保存済みのコマンドだけを実行する。（すり替えを防げ、実装も小さい。委員会の推奨）