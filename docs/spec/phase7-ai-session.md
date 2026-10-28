# フェーズ7 設計：AI 専用の継続 SSH セッション（REQ-016）

- 対象要件：REQ-016（`docs/spec/ssh-gate-spec.sqlite`）。受け入れ条件は「open_session / send / read / close で状態が持ち越される」「各送信が承認を通り、未承認の行はシェルに届かない」「人のタブには AI から触れない」「出力と送信行が監査に残る」「タイムアウトで閉じる」「人が強制終了できる」。
- 前提にする仕様：design-v1.md §5.0（所有とロック）、§5.2（ターミナル）、§5.3（MCP）、risk.go（許可リスト方式）、approval.go（`transitionRequest` の CAS、`needsHumanApproval`）、terminal.go（`outputBuffer` の合流）。
- 方針：**既存ファイルへの変更は「結線」だけにし、本体は新しいファイルに置く。** 本体で別チームが app.go / storage.go / approval.go / mcp_server.go / terminal.go / main.js を、別ブランチでフェーズ6（headless・Web 管理画面）が進んでいるため。結線の一覧は §9.2 にまとめる。

---

## 0. 結論（要約）

1. 実行方式は2つ。**既定は `exec` モード**（専用の ssh.Client を1本持ち、1行ごとに新しい exec チャネルで `/bin/sh -s` に流す。cwd と環境変数はゲートウェイが保持して毎回の先頭に付ける）。**`shell` モード**（PTY 上の継続シェル）は、人が接続先ごとに許可したときだけ使える。
2. exec モードでは alias・関数・trap・シェルオプションが行をまたいで残らない。「同じ文字列でも意味が変わる」原因は cwd と環境変数だけになり、そのどちらもゲートウェイが知っていて承認画面に出せる。
3. 1行ずつ `validateSessionLine`（改行・制御文字・ヒアドキュメント・行継続などは**キューにも入れずに拒否**）→ `classifySessionLine`（`analyzeCommand` に「セッションで状態を変える構文」の上乗せ）→ `needsHumanApproval`（そのまま）→ セッション固有の追加条件（承認省略は既定で Low まで、汚染されたセッションは全行承認）を通す。条件は**足すだけで、既存の判定を緩めることはない**（例外は、ゲートウェイ自身が解釈する単独の `cd` と安全な名前の `export` だけで、これらは Low にする。§3.2）。
4. 送信行は `command_requests` の1行として記録する。承認キュー・CAS・期限切れ・`get_request_status`・サイドバーの承認待ち件数はそのまま使える。セッションとの対応は新しいテーブル `ai_session_lines` に持つ。
5. 未承認の行が PTY やチャネルに届く経路は1つだけ（`aiSession.startLine`）。これを呼べるのは `pending→running` または `auto_running` の CAS に勝った呼び出しだけ。
6. セッションは1つにつき**同時に1行**まで（承認待ちか実行中の行があれば `session_busy`）。行の順序と、承認時に見せた文脈が実行時にも同じであることを保証する。
7. 所有者は「エージェント専用トークンで認証された名前」だけ。共有トークンや未認証では open できない。ID は `AIS-` で始まり、人のタブ（`TERM-`）とは別の map に置く。AI のツールは `terms` を一切参照しない。
8. 寿命は、アイドル 10 分・最大 1 時間・1エージェント 3 本・全体 16 本（いずれも設定で変えられる）。終了理由は列挙値で記録する。DisconnectConnection、接続先の削除、トークンの失効、アプリの終了で閉じる。
9. 出力はリングバッファ（既定 1 MiB）に入れる。AI 向けにはエスケープシーケンスを除いたテキストを、カーソル（テキストのバイトオフセット、単調増加）で返す。取りこぼしがあれば `missed_bytes` で明示する。秘密らしい文字列は AI 向けと監査では伏せ、生の出力は人の閲覧用にメモリにだけ残す。
10. DB は user_version を 5 に上げ、`ai_sessions`、`ai_session_lines`、`ai_session_events`、`ai_session_output`、`ai_session_settings` を追加する。既存テーブルの列は変えない。
11. GUI：サイドバーの「エージェント（MCP）」欄に「AI セッション n →」を出す。新しい画面「AI セッション」（`frontend/src/aisessions.js`）に、一覧、読み取り専用の xterm、強制終了、設定を置く。

---

## 1. 実行モード

### 1.1 exec モード（既定）

- open_session で専用の `ssh.Client` を開く（§5.0 の所有者は `aiSession`）。開いた直後に `exec env -u BASH_ENV -u ENV /bin/sh -s` を実行し、stdin に `pwd -P` を流して初期 cwd を得る。sh が無いときは `open_failed`（error_kind `no_posix_shell`）にする。
- 1行ごとに新しい exec チャネル（PTY なし）を開き、コマンドとして `exec env -u BASH_ENV -u ENV /bin/sh -s`（`shell:"bash"` のときは `exec env -u BASH_ENV -u ENV bash --noprofile --norc -s`）を送る。**エージェントの行は exec のコマンド文字列には入れず、stdin に流す。** このため、ログインシェルが bash・zsh・fish のどれであっても、ゲートウェイのクォート処理の違いで意味が変わることはない。
- stdin に流す内容（ゲートウェイが組み立て、エージェントの行はちょうど1行だけ入る）：

  ```
  cd -- '<cwd>' 2>/dev/null || { printf '\036SGERR:%s:cwd\036' '<nonce>' >&2; exit 201; }
  export NAME1='value1'          # 環境変数の上乗せ（ゲートウェイがクォートする）
  unset NAME2
  <エージェントの行>
  ```

  stdin を閉じるので、続けて stdin を読むプログラム（`cat`、`read`、REPL、`sudo -S`）は EOF を受け取って終わる。対話プログラムで固まることはない。
- **単独の `cd` 行**（`cd`、`cd <素の語>`、`cd -`）は、ゲートウェイの状態更新として扱う。行の後ろに `&& printf '\036SGCWD:<nonce>:%s\036' "$(pwd -P)"` を付けて実行し、マーカーから新しい cwd を読む。マーカーは出力から除く。
- **単独の `export NAME=値` / `NAME=値` / `unset NAME`** は、ゲートウェイの上乗せ表（`envOverlay`）を更新するだけで、リモートでは何も実行しない（exit_code 0）。値は risk.go の `opaqueCommand` が通す文字（クォートや `$` を含まない）に限る。
- 複合した行の中の `cd`（例：`cd /tmp && ls`）は、その行の中でだけ効く。応答の `cwd_persisted:false` と `notice` で伝える。
- 行のタイムアウトは、`timeout_seconds`（1〜3600。既定は接続先の CommandTimeout、未設定なら 30 秒）。

### 1.2 shell モード（接続先ごとに人が許可したときだけ）

- 専用の Client に PTY（`TERM=dumb`、`ECHO=0`）を要求し、`exec env -u BASH_ENV -u ENV -u PROMPT_COMMAND TERM=dumb /bin/sh -i`（bash のときは `bash --noprofile --norc --noediting -i`）を実行する。rc ファイルを読まないので、開いた時点では alias も関数もない。
- 起動直後に、ゲートウェイが**初期化行**を1回だけ送る（出力は AI の出力には入れない）。`PS1='\036SG:<session-nonce>:$?\036'`、`PS2='\036SG2:<session-nonce>\036'`、`unset PROMPT_COMMAND`、`trap - DEBUG 2>/dev/null`。PS1 と PS2 は export しない。nonce はセッションごとに `crypto/rand` で作る 128 bit の値で、AI には返さない。
- 行の終了は、PS1 のマーカーが出たことで判定し、`$?` を exit_code とする。PS2 のマーカーが出たら（クォートの閉じ忘れなどで、続きの入力を待っている）、すぐに Ctrl-C を送って打ち消す。その行は `failed`（error_kind `incomplete_line`）にする。マーカーは AI 向けの出力からも監査からも除く。
- マーカーが来ない間（前面でプログラムが動いている、stdin を読んでいる、など）は `busy` とし、送れるのは `interrupt` と `close_session` だけにする。**こうして、行は必ずシェルのプロンプトに届く。** プロンプトが戻っていないのに次の行を送ると、それが前面のプログラム（mysql や python など）の入力として解釈されてしまうので、それを防ぐ。
- ECHO=0 なので入力はエコーされない。ゲートウェイが `$ <行>\n` を出力の流れに書き足し、読める記録にする（`gateway_echo` の印を付け、監査では区別できるようにする）。
- 残るリスク：人が承認した High の行（例：`python3`）で起動したプログラムが nonce を知り、偽のプロンプトを出す可能性はある。nonce を読む手段（`set`、`declare`、`echo $PS1` など）はすべて High なので、人の承認を経ない限り起きない。これは承認画面の注意書きに明記する。

### 1.3 どちらを使うか

| 観点 | exec（既定） | shell（許可制） |
|---|---|---|
| cd・環境変数の持ち越し | ゲートウェイが持つ（REQ-016 を満たす） | シェルが持つ |
| alias・関数・trap・set | 行をまたいで残らない | 残る。定義する行は High、承認後はセッションを「汚染」扱いにする |
| 対話プログラム | stdin が EOF になるので固まらない | busy になる。interrupt で戻す |
| 終了コード | チャネルの exit-status | PS1 マーカーの `$?` |
| 使う場面 | ほぼすべて | `source venv/bin/activate` のように、シェルの状態がどうしても要るとき |

- open_session の `mode` の既定は `exec`。`shell` を指定しても、その接続先が `AISessionSettings.ShellConnIDs` に入っていなければ `shell_mode_not_allowed` を返す。

---

## 2. MCP ツール

§5.3 の共通の決まり（isError、structuredContent、annotations、outputSchema、`additionalProperties:false`、エラーの候補名）をそのまま使う。新しいツールは `mcp_aisession.go` の `aiSessionTools()` で定義し、`mcpTools()` の末尾に連結する（結線は1行）。

### 2.1 共通

- **呼び出し元**：エージェント専用トークンで認証された `caller.Agent` が必須。空（共有トークン、または Bearer が OFF の loopback）なら `agent_token_required` を返す。`agent_name` の引数は受け取らない（名乗りで所有者を決めないため）。
- **機能が OFF のとき**：`AISessionSettings.Enabled=false`（既定）なら、すべてのツールが `ai_session_disabled` を返す。tools/list には常に載せる（説明文に「GUI で有効にする必要がある」と書く）。
- **session_id**：`AIS-` に続けて base32 で 26 文字（128 bit の乱数）。この形に合わない ID は `session_not_found` にする（`TERM-` の ID を渡してもタブの存在は分からない）。
- **所有者の検査**：見つかったセッションの owner と `caller.Agent` が違えば `not_owner`。ID は推測できないので、存在を知られることの害は小さい。調べやすさを優先する。
- **閉じたセッション**：閉じてから 5 分は墓標として残し、`read_output` で最後の出力を読めるようにする（それ以外のツールは `session_closed` と `close_reason` を返す）。5 分を過ぎたら `session_not_found`。

### 2.2 open_session（destructive, openWorld）

入力：

```json
{ "host": "web-01", "reason": "nginx の設定を調べる", "mode": "exec|shell", "shell": "sh|bash",
  "client_request_id": "任意。再送したとき同じセッションを返す" }
```

- `agent_name` は受け取らない（トークンで決まる）。`reason` は必須（1〜500 文字）。監査と GUI の一覧に出す。
- 接続には MCP と同じ設定（StrictHostKey、`mcpCredentialTimeout`）を使う。未知のホスト鍵なら `connect_failed`（error_kind `hostkey_unknown`）。パスフレーズ付きの鍵でキャッシュが無ければ `connect_failed`（error_kind `auth`、`needs_passphrase:true`）。
- 開くこと自体は承認を要さない（何も実行しないため）。ただし、上限、接続先の種別、shell の許可は検査する。

出力：`{session_id, connection, mode, shell, state:"idle", cwd, opened_at, idle_timeout_s, expires_at, output_cursor:0, limits:{per_agent, total, ring_bytes}}`

エラー：`host_not_found`、`ambiguous_host`、`serial_not_supported`、`shell_mode_not_allowed`、`session_limit`（`scope: per_agent|total`、`retry_after_ms` は付けない）、`connect_failed`（error_kind は §5.3 の値と `no_posix_shell`）、`agent_token_required`、`ai_session_disabled`、`shutting_down`。

### 2.3 send_line（destructive, openWorld）

入力：

```json
{ "session_id": "AIS-…", "line": "systemctl status nginx", "client_request_id": "必須",
  "reason": "任意", "wait_seconds": 0-60, "timeout_seconds": 1-3600, "force_queue": false }
```

- `client_request_id` は**必須**。状態を持つセッションで同じ行が二重に実行されると結果が変わるため。同じエージェントと client_request_id の組が 24 時間以内にあれば、既存の request_id を返す（`deduplicated:true`）。ただし session_id か line が違えば `idempotency_conflict` を返す。
- 処理の順番：
  1. 所有者・状態を検査する（`session_closed` / `not_owner` / `session_busy`）
  2. `validateSessionLine`。NG なら `line_rejected`（キューには入れず、`ai_session_events` に記録する）
  3. `classifySessionLine`
  4. `needsHumanApproval` とセッションの追加条件（§3.4）で判定する
  5. 承認が要れば `queueCommand`、要らなければ `auto_running` を永続化してから startLine
- `wait_seconds`（既定 10）：自動実行したときは、行が終わるか wait_seconds が経つまで待ってから応答する。承認待ちのときは待たずにすぐ返す（承認を待つのは `get_request_status` の役目）。

出力：

```json
{ "request_id": "REQ-…", "session_id": "AIS-…", "seq": 7, "state": "pending|auto_running|auto_succeeded|auto_failed",
  "risk": "Low|Medium|High", "risk_reasons": ["env_overlay_unsafe"], "requires_approval": true,
  "effect": "none|cd|env|state_change", "cwd": "/var/www", "cwd_persisted": true,
  "cursor_before": 10240, "cursor_after": 10802, "exit_code": 0,
  "output": "（この行の出力テキスト。上限 16 KiB）", "output_truncated": false,
  "session_state": "idle|busy|pending_approval", "expires_at": "…",
  "next_action": "call get_request_status | call read_output" }
```

エラー：`session_not_found`、`session_closed`（`close_reason` 付き）、`not_owner`、`session_busy`（処理中の行の `request_id` と `state` 付き）、`line_rejected`（`reject_reason` 付き。値は §3.1）、`idempotency_conflict`、`target_changed`（接続先の user@host:port が変わった。このときセッションも閉じる）、`agent_token_required`、`ai_session_disabled`、`storage`。

### 2.4 read_output（readOnly）

入力：`{session_id, since_cursor?: int (省略時はリングの先頭), max_bytes?: 1-65536 (既定 16384), wait_seconds?: 0-30}`

- カーソルは、**AI 向けテキストの流れ**における、セッション開始からのバイトオフセット（uint64 で単調増加）。テキストは UTF-8 の文字の境目でしか切らない。
- `since_cursor` がリングの先頭より前なら、リングの先頭から返し、`missed_bytes` に失った量を入れる（黙って捨てない）。`since_cursor` が末尾より後なら `invalid_cursor` を返す。
- `wait_seconds>0` のときは long-poll にする。新しい出力が来るか、処理中の行の状態が変わるか、セッションが閉じるまで待つ（`sync.Cond` で起こす。同時に待てる数は `mcpRuntime.acquireWaiter` の枠を共有する）。

出力：`{text, start_cursor, cursor (次の since_cursor), end_cursor, missed_bytes, more:bool, session_state, close_reason?, line:{request_id, seq, state, exit_code?}, cwd, redactions:int}`

エラー：`session_not_found`、`not_owner`、`invalid_cursor`、`server_busy`（long-poll の枠が足りない）。

### 2.5 interrupt（destructive, idempotent）

入力：`{session_id}`

- **承認は要らない。** 理由：Ctrl-C は新しいコマンドを持ち込まず、止める方向にしか働かない。INT の trap を仕掛ける行は High なので、もし仕掛けられていても人が承認したものに限られる。人が承認した行を AI が止められないと、`tail -f` などが寿命まで残ってしまう。
- exec モード：処理中のチャネルに `Signal(ssh.SIGINT)` を送る。3 秒たっても終わらなければチャネルを閉じ、行は `failed`（error_kind `canceled`）にする。shell モード：PTY に `0x03` を書く。これは `startLine` を通らない唯一の書き込みで、書けるバイトは `0x03` に固定する（`writeInterrupt`）。5 秒たってもプロンプトが戻らなければ、応答に `hint:"call close_session"` を付ける。
- 1 秒に 1 回までにし、超えたら `interrupt_rate_limited` を返す。毎回 `ai_session_events`（kind `interrupt`）に記録する。
- 処理中の行が無いときは何もしない（`interrupted:false`）。承認待ちの行は取り消さない（取り消すなら close_session を使う）。

出力：`{session_id, interrupted:bool, line:{request_id, state}}`

### 2.6 close_session（destructive, idempotent）

入力：`{session_id}`。出力：`{session_id, closed:true, close_reason:"agent_closed", lines, output_bytes}`。閉じていれば、そのときの close_reason を返す（エラーにはしない）。

### 2.7 list_sessions（readOnly）

入力：`{include_closed?: bool}`。出力：`{sessions:[{session_id, connection, mode, state, cwd, opened_at, last_activity_at, idle_expires_at, expires_at, lines, pending_request_id?, close_reason?}]}`

- 呼び出し元が所有するものだけを返す。人のターミナルタブと他のエージェントのセッションは返さない。
- design-v1 §3 で却下した「接続一覧としての `list_sessions`」とは別物。こちらは AI セッションだけを列挙する（ツールの説明文にもそう書く）。

### 2.8 get_request_status との関係

- 送信行は `command_requests` の1行なので、`get_request_status(request_ids, wait_seconds)` で承認と実行を待てる（変更は不要）。`requestView` に `session_id` と `session_seq` を足すだけにする（結線）。
- 行の出力（`stdout`）は、行が始まってから終わるまでのテキストを MaxOutputKB で切ったもの。セッション全体の流れは read_output で読む。
- エージェントの典型的な流れ：`send_line` → `pending` なら `get_request_status(wait_seconds=60)` を繰り返す → `succeeded` / `failed` になったら `read_output(since_cursor=cursor_before)`。

### 2.9 状態の列挙

- session_state：`opening`、`idle`、`pending_approval`、`busy`、`closing`、`closed`
- close_reason：`agent_closed`、`human_closed`、`idle_timeout`、`max_lifetime`、`disconnected`（DisconnectConnection）、`connection_deleted`、`target_changed`、`token_revoked`、`ai_session_disabled`、`remote_exit`、`conn_lost`、`app_shutdown`、`app_restart`（起動時の回収）、`open_failed`

---

## 3. 承認

### 3.1 行の検査 `validateSessionLine`（キューに入れず拒否）

次のどれかに当たったら `line_rejected` を返す。

| reject_reason | 条件 |
|---|---|
| `empty` | 空白を除くと空 |
| `too_long` | 4096 バイト超 |
| `invalid_utf8` | UTF-8 として不正 |
| `multiline` | `\n`、`\r`、U+2028、U+2029、U+0085 を含む |
| `control_char` | `\t` 以外の C0 / DEL / C1 制御文字、Cf（ゼロ幅など）を含む。Ctrl-C も含む（Ctrl-C は interrupt で送る） |
| `heredoc` | 引用の外にある `<<`（`<<-`、`<<<` を含む） |
| `line_continuation` | 引用の外で行末が `\`、または `|`、`&&`、`||` で終わる |
| `unbalanced_quote` | `'`、`"`、`` ` ``、`$(`、`(`、`{` の対応が取れない（小さな字句解析で判定） |

- 拒否は `ai_session_events` に `line_rejected`（行の先頭 200 字と reason）として残す。command_requests には入れない（承認の対象ではないため）。
- 要件文の「High として拒否」は、「キューにも入れない」ことと読み替える。High でキューに入れると、人が承認してしまえる（複数行の入力は shell モードで行の区切りを壊すため、承認しても安全に流せない）。

### 3.2 分類 `classifySessionLine(line, st sessionRiskState) sessionLineClass`

`analyzeCommand(line)` の結果に、次を上乗せする。下げるのは、表の最初の2行（ゲートウェイが自分で解釈する単独の `cd` と、安全な名前の `export` / `unset`）だけにする。exec モードでは、env の行はリモートで何も実行しない。cd の行は `cd` と `pwd -P` しか実行しない。そのため、Low にしても副作用は増えない。

| 対象 | リスク | 効果（effect） |
|---|---|---|
| 単独の `cd` / `cd <素の語>` / `cd -` | Low（risk.go 単体では Medium） | `cd` |
| 単独の `export N=v` / `N=v` / `unset N`、N が安全な名前（`LANG`、`LC_*`、`TZ`、`NO_COLOR`、`COLUMNS`、`LINES`、`TERM`） | Low | `env` |
| 上と同じ形で、N が危険な名前 | High | `env` |
| 上と同じ形で、その他の名前 | Medium。以後セッションに `env_overlay_unsafe` を立てる | `env` |
| `alias`、`unalias`、関数の定義、`set`、`shopt`、`trap`、`enable`、`builtin`、`hash`、`bind`、`complete`、`declare`、`typeset`、`readonly`、`local`、`export -f/-n/-p`、`stty`、`ulimit`、`umask`、`pushd`、`popd`、`exec` | High | `state_change` |
| 背景で続くもの・セッションの外で生き残るもの：`setsid`、`disown`、`screen`、`tmux`、`at`、`batch`、`systemd-run` | High | — |
| `nohup` | High（セッションでは寿命を超えて残るため、risk.go の Medium から上げる） | — |
| 先頭の `VAR=v cmd`（その行だけの代入）で VAR が危険な名前 | High | — |

- 危険な名前：`PATH`、`LD_*`、`DYLD_*`、`BASH_ENV`、`ENV`、`PROMPT_COMMAND`、`PS0`〜`PS4`、`IFS`、`CDPATH`、`SHELLOPTS`、`BASHOPTS`、`HISTFILE`、`PAGER`、`GIT_*`、`MANPAGER`、`EDITOR`、`VISUAL`、`LESSOPEN`、`LESSCLOSE`、`PYTHON*`、`PERL5*`、`RUBYOPT`、`NODE_OPTIONS`、`SSH_ASKPASS`、`SUDO_ASKPASS`、`TMPDIR`。
- `env_overlay_unsafe` が立っている間は、全行の**リスクの下限を Medium** にする（環境変数で振る舞いが変わる Low のコマンドを、承認省略で通さないため）。`unset` でその名前を外せば、下限も戻る。
- 戻り値：`{Risk, Reasons []string, Effect, CdTarget, EnvSet map, EnvUnset []string, UsesSudo}`。Reasons は `cd_builtin`、`env_overlay`、`env_overlay_unsafe`、`dangerous_env`、`shell_state_change`、`detaches`、`session_tainted`、`base:<risk.go の結果>` のような英字のコード。承認画面で JA/EN に訳して出す。

### 3.3 needsHumanApproval はそのまま使う

- 呼び出しは `a.needsHumanApproval(conn, risk, line, caller, caller.Agent, forceQueue)`。risk は §3.2 で上乗せした値を渡す。High は必ずキュー、`RequireApprovalSudo`（`commandUsesSudo(line)`）、`RequireApprovalProd`、`RequireApprovalWriteOp`、エージェントの承認省略の範囲（max_risk、scope_conn_ids、期限、専用トークン）は、すべて既存の規則どおりに効く。

### 3.4 セッション固有の追加条件（承認を足すだけ）

`sessionNeedsApproval(s, class)` が true なら、`needsHumanApproval` が false でもキューに入れる。

1. `class.Risk` が `AISessionSettings.BypassMaxRisk`（既定 `Low`。`Medium` まで上げられる。High は選べない）を超えるとき。継続セッションでは副作用が積み重なり、Medium の自動実行が execute_command よりも危ないため。
2. セッションが `tainted` のとき。shell モードで `state_change` の行が承認された後は、セッションが閉じるまで全行を承認制にする。exec モードでは、`state_change` の行は残らないので汚染しない。
3. `effect=env` で、名前が安全な名前ではないとき。
4. 承認省略の付与（`ApproveCommandAndBypassAgent`）は、セッションの行からでも行える。ただし付与される範囲は既存のエージェント単位のもので、セッションでは上の 1〜3 が常に優先する。

### 3.5 未承認の行は PTY に一切書かない（不変条件）

- 行をリモートに渡す関数は `(*aiSession).startLine(req CommandRequest)` だけ。呼んでよいのは次の2か所だけにする。
  - `sendLineTool` で、`auto_running` を記録（recordCommandRequest）した直後
  - `approveAISessionLine` で、`transitionRequest(pending→running)` が成功した直後
- `startLine` は `s.mu` の下で `s.inflight == req.ID`、`s.state != closing/closed`、`req.Status ∈ {running, auto_running}` を確かめる。どれかが違えば、書かずに `finishRequest(failed, canceled)` にする。
- shell モードで、行以外に PTY へ書くのは、初期化行（open 時。AI の入力を含まない）と `0x03`（interrupt）と、PS2 を検知したときの `0x03` の3つだけ。これらは `writeControl` にまとめ、書けるバイト列を固定する。
- テスト：fake の transport が受け取ったバイト列を記録し、「承認待ち・拒否・期限切れ・withdrawn の行が1バイトも届かない」「Approve と Reject、Approve と close を並行させても、届くのは高々1回」を `-race -count=50` で確かめる。

### 3.6 承認の経路（approval.go への結線）

- `approve()` の先頭で `if link, ok := a.aiLineLink(id); ok { return a.approveAISessionLine(id, command, grantBypass, link) }` と分岐する（結線は1行）。`RejectCommand` と `expireRequest` は変えない。セッション側は「処理中の行の状態」をその都度 `findRequest` で確かめるので、拒否や期限切れのときにコールバックは要らない。
- `approveAISessionLine` の手順：
  1. `command` が空か `req.Command` と同じであることを確かめる。違えば `edit_not_allowed`（AI セッションの行は修正して承認できない。修正すると、AI が持っている文脈と食い違うため）。
  2. セッションが開いていて、`inflight == id` であることを確かめる。閉じていれば `session_closed`。
  3. `targetFingerprint(conn) == session.targetFP` を確かめる。違えば `target_changed`（セッションも閉じる）。
  4. `beginExec(a.appCtx, conn.ID)` で実行を登録する（DisconnectConnection と終了時の drain の対象になる）。
  5. `transitionRequest(pending→running, DecidedBy=gui)` の CAS が成功したら `startLine`。
  6. 行が終わったら `finishRequest`。
- 承認待ちの期限：`ExpiresAt = min(now + pendingRequestTTL, session.expiresAt)`。セッションが閉じたときは、承認待ちの行を `withdrawn` にする（`DecidedBy=system`）。

### 3.7 承認画面に出すもの

`GetAISessionLineContext(requestID) AISessionLineContext` を新しく作る（approvals.js からは、行が `sessionId` を持つときだけ呼ぶ）。

- 見出し：「AI セッション内の行」/「Line in an AI session」、session_id、エージェント名、モード（exec / shell）、セッション内の何行目か
- 実行時の文脈：cwd、環境変数の上乗せ（値は 64 文字で切る。名前に TOKEN / SECRET / PASSWORD / KEY を含む値は伏せる）、`tainted` の警告
- 直前の 3 行：行、状態、exit_code
- 直前の出力の末尾 20 行（AI 向けと同じテキストで、秘密は伏せる）
- 分類の理由（Reasons を JA/EN に訳したもの）
- コマンドの修正欄は出さない（読み取り専用）。「承認して承認省略も許可」は出すが、§3.4 の上限を説明文に出す

---

## 4. セッションの所有と分離

- `aiSessionState` を App に埋め込む。`aiMu sync.Mutex`、`aiSessions map[string]*aiSession`、`aiTombstones map[string]aiSessionTombstone`、`aiByIdempotency map[string]string`。`terms`・`serialOwners` とは別の map にし、`termMu` も使わない。
- `aiSession` の主なフィールド：`id`、`owner`（`caller.Agent`）、`connID`、`connName`、`targetFP`、`mode`、`shell`、`client *ssh.Client`（このセッションが所有する）、`transport aiTransport`、`mu`（状態用）、`state`、`cwd`、`env envOverlay`、`tainted`、`inflight`（request ID）、`seq`、`openedAt`、`lastActivity`、`expiresAt`、`out *aiOutput`、`closeOnce`、`done chan struct{}`、`closeReason`。
- **AI から人のタブへの経路は無い**：AI のツールは `aiSessions` だけを引く。`SendTerminalInput`、`ResizeTerminal`、`CloseTerminal`、`ListTerminalSessions` は `terms` だけを引き、`AIS-` の ID は登録されない。テストで「`TERM-` の ID を send_line に渡すと `session_not_found`」「`AIS-` の ID を SendTerminalInput に渡すと `no_session`」「list_sessions に人のタブが出ない」を確かめる。
- **人の操作**（Wails のバインディングで、フェーズ6の Web 管理画面からも同じ名前で呼べる）：`ListAISessions`、`GetAISession`、`ReadAISessionRaw`、`CloseAISession`、`CloseAllAISessions`、`GetAISessionLineContext`、`GetAISessionSettings`、`SaveAISessionSettings`。人は**入力できない**（人の入力が混ざると、AI の文脈と承認の前提が崩れるため。人が操作したいときは、普通のターミナルタブを開く）。
- エージェント用のトークンでは、これらのバインディングは呼べない（MCP の経路に出さない。フェーズ6の Web 管理 API では、管理者のログインが必要な側に置く）。

---

## 5. 寿命

| 項目 | 既定 | 範囲 | 設定名 |
|---|---|---|---|
| 機能の有効化 | OFF | — | `Enabled` |
| アイドルタイムアウト | 10 分 | 1〜120 分 | `IdleMinutes` |
| 最大寿命 | 60 分 | 5〜480 分 | `MaxMinutes` |
| 1 エージェントあたりの同時数 | 3 | 1〜8 | `PerAgent` |
| 全体の同時数 | 16 | 1〜64 | `Total` |
| 承認省略の上限 | Low | Low / Medium | `BypassMaxRisk` |
| shell モードを許す接続先 | なし | connection id の配列 | `ShellConnIDs` |
| リングバッファ | 1 MiB | 64 KiB〜8 MiB | `RingKB` |
| 監査に残す出力 | 1 MiB / セッション | 0〜16 MiB | `AuditKB` |

- 「アイドル」は、所有者のツール呼び出し（send_line / read_output / interrupt）が無く、かつ処理中の行が無い状態とする。承認待ちの行がある間はアイドルとみなさない（人の判断を待っている間に閉じないため）。ただし最大寿命は必ず効く。
- 掃除役 `runAISessionSweeper(ctx)` を `a.wg` で起こし、5 秒ごとに次を確かめて閉じる：アイドル、最大寿命、所有者のトークンが失効したか（`agentHasToken`）、`Enabled=false`。あわせて墓標を 5 分で消す。ssh の keepalive（30 秒ごと、2 回続けて失敗したら `conn_lost`）は、セッションごとの goroutine で行う。
- **閉じる手順** `closeAISession(id, reason, by)`（`closeOnce` で 1 回だけ）：
  1. `s.mu` の下で `state=closing` と close_reason を決める。ここで処理中の行と transport を取り出す。
  2. ロックを外してから、処理中の行を片付ける。承認待ちなら `withdrawn` に、実行中なら `failed`（`canceled`）に、アプリ終了なら `interrupted` にする。いずれも `transitionRequest` の CAS を通す。
  3. transport を閉じ、client を閉じる（所有者だけが Close する。§5.0）。
  4. `ai_sessions.closed_at`・close_reason・closed_by を書き、`ai_session_events` に `close` を書く。最後の出力を監査に書き出す。
  5. `aiMu` の下で map から外し、墓標を作る。`aisessions:changed` と `sessionsChanged()` を出す。
- **アプリの終了**（§5.0 の順序に1段足す）：`appCancel` → `cancelExecs` → `stopMCPServer` → `drainMCPCommands` → **`closeAllAISessions(app_shutdown)`（各セッションの手順 4 まで、合計最大 2 秒待つ）** → `CloseAllTerminals` → `wg.Wait` → `store.Close`。行の実行は `beginExec` に登録しているので、手前の drain の段で `interrupted` が記録される。
- **起動時の回収**：`ai_sessions.closed_at IS NULL` の行を `app_restart` で閉じる。`ai_session_lines` に紐づく `pending` の要求は `withdrawn` にする（そのまま残すと、セッションが無いのに承認できてしまう）。`running` の要求は、既存の回収で `interrupted` になる。
- **DisconnectConnection**：その接続先の AI セッションも `disconnected` で閉じ、メッセージに「AI セッション n 件」を足す。確認ダイアログ（§6.5）の文にも件数を出す。**DeleteConnection** は `connection_deleted`。**SaveConnection** でホストやユーザーが変わったときは、フックを入れずに、次の send_line で fp の不一致を見つけた時点で `target_changed` として閉じる（掃除役も確かめる）。
- **ロックの規約**（§5.0 に追記）：`aiMu` と各セッションの `s.mu` と出力用の `out.mu` は、それぞれ単独で持つ。`a.mu`、`termMu`、`execMu`、`sessionsMu` と同時に持たない。`transitionRequest`、`emitEvent`、`sessionsChanged`、ネットワークの I/O は、どのロックも持たずに呼ぶ。

---

## 6. 出力

### 6.1 流れ

```
transport の stdout / stderr（PTY なら1本）
  → aiOutput.Write(raw)
       ├ raw リング（RingKB）              … 人の閲覧用。rawCursor で読む。DB には書かない
       ├ ANSI 除去（状態を持つ stripper） → 秘密の検知（行単位） → text リング（RingKB）  … AI 向け。cursor で読む
       └ 監査用の書き出し（text と同じもの。AuditKB まで）
  → aisession:data イベント（16ms または 32KB ごとにまとめる。terminal.go の outputBuffer と同じ方式）
```

- `outputBuffer` は流用しない（あれは frontend への送信だけを担い、溢れたら捨てる）。新しい `aiOutput` は、2つのリングとカーソル、`sync.Cond` を持つ。
- exec モードでは、stdout と stderr を届いた順に1本の流れにまとめる。行ごとの記録（command_requests の stdout / stderr）は別々に持つ。

### 6.2 エスケープシーケンスの扱い（`stripTerminal`）

- 取り除くもの：CSI（`ESC [ … 終端`）、OSC（`ESC ] … BEL|ST`）、DCS / SOS / PM / APC（`… ST`）、そのほかの2バイトの ESC シーケンス、`\t` と `\n` 以外の C0 制御文字、C1 制御文字（U+0080〜U+009F）。
- `\r\n` は `\n` にする。単独の `\r` は、その行のそれまでの内容を捨てる（進捗表示の上書き）。`\b` は直前の 1 文字を消す。
- シーケンスがチャンクの境目をまたいでもよいように、状態はセッションごとに持つ。OSC や DCS の長さが 4 KiB を超えたら打ち切り、`[ssh-gate: oversized escape dropped]` を挿入する。
- 人の閲覧用の xterm は raw を表示する。ただし `disableStdin:true` にし、OSC 52（クリップボード）を処理するアドオンも、リンクを開くアドオンも読み込まない。タイトルの変更（OSC 0/2）は無視する。

### 6.3 秘密の文字列（`redactSecrets`）

- 検知するもの：PEM の秘密鍵ブロック（BEGIN〜END の全体）、AWS のアクセスキー（`AKIA…` / `ASIA…`）と secret らしい 40 文字、GitHub のトークン（`ghp_` `gho_` `ghs_` `github_pat_`）、Slack（`xox[abpr]-`）、JWT（`eyJ…\.…\.…`）、`Authorization: Bearer …`、`password=` / `passwd:` / `secret=` / `token=` の値、URL 中の `user:pass@`。
- 置き換え：`[REDACTED:<kind>:<sha256 の先頭 8 桁>]`。同じ値は同じ印になるので、AI は「同じ秘密が2回出た」ことは分かるが、値は分からない。
- 適用する範囲：
  - AI 向けのテキスト（read_output と send_line の `output`、command_requests の stdout / stderr）：伏せる。
  - 監査（`ai_session_output`、command_requests）：伏せたものだけを書く。生の値は DB にも WAL にも残さない。
  - 人の閲覧用の raw リング：伏せない（人の手元のメモリにだけあり、セッションを閉じて墓標が消えたら捨てる）。
  - `ai_session_events` に `secret_redacted`（kind、行の seq、件数）を残す。GUI の一覧には「秘密を n 件伏せました」と出す。
- 行そのもの（送信したコマンド）は伏せずに記録する。承認する人が、実行されるものをそのまま見る必要があるため。ただし、環境変数の上乗せを表示するときは、名前に TOKEN / SECRET / PASSWORD / KEY を含む値を伏せる。
- 行単位で検知するので、PEM のような複数行のブロックは、BEGIN の行から END の行まで（最大 64 行）を保留してから判定する。保留している分は、read_output にもまだ出さない（`more:false` のまま、保留が解けたら出す）。

### 6.4 上限

- テキストのリングも raw のリングも、溢れたら古い分から捨てる。読み手のカーソルが捨てた範囲にあれば、`missed_bytes` で知らせる。
- 監査の書き出しは `AuditKB` まで。超えたら `ai_session_events` に `output_truncated`（その時点のカーソル）を1回だけ書き、以後は `ai_sessions.output_bytes` の合計だけを更新する。
- 行ごとの stdout は `MaxOutputKB`（既存の設定）まで。

---

## 7. 監査と DB（user_version 5）

### 7.1 方針

- **送信行は command_requests に入れる**。承認の状態機械、CAS、期限、`get_request_status`、承認キューの UI、サイドバーの件数、サーバータブの ⏳ を作り直さずに済む。command_requests の列は変えない（`requested_command` に行を、`reason` に send_line の reason か open_session の reason を入れる）。
- セッション固有の情報は、新しいテーブルに置く。CommandRequest 構造体に足すのは、メモリ上だけの `SessionID string json:"sessionId,omitempty"` と `SessionSeq int json:"sessionSeq,omitempty"` の2つで、起動時に `ai_session_lines` から埋める。storage.go の INSERT / SELECT は変えない。

### 7.2 テーブル（`storage_aisession.go`）

```sql
CREATE TABLE IF NOT EXISTS ai_sessions (
  id TEXT PRIMARY KEY,                 -- AIS-…
  agent_name TEXT NOT NULL,
  connection_id TEXT NOT NULL,
  host_name TEXT NOT NULL,             -- 開いた時点の名前
  target_fp TEXT NOT NULL,             -- user@host:port
  mode TEXT NOT NULL,                  -- exec | shell
  shell TEXT NOT NULL,                 -- sh | bash
  reason TEXT NOT NULL,
  client_request_id TEXT NOT NULL DEFAULT '',
  opened_at INTEGER NOT NULL,          -- ms
  closed_at INTEGER,                   -- NULL = 開いている（起動時に app_restart で回収）
  close_reason TEXT NOT NULL DEFAULT '',
  closed_by TEXT NOT NULL DEFAULT '',  -- agent:<name> | gui | system
  line_count INTEGER NOT NULL DEFAULT 0,
  output_bytes INTEGER NOT NULL DEFAULT 0,
  stored_output_bytes INTEGER NOT NULL DEFAULT 0,
  redactions INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS idx_ai_sessions_open ON ai_sessions(closed_at);
CREATE INDEX IF NOT EXISTS idx_ai_sessions_agent ON ai_sessions(agent_name, opened_at DESC);

CREATE TABLE IF NOT EXISTS ai_session_lines (
  request_id TEXT PRIMARY KEY,         -- command_requests.id
  session_id TEXT NOT NULL,
  seq INTEGER NOT NULL,
  cwd TEXT NOT NULL,                   -- 実行時の文脈（承認画面に出したもの）
  env_json TEXT NOT NULL,              -- 上乗せの表。秘密らしい値は伏せて保存する
  effect TEXT NOT NULL,                -- none | cd | env | state_change
  risk_reasons TEXT NOT NULL,          -- JSON 配列
  tainted INTEGER NOT NULL DEFAULT 0,
  cursor_start INTEGER NOT NULL,
  cursor_end INTEGER,
  UNIQUE (session_id, seq)
);

CREATE TABLE IF NOT EXISTS ai_session_events (
  session_id TEXT NOT NULL,
  seq INTEGER NOT NULL,
  at INTEGER NOT NULL,
  kind TEXT NOT NULL,                  -- open | line_rejected | interrupt | secret_redacted | output_truncated | tainted | close
  detail_json TEXT NOT NULL,
  PRIMARY KEY (session_id, seq)
);

CREATE TABLE IF NOT EXISTS ai_session_output (
  session_id TEXT NOT NULL,
  chunk_seq INTEGER NOT NULL,
  cursor_start INTEGER NOT NULL,       -- text カーソル
  text TEXT NOT NULL,                  -- ANSI 除去・秘密を伏せた後のもの
  created_at INTEGER NOT NULL,
  PRIMARY KEY (session_id, chunk_seq)
);

CREATE TABLE IF NOT EXISTS ai_session_settings (
  id INTEGER PRIMARY KEY CHECK (id = 1),
  payload_json TEXT NOT NULL,
  updated_at TEXT NOT NULL
);
```

- 設定を MCPSettings に入れず、別のテーブルにする理由：MCPSettings は本体側で `SaveMCPSettings` が全体を保存しており、フィールドを足すと競合しやすいため。
- 出力の書き出しは、64 KiB または 2 秒ごとにまとめて1つのトランザクションで行う（`writeMu` を通す）。行の開始と終了、イベント、close はその都度書く。順序は「DB に書けたらメモリに反映する」（§5.0）。ただし出力のチャンクだけは、メモリのリングが先になる（AI への遅延を避けるため）。DB への書き込みに失敗したら `storage_error` イベントを出し、そのセッションの以後の send_line を `storage` エラーで止める（監査の無い実行はしない）。

### 7.3 user_version の扱い

- `schemaVersion` を 4 から 5 に上げる。`migrate()` の CREATE 文の後で `migrateAISessionTables(tx)` を呼ぶ（結線は1行）。すべて `CREATE … IF NOT EXISTS` なので、何度呼んでも同じ結果になる。v4 → v5 で既存の行は変わらない。既存の `backupBeforeMigration` が、移行の前に退避を作る。
- フェーズ6が先に 5 を使ったときは、マージの時点で 6 に繰り上げる（どちらも CREATE だけなので、番号の付け替えで済む）。
- v5 の DB を古いアプリで開くと、既存の「既知の版より新しい DB には書かない」規則で拒否される。リリースノートに書く。
- 保持期間：閉じてから 30 日を過ぎたセッションの `ai_session_output` を消す（`ai_sessions`、`ai_session_lines`、`ai_session_events` は残す）。既存の保持処理と同じ周期で、新しい関数 `pruneAISessionOutput` を掃除役から呼ぶ。
- セッションの行を `DeleteCommandRequest` で消すのは、セッションが閉じてからに限る（結線：`if a.aiLineOpen(id) { return 実行中扱いで拒否 }`）。

---

## 8. GUI

### 8.1 サイドバー（sidebar.js の `renderMCPSection` に1行）

```
── エージェント（MCP）────────
MCP ● 待受中 127.0.0.1:8787
アクティブなエージェント 2（直近5分）
AI セッション 2（承認待ち 1） →        ← 0 件で機能 OFF なら出さない
承認待ち 3 →
```

- EN：`AI sessions 2 (1 awaiting approval) →`。クリックすると `data-action="side-aisessions"` で「AI セッション」画面へ移る。
- 件数は `sessions:changed` の payload に足す `aiSessions: {open, pendingApproval}` から取る（`sessionsPayload` に1フィールド足す結線）。

### 8.2 画面「AI セッション」（`frontend/src/aisessions.js`）

```
┌ AI セッション ─────────────────────────────────────────── [設定] ┐
│ ● AIS-7Q… web-01  claude-code  exec  /var/www  12分  行 8  承認待ち 1  [閲覧] [強制終了] │
│ ● AIS-K2… db-stg  codex        shell ⚠汚染    3分  行 2               [閲覧] [強制終了] │
│ ○ AIS-AB… web-01  claude-code  終了（アイドル）  14:02                 [閲覧]            │
├──────────────────────────────────────────────────────────────────────────────┤
│ 閲覧中: AIS-7Q… — 読み取り専用                                                        │
│ ┌ xterm（disableStdin） ───────────────────────────────────────────────────────────┐ │
│ │ $ systemctl status nginx                                                     │ │
│ │ ● nginx.service - …                                                          │ │
│ └──────────────────────────────────────────────────────────────────────────────┘ │
│ 送信した行: #1 cd /var/www ✓0 · #2 ls ✓0 · … #8 nginx -t ⏳承認待ち → 承認画面へ          │
└──────────────────────────────────────────────────────────────────────────────┘
```

- 一覧：状態の記号・文字・アイコン（`bi-robot`）を併記し、色だけに頼らない（§6.1 の規則）。閉じたセッションは墓標が残っている間だけ表示する。
- 閲覧：`mountAISessionViewer` が `#terminal-dock` とは別のコンテナに xterm を1つ作り、`ReadAISessionRaw(id, 0)` でリングの内容を埋めてから、`aisession:data` を追う。画面を離れたら dispose する（人のタブとは違い、保持しなくてよい。戻ったら読み直す）。リングの先頭より前は「以前の出力は破棄されました（n バイト）」と表示する。
- 強制終了：確認ダイアログ「このセッションを終了します。承認待ちの行は取り下げられ、実行中の行は中止されます」→ `CloseAISession(id)`（reason `human_closed`）。
- 設定：§5 の表の項目。shell を許す接続先は、接続先の一覧から選ぶ。`Enabled` を ON にするときは、「エージェント専用トークンが必要」「High は常に承認」の説明を出す。
- 承認画面（approvals.js）：選んだ要求に `sessionId` があれば、`GetAISessionLineContext` の結果を枠で出し、コマンドの textarea を readonly にする（approvals.js への結線は数行）。

### 8.3 main.js への結線（最小限）

- `shell.js` の `PAGES` に `'aisessions'` を、`NAV_ICONS` に `bi-robot` を足す。
- `renderPage()` に `else if (state.active === 'aisessions') renderAISessionsPage(root, ctx)` を1行足す。クリックの委譲に `handleAISessionsClick` を1行足す。
- `EventsOn('aisession:data', onAISessionData)`、`EventsOn('aisessions:changed', onAISessionsChanged)` を1行ずつ。
- 画面を離れる処理に `disposeAISessionViewer()` を1行。`pageIsBusy()` に `aiSessionsBusy()` を足す（設定の入力中は再描画しない）。
- I18N：`aisessions.js` が `AI_SESSIONS_I18N = {ja, en}` を export し、`i18n.js` でマージする（1行）。

### 8.4 文言（主なもの）

| キー | JA | EN |
|---|---|---|
| nav.aisessions | AI セッション | AI sessions |
| side.aiSessions | AI セッション {n}（承認待ち {p}） | AI sessions {n} ({p} awaiting approval) |
| ais.state.idle / busy / pending_approval / closed | 待機中 / 実行中 / 承認待ち / 終了 | Idle / Running / Awaiting approval / Closed |
| ais.close.* | エージェントが終了 / 人が強制終了 / アイドルで終了 / 最大時間で終了 / 切断で終了 / 接続先の削除で終了 / 実行先の変更で終了 / トークン失効で終了 / 機能 OFF で終了 / リモートで終了 / 接続断 / アプリ終了 / 再起動で回収 / 開始に失敗 | Closed by agent / Force-closed / Idle timeout / Max lifetime / Disconnected / Connection deleted / Target changed / Token revoked / Feature disabled / Remote exit / Connection lost / App shutdown / Recovered after restart / Failed to open |
| ais.readonly | 読み取り専用 | Read-only |
| ais.forceClose | 強制終了 | Force close |
| ais.confirmClose | このセッションを終了します。承認待ちの行は取り下げられ、実行中の行は中止されます。 | End this session? Pending lines are withdrawn and the running line is stopped. |
| ais.tainted | シェルの状態（alias / 関数 / trap など）が変更されています。以後の行はすべて承認が必要です | Shell state (aliases, functions, traps…) was changed. Every further line needs approval. |
| approval.aiLine | AI セッション内の行 | Line in an AI session |
| approval.aiContext | 実行場所 {cwd} · 直前の行 | Runs in {cwd} · Previous lines |
| ais.redacted | 秘密らしい文字列を {n} 件伏せました | {n} secret-like strings redacted |
| ais.missed | 以前の出力は破棄されました（{n} バイト） | Earlier output was discarded ({n} bytes) |
| ais.disabled | AI セッションは無効です。設定で有効にできます | AI sessions are disabled. Enable them in settings. |

---

## 9. 担当分けとテスト計画

### 9.1 担当（新しいファイル）

| 担当 | ファイル | 公開する関数・型（パッケージ内） |
|---|---|---|
| A. ライフサイクル | `aisession.go` | `type aiSessionState`、`type aiSession`、`type AISessionInfo`、`(a *App) openAISession(ctx, caller mcpCaller, args openSessionArgs) (*aiSession, error)`、`closeAISession(id, reason, by string) bool`、`closeAllAISessions(reason string, grace time.Duration)`、`closeAISessionsForConnection(connID, reason string) int`、`lookupAISession(id, owner string) (*aiSession, error)`、`runAISessionSweeper(ctx)`、`aiSessionCounts() aiSessionCounts`、`recoverAISessionsOnStartup()`、バインディング `ListAISessions() []AISessionInfo`、`GetAISession(id) AISessionDetail`、`CloseAISession(id) ActionResult`、`CloseAllAISessions() ActionResult`、`GetAISessionSettings() AISessionSettings`、`SaveAISessionSettings(AISessionSettings) ActionResult` |
| B. 転送路 | `aisession_transport.go` | `type aiTransport interface { StartLine(ctx, line lineScript, out io.Writer) (lineHandle, error); Interrupt() error; Close() error }`、`type lineHandle interface { Wait() (exitCode int, err error) }`、`newExecTransport(client *ssh.Client, shell string) (aiTransport, string /*cwd*/, error)`、`newShellTransport(ctx, client, shell string, nonce []byte) (aiTransport, error)`、`buildExecScript(cwd string, env envOverlay, line string, nonce string, cdProbe bool) []byte`、`parseMarkers(...)`、`writeControl` |
| C. 行の検査・分類・承認 | `aisession_line.go`、`aisession_risk.go` | `validateSessionLine(line string) (rejectReason string)`、`classifySessionLine(line string, st sessionRiskState) sessionLineClass`、`sessionNeedsApproval(s *aiSession, c sessionLineClass, settings AISessionSettings) bool`、`(a *App) sendSessionLine(ctx, caller, args sendLineArgs) (any, error)`、`(a *App) approveAISessionLine(id, command string, grantBypass bool, link aiLineLink) ActionResult`、`(a *App) aiLineLink(requestID string) (aiLineLink, bool)`、`(a *App) aiLineOpen(requestID string) bool`、`(s *aiSession) startLine(req CommandRequest) error`、`(a *App) interruptAISession(caller, id) (any, error)`、`GetAISessionLineContext(requestID) AISessionLineContext` |
| D. 出力 | `aisession_output.go`、`aisession_redact.go` | `type aiOutput`、`newAIOutput(ringBytes int, onText func(chunk textChunk), emitRaw func([]byte, uint64)) *aiOutput`、`(o *aiOutput) Write(p []byte) (int, error)`、`ReadText(since uint64, max int) textRead`、`ReadRaw(since uint64, max int) rawRead`、`WaitText(ctx, since uint64) bool`、`MarkLine(seq int) uint64`、`type terminalStripper`、`(s *terminalStripper) Feed(p []byte) []byte`、`redactSecrets(line string) (string, []redaction)`、`type secretLineBuffer`（PEM の保留） |
| E. 永続化 | `storage_aisession.go` | `migrateAISessionTables(tx *sql.Tx) error`、`(s *Store) insertAISession(row)`、`closeAISessionRow(id, reason, by, at, counters)`、`insertAISessionLine(row)`、`finishAISessionLine(requestID, cursorEnd)`、`insertAISessionEvent(ev)`、`appendAISessionOutput(chunks []outputChunk)`、`loadAISessionLinks() (map[string]aiLineLink, error)`、`openAISessionRows() ([]aiSessionRow, error)`、`loadAISessionSettings() / saveAISessionSettings(...)`、`pruneAISessionOutput(before time.Time) (int64, error)` |
| F. MCP | `mcp_aisession.go` | `aiSessionTools() []map[string]any`、`(a *App) callAISessionTool(ctx, caller, params toolCallParams) (any, *rpcError, bool /*handled*/)`、引数の型 `openSessionArgs`、`sendLineArgs`、`readOutputArgs`、`sessionIDArgs`、`listSessionsArgs`、`aiSessionError(code, message string, extra map[string]any) any` |
| G. frontend | `frontend/src/aisessions.js`、`frontend/src/aisessions.test.js` | `AI_SESSIONS_I18N`、`renderAISessionsPage(root, ctx)`、`handleAISessionsClick(event, ctx) bool`、`mountAISessionViewer(container, sessionId, deps)`、`disposeAISessionViewer()`、`onAISessionData(payload)`、`onAISessionsChanged(payload)`、`summarizeAISessions(list) {open, pendingApproval}`、`aiSessionStateLabelKey(code)`、`aiSessionCloseReasonKey(code)`、`renderAILineContext(ctx, t)`（承認画面用）、`aiSessionsBusy()` |
| H. 結線（最後に1人で行う） | 既存ファイル（§9.2） | — |

- A・B・C・D・E・F・G は、互いの関数名と型（上表）だけを約束にして並行に進める。B は C・D に依存しない（`io.Writer` に書くだけ）。C は A の `aiSession` のフィールドに依存するので、A が最初の日に型だけを入れた `aisession_types.go` を作る。
- テスト用の fake：`aisession_fake_test.go`（B の `aiTransport` の fake。受け取ったバイト列を記録し、終了コードと出力を台本で返す）と、`sshtest_aisession_test.go`（sshtest サーバーに、`/bin/sh -s` を真似る小さな解釈器と PS1 マーカーを返す PTY シェルを足す。本物の /bin/sh には頼らない。Windows の CI でも動く）。

### 9.2 既存ファイルへの結線（H。行数の目安）

| ファイル | 変更 |
|---|---|
| app.go | App に `aiSessionState` を埋め込む（1）。CommandRequest に `SessionID` / `SessionSeq`（2）。NewApp で `recoverAISessionsOnStartup` と掃除役を起こす（3）。shutdown に `closeAllAISessions(app_shutdown, 2s)`（1）。DisconnectConnection と DeleteConnection で `closeAISessionsForConnection`（各 2）。`sessionsPayload` に `AISessions`（2）。DeleteCommandRequest で `aiLineOpen` を確かめる（3） |
| approval.go | `approve()` の先頭で `aiLineLink` に分岐する（3） |
| mcp_server.go | `callMCPTool` の default の前で `callAISessionTool` に回す（4）。`mcpTools()` に `aiSessionTools()...` を連結（1）。`requestView` に `session_id` / `session_seq`（3）。initialize の instructions に1文 |
| storage.go | `schemaVersion = 5`（1）。`migrate()` で `migrateAISessionTables(tx)` を呼ぶ（3） |
| events.go | `aiSessionDataEvent = "aisession:data"`、`aiSessionsChangedEvent = "aisessions:changed"`（2） |
| terminal.go | 変更しない |
| frontend/src/shell.js | `PAGES`、`NAV_ICONS`、`pageTitle`（3） |
| frontend/src/sidebar.js | `renderMCPSection` に1行と、クリックの扱い（6） |
| frontend/src/main.js | §8.3（約 8） |
| frontend/src/approvals.js | `sessionId` があれば文脈を出し、textarea を readonly にする（約 10） |
| frontend/src/i18n.js | `AI_SESSIONS_I18N` のマージ（2） |
| `wails generate module` | バインディングの再生成 |

### 9.3 テスト計画

**Go（`go test -race -count=1`、並行系は `-count=50`）**

- `aisession_risk_test.go`（表駆動）：`validateSessionLine` で、§3.1 の各 reject_reason に当たる例と、通る例（日本語の引数、`\t`）。`classifySessionLine` で、`cd /var/www` → Low/cd、`cd /tmp && ls` → risk.go のまま・cwd_persisted=false、`export LANG=C` → Low、`export FOO=1` → Medium・unsafe、`export PATH=/tmp` / `LD_PRELOAD=x ls` → High、`alias ll=ls` / `trap x INT` / `set -o vi` / `nohup x` / `tmux` → High、unsafe な上乗せの下での `ls` → Medium、`unset FOO` で下限が戻ること。risk.go の既存テストがすべて変わらず通ること（下げていないことの確認）。
- `aisession_approval_test.go`：
  - 承認待ちの行が transport に届かない
  - Approve で1回だけ届く
  - Approve と Reject、Approve と close、Approve と期限切れを並行させても、届くのは高々1回
  - `session_busy`（承認待ちの間に2行目を送る）
  - High は承認省略があってもキューに入る
  - BypassMaxRisk=Low のとき、Medium がキューに入る
  - tainted の後は Low もキューに入る
  - RequireApprovalSudo
  - 共有トークンでは `agent_token_required`
  - 修正したコマンドでの承認は `edit_not_allowed`
  - fp が変わったら `target_changed` で閉じる
  - client_request_id の再送で1回だけ実行され、違う行なら `idempotency_conflict`
- `aisession_isolation_test.go`：`TERM-` の ID → `session_not_found`、`AIS-` の ID → SendTerminalInput で `no_session`、他のエージェント → `not_owner`、list_sessions に人のタブも他人のセッションも出ない。
- `aisession_lifecycle_test.go`（clock を差し替える）：アイドルで閉じる、承認待ちの間はアイドルで閉じない、最大寿命では閉じる、1エージェントと全体の上限、トークンを失効すると閉じる、Enabled=false で閉じる、DisconnectConnection と DeleteConnection で閉じて件数が返る、`KillAll` で `conn_lost`、shutdown で実行中の行が `interrupted` になり `closed_at` が書かれてから store が閉じる、起動時に開いたままの行が `app_restart` で閉じ、承認待ちの行が `withdrawn` になる。ロックの規約は `-race` と、ロックを持ったまま emit しないことの検査（fake emit の中で `aiMu.TryLock` が成功すること）で確かめる。
- `aisession_output_test.go`：カーソルが単調に増える、max_bytes が文字の境目で切れる、リングが溢れたときの `missed_bytes`、`invalid_cursor`、long-poll が出力・状態変化・close・タイムアウトのそれぞれで戻る、エスケープシーケンスがチャンクの境目をまたぐ場合（1 バイトずつ流す fuzz も足す）、`\r` の上書き、巨大な OSC の打ち切り。
- `aisession_redact_test.go`：各種の秘密を検知する、同じ値なら同じ印になる、PEM が複数行でも伏せられ、保留中は出ない、DB 本体と `-wal` に生の値が残らない（storage_test.go の平文検査と同じ方法）。
- `aisession_transport_test.go`（拡張した sshtest を使う）：exec モードで `cd` が次の行に持ち越される、`export` も持ち越される、alias が持ち越されない、stdin が EOF になる、cwd が消えたら 201 と分かるメッセージ。shell モードで PS1 マーカーで終了と終了コードが取れる、PS2 で `incomplete_line` になり Ctrl-C が送られる、前面のプログラムがあるときは `busy`、interrupt で戻る。マーカーが AI 向けの出力にも監査にも出ないこと。
- `mcp_aisession_test.go`：tools/list の schema（`additionalProperties:false`、outputSchema）、各エラーコードの isError と structuredContent、`get_request_status` に session_id が載る、一連の流れ（open → send(cd) → send(pwd) → read → close）が受け入れ条件どおりに動く。
- `storage_aisession_test.go`：v4 から v5 への移行（退避が作られる、既存の行が変わらない、2回開いても同じ）、v6 の DB は拒否する、`pruneAISessionOutput`。

**frontend（vitest）**

- `aisessions.test.js`：`summarizeAISessions` と状態・終了理由のラベル（表駆動、JA/EN の両方でキーの漏れが無いこと）、一覧の描画のスナップショット（JA と EN）、閲覧用の xterm が `disableStdin:true` で作られ、onData を登録しないこと、強制終了の確認ダイアログでキャンセルしたら CloseAISession を呼ばないこと、`aisession:data` が閲覧中のセッションの分だけ書かれること、`missed` の表示、`renderAILineContext` が文脈を出し、エスケープ処理されていること（行に `<script>` を含めても描画されない）。
- `sidebar.test.js` に追記：AI セッションの行が件数どおりに出る、0 件で機能が OFF なら出ない。`approvals.test.js` に追記：`sessionId` 付きの要求では textarea が readonly になり、文脈の枠が出る。

**手動の確認**

- 実際の Linux（bash、dash、zsh、fish のログインシェル）と macOS に対して、exec モードで cd と export が持ち越されること。fish でも stdin 方式で動くこと。
- shell モードで、`vi` を開いた後に interrupt と close_session で抜けられること。
- Web 管理画面（フェーズ6）から一覧・閲覧・強制終了ができること（マージ後）。

---

## 10. ユーザー判断が要る点（推奨案つき）

1. **shell モードを v1 に入れるか**：(a) exec モードだけで出し、shell は次の段にする／(b) 接続先ごとの許可制で一緒に出す（本書の設計）。推奨は (b)。ただし、実装の量を抑えたいなら (a) にしても REQ-016 の受け入れ条件は満たせる。
2. **承認省略の上限の既定値**：推奨は Low（本書）。Medium にすると execute_command と同じ扱いになるが、状態が積み重なる分だけ危ない。
3. **秘密を AI からも伏せるか**：推奨は伏せる（本書）。伏せると、AI が設定ファイルの値を確かめたい場面では、人がターミナルで見ることになる。

---

## 11. 実装メモ（設計との対応）

- ファイル名は設計の表と一部違う：`ai_session.go`（A ライフサイクル・D 出力・B の shell モード）、`ai_session_exec.go`（B の exec モード、`buildExecScript`）、`ai_session_approval.go`（C）、`ai_session_redact.go`（D の `redactSecrets` と PEM の保留）、`ai_session_store.go`（E）、`mcp_ai_session.go`（F）、`ai_session_app.go`（App 側：`mcpAISessionBackend` の実装、Store への結線、起動時の回収、監査の書き出し）。
- 行の経路は1つだけ：`aiLineGate.startLine` → `aiSessionRunner.RunLine` → `aiTransport.StartLine`。`aiSession` はライフサイクル・transport・出力だけを持ち、cwd・環境変数の上乗せ・汚染・処理中の行はゲートが持つ（§3.5 の不変条件）。
- `aiSessionState` の App への埋め込みはまだで、`aiMgr()` / `aiLines()` が `*App` をキーにした sync.Map から引く（本体の app.go の変更と衝突しないため）。shutdown の最後に `forgetAISessions()` で外す。
- 行ごとの記録（command_requests の stdout / stderr）は、exec モードでは stdout と stderr を別々に伏せて持つ。shell モードは PTY が1本なので、行の範囲のテキストを stdout に入れる。
- exec モードの `cd -` は、行をまたいで OLDPWD を持たないので失敗する（既知の制限）。shell モードの cwd はゲートウェイから見えない（承認画面には開いた時点の値が出る）。
- §10 の判断は推奨案のまま実装した（shell モードは接続先ごとの許可制で同梱、承認省略の上限は Low、秘密は AI からも伏せる）。
