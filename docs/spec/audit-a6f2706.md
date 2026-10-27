# 監査報告書: a6f2706（v1 フェーズ0/1）

## 1. 概要

| 項目 | 内容 |
|---|---|
| 対象コミット | `a6f2706`（ブランチ `fix/v1-safety-multisession`、「v1 フェーズ0/1: 安全性・永続化・MCP状態取得の土台」） |
| 観点 | 12（http-guard / approval-cas / bypass-risk / ssh-exec / storage / secrets-leak / mcp-protocol / concurrency / frontend-xss / windows-unix / ci-release / tests-gaps） |
| ラウンド | 3 |
| 指摘総数 | 60 |
| 確定 | 58（検証者3人のうち2人以上が反証できなかったもの） |
| 重要度別 | critical 2 / major 29 / minor 27 |

再現はすべて作業ツリーとは別のスクラッチコピーで行っています。監査対象のツリーは変更していません。

### 1.1 印の意味

- **【P2】**: フェーズ2（`docs/spec/phase2-contract.md`、マルチタブ・サイドバー・編集画面）で触るファイルや機能にかかる指摘。フェーズ2の実装と同時に直す。
- **【P3】**: フェーズ3（MCP の充実と認証の強化、TOFU、client_request_id、承認画面）で触る範囲にかかる指摘。フェーズ3の実装と同時に直す。
- 印なし: フェーズ1の取りこぼし、またはどのフェーズにも入っていないもの。フェーズを待たずに単独で直す。

フェーズ2の契約では `mcp_server.go`・`storage.go`・`ssh_exec.go`・`httpguard.go` は「触らないファイル」です。そのため、これらのファイルの指摘には【P2】を付けていません。

### 1.2 重複グループ

同じ欠陥を別の観点の検証者が独立に報告したものです。番号は残してありますが、修正は1件として扱えます。

| グループ | 番号 | 内容 |
|---|---|---|
| A | #22, #40, #53 | 承認画面で、表示中の要求と操作対象の要求がずれる |
| B | #2, #11, #31 | 接続 ID がないときの名前フォールバックで、別の接続先で実行される |
| C | #4, #13, #18 | DeleteCommandRequest が CAS になっていない |
| D | #15, #17, #30 | client_request_id の重複排除が check-then-act の競合になる |
| E | #5, #28, #48 | classifyRisk の部分文字列一致がすり抜けられる |
| F | #6, #29, #46 | sudo の判定がすり抜けられ、テストも効いていない |
| G | #32, #52 | Bearer 判定と承認省略判定が別の設定を読んでいる |
| H | #20, #39 | ターミナル開始中の削除・切断が効かない |
| I | #24, #44 | release.yml の build ジョブに書き込み権限付きのトークンが残る |
| J | #25, #43 | Wails CLI の `@latest` と、action のタグ参照 |
| K | #26, #45 | govulncheck が linux だけで走る |

## 2. 重要度順の一覧

| # | 重要度 | 観点 | file:line | 要約 | 印 |
|---|---|---|---|---|---|
| 22 | critical | frontend-xss | frontend/src/main.js:2001 | 承認・拒否が、画面に出ていない最新の pending 要求に対して実行される | 【P2】【P3】 |
| 40 | critical | frontend-xss | frontend/src/main.js:2004 | 要求 A の編集済みコマンドが要求 B の接続先で承認・実行される | 【P2】【P3】 |
| 1 | major | http-guard | httpguard.go:232 | 不正な MCP パスを保存すると HandleFunc が panic し、以後毎回起動時に落ちる | 【P3】 |
| 2 | major | approval-cas | approval.go:139 | 削除済み接続の pending 要求が、同名の新しい接続先で実行される | 【P3】 |
| 3 | major | approval-cas | storage.go:319 | 2つ目のインスタンスが、実行中の要求を interrupted に書き換える | 【P2】 |
| 5 | major | bypass-risk | mcp_server.go:601 | 区切り文字・クォート・展開で High コマンドが Low に分類される | 【P3】 |
| 7 | major | ssh-exec | ssh_exec.go:85 | NewSession にタイムアウトもキャンセルも効かない | 【P3】 |
| 10 | major | storage | app.go:880 | 旧 JSON の取り込みが失敗すると、二度と再試行されない | |
| 11 | major | storage | approval.go:139 | 不変 ID が名前フォールバックで無効化される（B と同じ） | 【P3】 |
| 15 | major | mcp-protocol | mcp_server.go:305 | client_request_id の重複排除が競合し、複数回実行される | 【P3】 |
| 17 | major | concurrency | mcp_server.go:305 | 同じキーの並行送信がすべて実行される（D と同じ） | 【P3】 |
| 18 | major | concurrency | approval.go:241 | 実行中の要求が監査履歴から削除される | 【P2】 |
| 19 | major | concurrency | app.go:306 | 終了時に実行中コマンドを待たずに store.Close する | 【P2】 |
| 24 | major | ci-release | .github/workflows/release.yml:13 | build ジョブにも contents:write が付き、トークンが .git/config に残る | |
| 25 | major | ci-release | .github/workflows/release.yml:44 | Wails CLI が `@latest`、action がタグ参照 | |
| 28 | major | tests-gaps | mcp_server.go:601 | クォートや連続空白の High コマンドが Low になり、テストもない | 【P3】 |
| 29 | major | tests-gaps | approval.go:280 | `"sudo"` が RequireApprovalSudo をすり抜け、テストは sudo 規則を検証していない | 【P3】 |
| 31 | major | approval-cas | approval.go:139 | 名前フォールバックで実行先が変わり、監査記録の ConnectionID も上書きされる | 【P3】 |
| 32 | major | bypass-risk | approval.go:270 | 旧リスナーで認証なしで通った要求に、新設定の承認省略が適用される | 【P3】 |
| 33 | major | ssh-exec | ssh_exec.go:144 | ssh-agent が応答しないと openSSHClient が永久に止まる | 【P3】 |
| 34 | major | storage | storage.go:50 | 空の DB ファイルが残ると、旧 JSON が取り込まれずに *.migrated にされる | |
| 38 | major | concurrency | terminal.go:232 | ターミナル入力の順序が入れ替わり、途中までのコマンドが実行される | 【P2】 |
| 43 | major | ci-release | .github/workflows/release.yml:79 | softprops/action-gh-release が可変タグ @v2 で、書き込み権限付きで動く | |
| 44 | major | ci-release | .github/workflows/release.yml:31 | npm のライフサイクルスクリプトが書き込みトークンを読める | |
| 46 | major | tests-gaps | mcp_server_test.go:458 | 「sudo はキューに入る」のテストが sudo 規則まで届いていない | 【P3】 |
| 48 | major | bypass-risk | mcp_server.go:604 | High の一覧が固定の綴りだけで、ふつうの破壊的コマンドが Low になる | 【P3】 |
| 49 | major | bypass-risk | approval.go:277 | RequireApprovalWriteOp がリダイレクトや cp によるファイル書き込みを捕まえない | 【P3】 |
| 50 | major | storage | storage.go:150 | 途中で切れたバックアップを、正常なバックアップとして扱う | |
| 52 | major | concurrency | mcp_server.go:89 | 認証と承認省略判定が別のスナップショットを読む（G と同じ） | 【P3】 |
| 53 | major | frontend-xss | frontend/src/main.js:1063 | 選択が保存されず、ポーリング後に別の要求を承認・拒否する | 【P2】【P3】 |
| 55 | major | tests-gaps | mcp_server_test.go:456 | 「High は常にキュー」の不変条件にテストが効いていない | 【P3】 |
| 4 | minor | approval-cas | approval.go:237 | DeleteCommandRequest が CAS でなく、実行済みの記録も物理削除する | 【P2】 |
| 6 | minor | bypass-risk | approval.go:294 | パス付きやクォート付きの sudo で RequireApprovalSudo をすり抜ける | 【P3】 |
| 8 | minor | ssh-exec | ssh_exec.go:151 | strict モードで HostKeyAlgorithms を known_hosts に合わせていない | 【P3】 |
| 9 | minor | ssh-exec | ssh_exec.go:294 | 鍵パスの `$` や `%` が勝手に展開される | 【P3】 |
| 12 | minor | storage | storage.go:151 | VACUUM INTO のバックアップが chmod までの間 0644 になる | |
| 13 | minor | storage | approval.go:235 | 削除の SQL が無条件 DELETE（C と同じ） | 【P2】 |
| 14 | minor | secrets-leak | app.go:1087 | maskToken が短いトークンのほぼ全体を表示する | 【P3】 |
| 16 | minor | mcp-protocol | approval.go:396 | 再起動後は、500件より古い要求に重複排除が効かない | 【P3】 |
| 20 | minor | concurrency | terminal.go:146 | ダイヤル中に接続を削除しても、ターミナルが残る | 【P2】 |
| 21 | minor | concurrency | app.go:647 | DeleteMCPSettings と Save/Generate の競合で、リスナーが残る | 【P3】 |
| 23 | minor | windows-unix | app.go:845 | 設定ディレクトリが共有の /tmp にフォールバックし、他人の DB を開く | |
| 26 | minor | ci-release | .github/workflows/test.yml:42 | govulncheck が linux だけで、`@latest` | 【P2】 |
| 27 | minor | ci-release | .github/workflows/release.yml:3 | テストを通らなくてもリリースされる | |
| 30 | minor | tests-gaps | mcp_server.go:305 | client_request_id の並行テストがない（D と同じ） | 【P3】 |
| 35 | minor | secrets-leak | frontend/src/main.js:2177 | 認証方式を切り替えると、パスワードが鍵パス欄に平文で移る | 【P2】 |
| 36 | minor | secrets-leak | app.go:367 | 保存済みパスワードが、変更後の別ホストへ送られる | 【P2】 |
| 37 | minor | mcp-protocol | approval.go:136 | 最終状態の保存に失敗すると、結果が失われ running のまま残る | 【P3】 |
| 39 | minor | concurrency | terminal.go:146 | ダイヤル中の切断・削除が効かない（H と同じ） | 【P2】 |
| 41 | minor | windows-unix | storage.go:53 | Windows では 0600/0700 が ACL として効かない | |
| 42 | minor | windows-unix | ssh_agent_windows.go:24 | 別の綴りのパイプ名を無視して、既定の agent を使う | |
| 45 | minor | ci-release | .github/workflows/test.yml:41 | govulncheck が Windows/macOS 専用のコードを見ない（K と同じ） | 【P2】 |
| 47 | minor | approval-cas | storage.go:212 | 旧版の「承認待ち」がすべて pending になり、二重実行されうる | |
| 51 | minor | mcp-protocol | mcp_server.go:116 | クライアントからの JSON-RPC レスポンスに 202 ではなく 400 を返す | 【P3】 |
| 54 | minor | ci-release | frontend/package-lock.json:17 | npm 依存の脆弱性を CI が検査しない（vite に high 4件） | 【P2】 |
| 56 | minor | tests-gaps | approval.go:123 | risk_increased のガードにテストがない | 【P3】 |
| 57 | minor | tests-gaps | approval.go:267 | force_queue にテストがない | 【P3】 |
| 58 | minor | tests-gaps | approval.go:206 | タイムアウト時に stderr_truncated が false になる | 【P3】 |

## 3. 各指摘の詳細

### 3.1 critical

#### #22 [critical/frontend-xss] frontend/src/main.js:2001 【P2】【P3】（グループ A）

**内容**: 承認・承認省略つき承認・拒否のハンドラは、クリックした時点で `selectedCommandRequest()` を使って対象を引き直します。そのため、画面に出ている要求とは別の要求が対象になることがあります。`state.selectedRequestId` が空（キューの行を一度もクリックしていない）か、もう pending でない要求を指していると、先頭の pending 要求にフォールバックします。approval.go:44 は新しい要求をリストの先頭に入れるので、先頭は最新の要求です。textarea を編集中（`dirty.approval=true`）だと、refreshData（712行）は `state.data.requests` を新しい内容に差し替えますが、render() は呼びません。その結果、DOM には要求 A が表示されたまま、state はすでに要求 B を指しています。ハンドラは DOM の textarea の文字列（A を編集したコマンド）を B の id と組み合わせて送ります。

**再現シナリオ**:
1. 要求 A（ホスト dev-1、`rm -rf /tmp/build/*`、risk high）が pending。ユーザーがコマンド画面を開くと、行をクリックしなくてもフォールバックで A が表示される。
2. ユーザーが textarea を `rm -rf /tmp/build/cache` に編集する（dirty.approval=true）。
3. エージェントが要求 B（ホスト prod-db、`sudo systemctl restart postgres`、risk high）を送る。B は a.requests の先頭に入り、次の5秒ポーリングで state.data.requests は更新されるが、再描画はされない。
4. ユーザーが A を表示したままの画面で「承認」を押す。`ApproveCommand(B.id, 'rm -rf /tmp/build/cache')` が実行される。B の risk を超えていないので approve() の risk_increased チェックでは止まらない。編集したコマンドが prod-db で実行され、B が承認・実行済みとして記録される。A は pending のまま残る。
- 「承認して省略」なら、B のエージェントに、画面に一度も出ていない承認省略権限が付与される。「拒否」なら A ではなく B が拒否される。
- 選択していた A が編集中に別の理由で解決した場合（削除、実行中の interrupted など）も、フォールバックで最新の pending 要求が選ばれる。

**修正案**: commandDetail を描画するとき、表示した要求の id を DOM に持たせる（承認・拒否ボタンや textarea に `data-request-id`）。ハンドラはその id をそのまま使い、`selectedCommandRequest()` を呼ばない。選択がないときに「先頭の pending」へフォールバックするのをやめる。描画時に selectedRequestId を表示中の id に設定するか、要求を選ぶまで承認を無効にする。

#### #40 [critical/frontend-xss] frontend/src/main.js:2004 【P2】【P3】（グループ A）

**内容**: 承認と承認省略つき承認は、コマンド文字列を画面上の `#approval-command` textarea から取り、要求 ID はクリック時に state.data から計算し直した `selectedCommandRequest()` から取ります。selectedRequestId が `''`（pending 要求がフォールバックで表示されているだけ）のとき、この2つが別の要求を指すことがあります。その場合、要求 A の編集済みコマンドが、より新しい要求 B の接続先で承認・実行されます。

**再現シナリオ**:
1. 要求 A が pending。行をクリックせずにコマンド画面を開くと、selectedRequestId は `''` のままで、commandDetail は `find(status==='pending')` のフォールバックで A を表示する。
2. textarea を編集する（dirty.approval=true）。
3. エージェントが別ホスト（例: 本番）向けの要求 B を出す。approval.go:44 が B を先頭に追加する（`a.requests = append([]CommandRequest{req}, a.requests...)`）。
4. 5秒ごとの refreshData が state.data.requests を差し替えるが、protectApproval のため render() は呼ばれず、画面は A のまま。
5. 「承認」を押すと selectedCommandRequest() は最新の pending である B を返し、`ApproveCommand(B.id, <A の編集済みコマンド>)` が実行される。ユーザーが B を一度も見ないまま、A のコマンドが B の接続先で実行される。「approve-bypass-agent」なら B のエージェントに恒久的な承認省略も付く。「reject-command」（2056行）と「copy-command」も同じように B に対して動く。

**修正案**: 画面に出ている要求 ID を DOM（承認・拒否・承認省略ボタンや textarea の `data-request-id`）に入れ、ハンドラでは selectedCommandRequest() を計算し直さずにその ID を使う。または、commandDetail がフォールバックで選んだときにも state.selectedRequestId をその ID に設定する。その ID の要求がもう pending でなければ操作を拒否する。

### 3.2 major

#### #1 [major/http-guard] httpguard.go:232 【P3】

**内容**: MCP・Health・Audit のパスは、ユーザー設定から何の検査もなしに `http.ServeMux.HandleFunc` に渡されます。sanitizeMCPSettings（app.go:1006-1014）は空の値を埋めるだけです。そのため、先頭に `/` がないパス、重複したパス、パースできないパターンを渡すと HandleFunc が panic します。不正な設定はすでに SQLite に保存されているので、以後は起動するたびにアプリが落ちます。

**再現シナリオ**: MCP 設定画面で、MCPPath に `mcp`（先頭スラッシュなし）を入れる、MCPPath と HealthPath を同じ `/mcp` にする、または `/a/{x` や `/mcp `（末尾に空白）を入れて保存する。SaveMCPSettings は先に store.saveMCPSettings を呼び（app.go:594）、次に restartMCPServer → startMCPServerLocked → newMCPHandler → mux.HandleFunc が panic する。Go 1.26 のスクラッチプログラムで再現した（`host/path missing /`、`conflicts with pattern`、`invalid method`、`bad wildcard segment`）。最初の呼び出しでは Wails ディスパッチャの recover() が panic を捕まえるが、古いリスナーはもう止まっているので MCP は止まったままになり、呼び出しは結果を返さない。次回の起動では、app.startup が recover() のない素の goroutine の中で動く（wails v2.12.0 darwin/frontend.go:248-252）。そこから startMCPServer が呼ばれ、同じ panic でプロセスが終了する。以後は起動のたびに落ち、UI からは直せない。抜け出すには SQLite DB を手で編集するしかない。

**修正案**: sanitizeMCPSettings の中か、SaveMCPSettings が保存する前にパスを検証する。先頭の `/` を必須にし、空白と `{` `}` を禁止し、同じパスが2つあれば拒否する。あわせて newMCPHandler/startMCPServerLocked で recover() し、panic をエラーとして返す。そうすれば、不正な値がすでに保存されていてもアプリは動き続ける。

#### #2 [major/approval-cas] approval.go:139 【P3】（グループ B）

**内容**: pending 要求の ConnectionID がもう存在しないとき、approve() は `findConnection(req.Host)` にフォールバックします。これは名前か host/IP で一致を探します。そのため古い pending 要求が、いまその名前を持っている接続で実行されます。さらに状態遷移の mutate 処理が ConnectionID/Host を新しい接続先に書き換えるので、監査記録からは接続先が変わったことが分かりません。

**再現シナリオ**: エージェントが接続 "web"（ID A、サーバー A）に `echo destroy` を積む。ユーザーが A を削除する。DeleteConnection は pending 要求を pending のまま残す。続いて、別のサーバー B（例: 本番）を指す同名の "web" を新しく追加する。キューから古い要求を承認すると、コマンドは B で実行される。スクラッチコピーで再現した（approve は成功、execsA=[]、execsB=[echo destroy]）。関連するケースも再現した。要求が pending の間に接続 A の host や port を編集した場合（sameTarget=false）も、要求が接続先の指紋を持っていないので、承認すると新しい接続先で実行される。

**修正案**: 名前・host によるフォールバックをやめる。connectionByID が失敗したら、承認を拒否するか、要求を failed/expired にする。さらに、DeleteConnection のときにその接続の pending 要求を CAS で終端状態に移す（または host/port/user の指紋と結び付ける）。接続先が変わっていれば承認を拒否する。

#### #3 [major/approval-cas] storage.go:319 【P2】

**内容**: 同じ DB に対して2つ目のインスタンスが起動するのを止める仕組みがありません。2つ目の NewApp() が recoverInterruptedRequests を実行し、1つ目のインスタンスで実行中の running/auto_running 行を interrupted に書き換えます。1つ目のインスタンスの finishRequest はそのあと CAS に負けます。コマンドの結果は失われ、1つ目のインスタンスのメモリ上では、その要求が永久に running のまま残ります。

**再現シナリオ**: インスタンス1で `sleep 1` を承認する（status running）。実行中に、2つ目のプロセスが同じ SSH_GETE_CONFIG_DIR/DB を開く（バイナリの直接起動や `open -n` など）。コマンドはリモートで完了するが、ログには `finish request ...: request state changed` と出る。DB 上は status=interrupted、stdout は空、error_kind=canceled（「結果不明、キャンセル」）になっているが、実際には成功している。インスタンス1のメモリでは running のままなので、get_request_status はエージェントに running を返し続け、DeleteCommandRequest は「実行中」として削除を拒否する。スクラッチコピーで再現した（インスタンス1は status=running、DB は status=interrupted）。

**修正案**: 起動時に DB の単一インスタンスロック（flock / ロックファイル、または Wails の SingleInstanceLock）を取り、ロックを持っている間だけ回収を実行する。または、running 行に所有者/起動 ID を記録し、所有者がもういない行だけを回収する。finishRequest が CAS に負けたら、running を残さず DB からメモリを読み直す。

#### #5 [major/bypass-risk] mcp_server.go:601 【P3】（グループ E）

**内容**: classifyRisk は、前後に空白を足しただけの生のコマンド文字列に部分文字列一致をかけています。シェルの区切り（`;` `&&` `||` `|`、改行）の直後に置いた High コマンドや、クォートや展開で隠した High コマンドは Low に分類されます。needsHumanApproval（approval.go:274）はこの結果だけで判断するので、承認省略を持つエージェントに対しては「High は常にキューに入る」が成り立ちません。

**再現シナリオ**: Bearer を有効にし、エージェント X に承認省略を付与する（例: ApproveCommandAndBypassAgent）。X が execute_command で次のいずれかを送る: `ls;rm important.db`、`cd /srv/app\nrm data.sqlite`、`true&&rm  -rf /var/lib/app`（空白2つ）、`"rm" -rf /`、`r''m -rf /`、`rm${IFS}-rf${IFS}/`、`` `echo rm` -rf / ``、`$(echo cm0gLXJmIC8= | base64 -d)`。関数をスクラッチにコピーして実行すると、すべて risk=Low になる。Low なので needsHumanApproval は false を返し、executeCommandAndLog が人間の承認なしに SSH で実行する。RequireApprovalWriteOp（risk != Low）を有効にしても、同じ Low の結果を見るので効かない。もう一方の分岐も同じで、`systemctl  stop sshd`（空白2つ）も Low になる。

**修正案**: 入力を小文字にしてから、`;` `&` `|` `(` `)` 改行、バッククォート、`$(` を区切りとしてシェルの単語に分割する。各単純コマンドの先頭の単語を、パスの basename を取りクォートを外して検査する。安全に解析できない要素（クォート、`$`、バッククォート、`\`、改行）を含むものは High か Unknown に分類し、Unknown は常にキューに入れる。

#### #7 [major/ssh-exec] ssh_exec.go:85 【P3】

**内容**: `client.NewSession()` は、handshake の期限を解除した後（183行）、ctx を監視する select より前に呼ばれています。そのため、チャネルを開く処理にはコマンドのタイムアウトもキャンセルも効きません。x/crypto の mux.openChannel は `<-ch.msg` でタイムアウトなしに待ちます。

**再現シナリオ**: サーバー（または侵害された・過負荷の sshd）が鍵交換と認証を終えたあと SSH_MSG_CHANNEL_OPEN に一切応答しない。あるいは認証直後に TCP 経路が無応答になる。runSSHCommand は NewSession で永久に止まる。executeSSH（approval.go:338）で設定した context.WithTimeout は無視され、承認済みの要求はいつまでも running のままになり、goroutine と TCP 接続がリークする。相手が生きていて黙っているだけなら、TCP keepalive も役に立たない。

**修正案**: セッションを開く処理を ctx を見ている区間に移す。例えば、openSSHClient が返った直後に、`<-ctx.Done()` でクライアントを閉じる goroutine を起動する。または、セッションが始まるまで netConn の期限を残しておく。

#### #10 [major/storage] app.go:880

**内容**: 一度きりの旧 JSON 取り込みは、トランザクションの中で動いておらず、再試行もできません。migrateLegacyData が動く前に、migrate() が user_version=2 をコミットしてしまっています。そのため取り込みが途中で失敗すると、次回の起動では `store.prevVersion >= 2` ですぐに戻り、二度と取り込みません。エラーはログに出るだけで、storageError も設定されません。

**再現シナリオ**: a6f2706 のコピーで再現した。新規の DB と、`[{name:"web"},{name:"web "},{name:"db"}]` を含む connections.json（2つの web は normalizeConnection の TrimSpace で同じ名前になる）、トークン SECRET を含む mcp-settings.json を用意する。初回起動は `UNIQUE constraint failed: connections.name` で失敗し、取り込まれた接続は1件だけになる。2回目の起動では prev=2 なので migrateLegacyData は nil を返す。結果は conns=1、MCP 設定は読み込まれず、トークンは空。平文の connections.json と mcp-settings.json はリネームされず、ディスクに残り続ける。migrate() のコミット後、取り込みの前にプロセスが落ちた場合も、同じように永久にスキップされる。事前に作られた0バイトのファイルが残っていた場合（fresh=false）は、JSON が取り込まれないまま *.migrated にリネームされる。

**修正案**: JSON の取り込みと user_version=2 の書き込みを同じトランザクションで行う（または「旧データ取り込み済み」のフラグを別に記録し、成功した後だけコミットする）。名前が衝突したら接尾辞を付けるか、その行をスキップして報告し、処理を続ける。失敗したら storageError を設定する。

#### #11 [major/storage] approval.go:139 【P3】（グループ B）

**内容**: approve() は不変 ID で接続先を探し、見つからなければ `findConnection(req.Host)` にフォールバックします。これは名前で、次に Host/IP で一致を探します。これでは不変 ID を使う意味がなくなります。接続が削除された pending 要求が、同じ名前（または一致する Host）を持つ別の接続で実行されます。

**再現シナリオ**: 再現した。接続 'prod'（10.0.0.1、ID A）を作り、queueCommand で pending 要求を積む。'prod' を削除し、別のホスト（127.0.0.1、ID B）を指す新しい 'prod' を作る。ApproveCommand では connectionByID(A) が失敗し、findConnection("prod") が B を返す。要求の connection_id は B に書き換えられ、SSH は新しい接続先に対して実行される。承認画面には同じ名前 'prod' が出ているので、承認する人には接続先が変わったことが分からない。その名前の接続がなければ、Host 文字列が req.Host と一致する SSH 接続が1つだけあるときに、それが選ばれる。

**修正案**: 名前によるフォールバックは、ConnectionID が空のとき（v2 より前の、対応付けできなかった行）だけ使う。ID があるのに見つからない場合は、要求を「接続が削除された」として失敗にする。接続を削除するときに、その接続の pending 要求を rejected/failed に移してもよい。

#### #15 [major/mcp-protocol] mcp_server.go:305 【P3】（グループ D）

**内容**: client_request_id による冪等性チェックは check-then-act の競合になっています。findRequestByClientID（approval.go:396）は a.mu.RLock を取ってすぐ放し、新しい要求はその後、queueCommand/executeCommandAndLog → recordCommandRequest が自分のロックの中で追加します。検索と挿入を不可分にする仕組みがなく、重複を捕まえる UNIQUE インデックスも (requested_by, client_request_id) にありません（storage.go:196-197）。

**再現シナリオ**: 最初の呼び出しがまだ処理中のうちに、エージェントが同じ agent_name と client_request_id で execute_command を再送する（クライアント側の再試行や、並列の再試行など）。リポジトリのスクラッチコピーで再現した。client_request_id=k1 の tools/call execute_command を20本同時に送ると、本来1件のはずの要求が13〜19件できた（3回試行）。エージェントが承認省略を持ち、コマンドが Low/Medium なら、SSH コマンドが本当に複数回実行される。これは仕様の受け入れ条件「同じ client_request_id を再送しても実行は1回だけ」が禁じている二重実行そのもの。キューに入る経路では、重複した pending 要求が複数、承認者に届く。

**修正案**: 検索と挿入を1つの a.mu.Lock の中で行う。または (agent, key) ごとのミューテックスか singleflight で、検索から記録までをまとめて保護する。あわせて、SQLite に `(requested_by, client_request_id) WHERE client_request_id<>''` の UNIQUE インデックスを足して最後の砦にする。

#### #17 [major/concurrency] mcp_server.go:305 【P3】（グループ D）

**内容**: client_request_id の重複チェック（findRequestByClientID）と記録の書き込み（queueCommand / executeCommandAndLog → recordCommandRequest）が1つの不可分な操作になっていません。同じキーが並列に届くと、どの要求もチェックを通り、要求の数だけコマンドが実行されます。

**再現シナリオ**: 承認省略が有効で Bearer トークンで認証済みのエージェントが、同じ agent_name と client_request_id="k1" で execute_command を8本同時に送る（並列の再試行など）。8本とも既存の要求が見つからず、それぞれ実行される。スクラッチコピーで `-race -count=3` で再現した（毎回 srv.Execs()==8）。承認省略のないエージェントでは、同じ競合で pending 要求が複数できる。同じ client_request_id の再送は多くても1回しか実行しない、という保証が壊れている。

**修正案**: a.mu の中で、重複の検索とキーの予約（仮置きの map やメモリ上のリストへの挿入）を一度に行う。または DB に UNIQUE(requested_by, client_request_id) 制約を付け、INSERT の失敗を重複ヒットとして扱う。

#### #18 [major/concurrency] approval.go:241 【P2】（グループ C）

**内容**: DeleteCommandRequest は check-then-act です。findRequest で RLock を取って状態を確認し、ロックを放してから、無条件の `DELETE ... WHERE id=?` とメモリからの削除を行います。その間に承認で要求が running になると、実行中のコマンドが監査履歴から削除されます。

**再現シナリオ**: pending 要求に対して、GUI の ApproveCommand と DeleteCommandRequest が同時に走る。Delete 側の findRequest は pending を見る。そのあと Approve 側の transitionRequest が running に移し、実行が始まる。続いて Delete が SQLite とメモリの両方から行を消す。SSH コマンドは実行され、finishRequest は 'request not found' で失敗し、実行の記録はどこにも残らない。スクラッチコピーでその区間だけを 20ms の sleep で広げたところ、10回中10回とも execs=1、inMem=false、inDB=false になった。

**修正案**: transitionRequest と同じように、状態の確認・DB の削除・メモリからの削除をまとめて a.mu の中で行う。DB 側も条件付きにする（`DELETE ... WHERE id=? AND status NOT IN ('running','auto_running')`、RowsAffected を確認）。

#### #19 [major/concurrency] app.go:306 【P2】

**内容**: フェーズ1は appCtx、wg、決まった終了順序（MCP の受付を止める → 実行中のコマンドを N 秒待ち、その後キャンセルして 'interrupted' を保存 → ターミナルを閉じる → store.Close）を要求しています。しかし現在の終了処理には待ちがありません。stopMCPServer が戻るとすぐに store.Close を呼びます（Shutdown は2秒で打ち切ってから Close）。GUI で承認したコマンドや MCP の自動実行がまだ動いていると、その結果はもう閉じた DB に書き込まれます。

**再現シナリオ**: GUI で承認したコマンドが2秒以上かかる（approve は最大 3600 秒のタイムアウト付きの context.Background を使う）、または承認省略の execute_command が実行中のときに、ユーザーがアプリを終了する。store.Close がすぐに走るので、finishRequest の updateCommandRequestFrom は 'database is closed' で失敗し、結果はログに出るだけになる。行は running/auto_running のまま残り、次回の起動で recoverInterruptedRequests が interrupted にする。実際には完了した（あるいはリモートでまだ動いている）コマンドが、出力も終了コードもない interrupted として記録される。GUI の approve は appCtx に結び付いていないので、そもそもキャンセルされない。

**修正案**: appCtx/appCancel と、approve と executeCommandAndLog をまとめて数える wg を追加する。終了処理では、MCP を止める → タイムアウト付きで wg.Wait → appCancel → 'interrupted' が保存されるまでもう一度待つ → CloseTerminal → store.Close の順にする。approve でも executeSSH に appCtx を渡す。

#### #24 [major/ci-release] .github/workflows/release.yml:13（グループ I）

**内容**: ワークフロー全体に付けた `permissions: contents: write` が、2つの build ジョブにも適用されています。これらのジョブは actions/checkout@v4 を使っていて、その既定値 persist-credentials=true により、GITHUB_TOKEN が .git/config に残ります。そのうえで第三者のコードを実行します。`go install wails@latest` と `wails build` で、後者は frontend:install の `npm install`（依存パッケージのライフサイクルスクリプトを含む）と vite build を実行します。

**再現シナリオ**: 誰かが v* タグを push する。npm の依存（vite、admin-lte、@xterm/*、bootstrap-icons、またはその推移的な依存）か、最新の wails CLI のリリースが乗っ取られていると、その postinstall やビルドのコードが build ジョブの中で .git/config からトークンを読む。contents:write があるので、リポジトリにコミットやタグを push したり、リリースの成果物を作成・差し替えたりできる。利用者は改ざんされたバイナリを受け取ることになる。書き込み権限が必要なのは `release` ジョブだけ。

**修正案**: トップレベルを `permissions: contents: read` にし、`release` ジョブだけに `contents: write` を付ける。build ジョブの checkout に `persist-credentials: false` を付ける。

#### #25 [major/ci-release] .github/workflows/release.yml:44（グループ J）

**内容**: リリースビルドは Wails CLI を `@latest` でインストールしていて、go.mod の wails/v2 v2.12.0 に固定されていません。すべての action（checkout、setup-go、setup-node、upload/download-artifact、第三者の softprops/action-gh-release@v2）が、コミット SHA ではなく可変のメジャータグで参照されています。

**再現シナリオ**:
- ケース1: Wails CLI の新しいリリース（例: v2.13.x）でビルドやテンプレートの生成方法が変わる。同じタグから、ライブラリとは版の違う CLI でバイナリが作られ、ビルドが壊れるか挙動が変わり、再現もできない。
- ケース2: softprops/action-gh-release の v2 タグが悪意あるコードに付け替えられると（tj-actions/changed-files の事件と同じ手口）、そのコードが release ジョブの中で contents:write 付きで動き、リリースの成果物を差し替えられる。

**修正案**: go.mod に合わせて `go install github.com/wailsapp/wails/v2/cmd/wails@v2.12.0` に変える。各 `uses:` を完全なコミット SHA に固定し、版をコメントで書く。

#### #28 [major/tests-gaps] mcp_server.go:601 【P3】（グループ E）

**内容**: classifyRisk は生の部分文字列で一致を見るので、シェルとしては同じ意味の、綴りが違う High コマンドが Low になります。「承認省略でも High は常にキューに入る」という規則（approval.go:274、フェーズ1の受け入れ条件）は破れます。TestBypassPolicyLimits は字義どおりの `rm -rf /tmp/x` しか試していません。

**再現シナリオ**: 既定の設定で Bearer を有効にし、エージェント 'trusted' に承認省略を付与する。execute_command で `"rm" -rf /tmp/x`、`r\m -rf /tmp/x`、`systemctl  restart nginx`（空白2つ）を送ると、どれも risk=Low に分類され、人間の承認なしに auto_* として即時実行される。スクラッチコピーで確認した（3つとも srv.Execs() に出た）。シェルはこれらを rm -rf や systemctl restart とまったく同じように実行するので、破壊的なコマンドがゲートを通り抜ける。クォート、バックスラッシュのエスケープ、空白の連続を扱うテストはない。

**修正案**: 分類の前にコマンドをトークンに分ける（クォートとバックスラッシュを外し、空白をまとめ、argv0 の basename を取る）。または安全側に倒し、クォート、バックスラッシュ、`$( )`、バッククォート、`${IFS}` を High とするか、承認省略の対象外にする。これらの綴りを並べた表駆動テストを追加し、state==pending を確認する。

#### #29 [major/tests-gaps] approval.go:280 【P3】（グループ F）

**内容**: containsWord はフィールドを完全一致で比べるので、クォートした `"sudo"` は RequireApprovalSudo のチェックに引っかからず、承認省略で実行されます。TestBypassPolicyLimits の「sudo はキューに入る」テストは `sudo echo x` を使っています。classifyRisk がこれをすでに Medium と判定するので、sudo 規則に届く前に RequireApprovalWriteOp でキューに入ります。このテストは sudo 規則そのものを検証していません。

**再現シナリオ**: 既定の設定（RequireApprovalSudo=true、RequireApprovalWriteOp=true）で Bearer を有効にし、承認省略付きのエージェントから `"sudo" id` を送る。classifyRisk は `sudo `（後ろに `"` が続く）を見つけられず Low にする。containsWord が作るフィールドは `"sudo"` で、`sudo` とは一致しないので、要求はキューに入らず自動で実行される。スクラッチコピーで確認した（state=auto_*、Execs に出た）。`\sudo id` と `s\udo id` も同じ経路を通る。RequireApprovalSudo を OFF にしても既存のテストは失敗しないので、このテストはこの規則について何も示していない。

**修正案**: 正規化したトークン列（クォートを外し、basename を取ったもの）で sudo を検出する。RequireApprovalWriteOp=false にして、sudo 規則だけで `sudo id`、`"sudo" id`、`/usr/bin/sudo id`、`echo|sudo id` がキューに入ることを確かめるテストを追加する。

#### #31 [major/approval-cas] approval.go:139 【P3】（グループ B）

**内容**: approve() は、要求の接続 ID がもう存在しないと、名前による検索（`findConnection(req.Host)`）にフォールバックします。そのため、接続が削除された pending 要求は、いまその名前を持つ接続で実行されます。そのあと状態遷移で r.ConnectionID と r.Host が新しい接続のものに上書きされ、監査記録から元の接続先が消えます。要求の User は書き換えられないので、記録に残るユーザーも間違ったものになります。

**再現シナリオ**:
1. エージェントが接続 "web"（ID A、127.0.0.1、ユーザー u）に `uptime` を積む。
2. ユーザーが接続 A を削除する。DeleteConnection は pending 要求を pending のまま残す。
3. ユーザーが同じ名前 "web" の新しい接続（ID B、127.0.0.2、ユーザー root、タグ prod）を作る。SaveConnection は既存の接続とだけ名前を比べるので、これは許される。
4. ユーザーが古い要求を承認する。connectionByID(A) が失敗し、findConnection("web") が B を返し、executeSSH は B で root として実行する。保存された記録は connection_id=B、user=u になる。

a6f2706 のコピー上のスクラッチテストで再現した（要求の ConnectionID が B の ID に書き換えられ、SSH が B に対して試みられた）。要求は別のマシン向けに積まれたもので、needsHumanApproval の prod タグや接続先のチェックは B について一度も評価されていない。findConnection は host/IP でも一致を見るので、名前が、Host がその文字列と一致する無関係な接続に解決されることもある。

**修正案**: req.ConnectionID が空でなく connectionByID が失敗したら、名前で探し直さず、要求を failed（接続先削除）で終える。名前によるフォールバックは、connection_id が空の旧データの行だけに残す。または DeleteConnection で、その接続の pending 要求を rejected/failed に移す。状態遷移で ConnectionID を上書きしない。

#### #32 [major/bypass-risk] approval.go:270 【P3】（グループ G）

**内容**: Bearer のチェックと承認省略の判断が、別々の設定を読んでいます。handleMCP は、リスナーが起動したときに取り込んだ設定で Bearer を確認します（mcp_server.go:89）。一方、needsHumanApproval は `GetMCPSettings().BypassAllowed` を通して現在の a.mcp を読みます。SaveMCPSettings と GenerateMCPToken は、restartMCPServer が古いリスナーを止める前に a.mcp を更新します（BearerEnabled=true、新しいトークン）。Shutdown は処理中のリクエストが最大2秒で終わるのを待ちます。そのため、古いリスナーで Bearer なしのチェックを通ったリクエストに対して、承認省略が新しい「Bearer 有効」の状態で判断されます。

**再現シナリオ**:
- 前提: Bearer が有効だったときに、ユーザーが 'claude-desktop' に承認省略を付与した。その後、loopback のリスナーで Bearer を OFF にした。agent_policies の行は取り消されていない。
- 攻撃: 認証のないローカルプロセスが、Bearer OFF のリスナーに POST /mcp を開き、ヘッダーと本文の一部を送って残りを止めておく。io.ReadAll は Bearer チェックの後に走り、ReadTimeout は 30 秒なので、これが成り立つ。ユーザーが「トークン生成」（GenerateMCPToken）を押すか、Bearer を有効にして設定を保存する。Shutdown がリスナーを閉じる（新しい接続が拒否されるので、プロセスはこれを検知できる）。そこでプロセスが残りの本文 `execute_command {agent_name:'claude-desktop', command:'echo pwned'}` を送る。
- 結果: needsHumanApproval は BypassAllowed=true と保存済みのポリシーを見て、トークンも人間の承認もないままコマンドを自動実行する（state=auto_succeeded、decided_by=bypass:claude-desktop）。a6f2706 のスクラッチコピーで再現した。変更前の設定で newMCPHandler を作り、app.GenerateMCPToken() を呼んでから、古いハンドラ経由で認証なしの execute_command を送ると、requires_approval=false、auto_executed=true になり、SSH テストサーバーで 'echo pwned' が実行された。SaveMCPSettings(TokenInput) でトークンを差し替えた場合も同じ欠陥がある。取り消されたトークンを使った処理中のリクエストが承認省略の権限を持ち続ける。

**修正案**: 承認省略の判断を、そのリクエストを認証したのと同じ設定・トークンで行う。リスナーが取り込んだ設定と「このリクエストは有効な Bearer を提示した」というフラグを handleMCP から executeCommandTool と needsHumanApproval に渡し、現在の BypassAllowed ではなくそのフラグを条件にする。または、a.mcp と mcpToken を更新する前に古いリスナーを止める。

#### #33 [major/ssh-exec] ssh_exec.go:144 【P3】

**内容**: ssh-agent への接続には期限がなく、ctx がキャンセルされても閉じられません。handshake を監視する goroutine は netConn しか閉じず、closeAgent は openSSHClient が戻るまで defer されています。agent が応答しないと、認証は `agentClient.Signers()`/Sign の中で止まったままになり、ConnectTimeout も netConn の期限も ctx（CommandTimeout）も効かず、openSSHClient は永久に止まります。

**再現シナリオ**: 固定したツリーで再現した（コピーは `/private/tmp/claude-501/-Users-katouyoshiya-Documents-ssh-gate-oss/d3371860-2ba6-4405-9062-4dd18830d8d2/scratchpad/agenthang/agenthang_test.go`）。SSH_AUTH_SOCK を、接続は受け付けるが一切応答しない agent に向ける（応答されない Touch ID や承認プロンプトを待っている Secretive/1Password 型の agent、または固まった agent）。接続は鍵認証で鍵ファイルがないので、207行の agent だけの経路を通る。runSSHCommand（2秒タイムアウトの ctx、ConnectTimeout=1）は8秒後も止まったままだった。暗号化された鍵の経路も同じで、248行の filtered() がダイヤルより前に `agentClient.Signers()` を同期的に呼ぶ。影響として、MCP の execute_command の goroutine が CommandTimeout を過ぎても running のまま残り、testSSHConnection / startSSHSession では UI の操作が止まる。

**修正案**: agent のソケットに期限を付ける。ダイヤルした直後に `agentConn.SetDeadline(now+timeout)` を設定し、handshake 監視の ctx.Done 分岐でも agent を閉じる（closeAgent はすでに Once で保護されている）。248行のダイヤル前の Signers() 呼び出しにも同じ期限を付ける。

#### #34 [major/storage] storage.go:50

**内容**: openStore は、DB ファイルが存在するかどうかだけで `fresh` を決め、SQLite が開く前に空の 0600 ファイルを作ります。初回起動が migrate のコミット前に失敗するか強制終了されると、0バイトのファイルが残ります。次回の起動では fresh=false、prevVersion=0 になります。migrateLegacyData は取り込みをスキップします（importData = store.fresh）が、connections.json と mcp-settings.json は *.migrated にリネームします。旧版の接続、パスワード、MCP トークンがすべて、何の知らせもなくアプリから消えます。

**再現シナリオ**: JSON だけの旧版から更新する。v1 の初回起動で `os.OpenFile(path, O_CREATE, 0600)` が走り、移行トランザクションがコミットされる前に落ちる（クラッシュ、強制終了、migrate のエラー、ディスクフル）。0バイトの ssh-gete.sqlite が残る。次回の起動ではファイルが存在するので fresh=false になり、user_version は 0 と読まれる。migrateLegacyData は importData=false にして、旧 JSON を *.migrated にリネームする。a6f2706 のスクラッチコピーで再現した（`/private/tmp/claude-501/-Users-katouyoshiya-Documents-ssh-gate-oss/d3371860-2ba6-4405-9062-4dd18830d8d2/scratchpad/st2/zz_audit_test.go`）。空の DB ファイルと、パスワード接続を1件含む connections.json を事前に用意し、openStore + migrateLegacyData を呼ぶ。結果は fresh=false prev=0 conns=0 jsonExists=false migratedExists=true。接続は取り込まれず、リネームされたファイルだけが残った。

**修正案**: 新規かどうかをファイルの有無ではなく DB の中身で決める（user_version==0 で、CREATE の前に connections にも sqlite_master にも行がない）。または、仕様 §5.5 が示すとおり、旧データの取り込みを schema_meta のフラグ（例: legacy_migrated）として記録する。JSON のリネームは取り込みが成功した後だけにする。

#### #38 [major/concurrency] terminal.go:232 【P2】

**内容**: SendTerminalInput に送ったキー入力（と ResizeTerminal に送ったリサイズ）は、順序が入れ替わってセッションに届くことがあります。darwin では、Wails v2.12.0 はバインドされたメソッドの呼び出しを1つずつ別の goroutine で動かします（internal/frontend/desktop/darwin/frontend.go:427 `go func(){ f.dispatcher.ProcessMessage(...) }`）。フロントエンドは xterm の onData のたびに SendTerminalInput(data) を await せずに呼びます（frontend/src/main.js:1318）。SendTerminalInput は termMu の中で a.term をコピーするだけで、そのあと順序の保証なしに ts.stdin.Write を呼びます。PTY への書き込み順は goroutine がたまたま動いた順になります。

**再現シナリオ**: ユーザーが SSH ターミナルで速く打つか、複数の onData チャンクに分かれる貼り付けをする（例: `rm -rf ./build/tmp` と Enter）。1文字・1チャンクごとに別の goroutine で Wails 呼び出しになる。'\r' を運ぶ goroutine が先の文字を運ぶ goroutine より先に stdin.Write すると、リモートのシェルは途中までのコマンド（例: `rm -rf ./build`）を実行し、残りの文字は次のプロンプトに入る。単純に文字が入れ替わることもある（'/tmp' が '/tpm' になる）。ウィンドウを続けてリサイズしたときの ResizeTerminal も同じで、古い WindowChange が最後に適用されると PTY のサイズが間違ったまま残る。ここは承認ゲートのない対話経路なので、順序が入れ替わった破壊的な行がそのまま実行される。

**修正案**: バックエンドで入力を直列化する。terminalSession ごとにバッファ付きチャネルと書き込み専用の goroutine を1つ持たせるか、ミューテックスとフロントエンドが付ける連番で到着後に並べ直す。またはフロントエンドで SendTerminalInput の Promise をつなぎ、前の呼び出しが解決してから次を送る。リサイズも同じようにまとめ、最新のサイズだけを残す。フェーズ2の「セッションごとの入力 goroutine」「送信キュー」でこれを満たすこと。

#### #43 [major/ci-release] .github/workflows/release.yml:79（グループ J）

**内容**: 第三者の action である softprops/action-gh-release は、コミット SHA ではなく可変タグ @v2 で固定されています。しかも contents:write の GITHUB_TOKEN 付きで動き、リリースのバイナリを受け取ります。ほかの action もすべてタグで固定されているだけです。

**再現シナリオ**: 上流の v2 タグが動かされると（メンテナの侵害や、tj-actions の事件のような付け替え）、次の `git push --tags v*` で攻撃者のコードが release ジョブの中で動く。そのコードは contents:write のトークンとダウンロード済みの dist/*.zip を持っているので、公開される ssh-gete のバイナリをトロイの木馬入りに差し替えたり、リポジトリにコミットやタグを push したりできる。ワークフローには SHA 固定もチェックサムも署名もなく、これを検知できない。

**修正案**: すべての `uses:` を完全なコミット SHA に固定し（`# v2.x.y` のコメント付き）、Dependabot（github-actions エコシステム）で更新する。release ジョブだけに `permissions: contents: write` を付け、ワークフローの既定は read にする。

#### #44 [major/ci-release] .github/workflows/release.yml:31（グループ I）

**内容**: build ジョブでは actions/checkout@v4 が既定の persist-credentials: true で動くので、contents:write の GITHUB_TOKEN が .git/config に書かれます。そのあと `wails build` が `frontend:install` = `npm install`（wails.json）を実行し、同じワークスペースで依存ツリーの npm ライフサイクルスクリプトを実行します。

**再現シナリオ**: vite、admin-lte、@xterm の推移的な npm 依存のどれかが、lockfile が解決する版で悪意ある postinstall を出したとする（lockfile にはすでに hasInstallScript のエントリがある）。タグのビルド中に、そのスクリプトは .git/config から http.extraheader のトークンを読み、contents:write でブランチやタグに push したり、Release を編集・作成したりできる。これは公開ジョブより前に起きるので、利用者がダウンロードするバイナリを差し替えられる。build ジョブには git の資格情報も書き込み権限もまったく必要ない。

**修正案**: build ジョブの checkout に `persist-credentials: false` を付け、build ジョブに `permissions: contents: read` を付ける。`npm ci --ignore-scripts` も検討する（frontend:install を `npm ci` に変える）。

#### #46 [major/tests-gaps] mcp_server_test.go:458 【P3】（グループ F）

**内容**: TestBypassPolicyLimits の「sudo はキューに入る」のチェックは、RequireApprovalSudo の規則まで届いていません。classifyRisk が 'sudo ' を Medium にするので、`sudo echo x` は既定の RequireApprovalWriteOp 規則ですでにキューに入ります。sudo の規則（approval.go:280）には実際に効いているテストがなく、その規則だけが捕まえるはずの入力は、既定の設定のままで SSH まで届きます。

**再現シナリオ**: 監査対象のツリーではなく、スクラッチコピーでだけ確認した。approval.go:280 を `if false && containsWord(...)` に変えても、TestBypassPolicyLimits を含む `go test .` 全体が通った。既定の設定（Bearer 有効、エージェント 'trusted' に承認省略、RequireApproval* はすべて true）で実際に承認省略を使い、`'sudo' id`、`s\udo id`、`/usr/bin/sudo<TAB>id` を execute_command で送ると、一度もキューに入らない。スクラッチコピーでは、3つとも classifyRisk は Low、containsWord(cmd,"sudo") は false だった。WriteOp 規則も（risk が Low なので）sudo 規則も発火せず、needsHumanApproval は false を返す。リモートのシェルは人間の承認なしに sudo を実行し、どのテストも失敗しない。

**修正案**: RequireApprovalWriteOp=false、RequireApprovalSudo=true にした表のケースを追加し、`sudo echo x` がキューに入ることを確かめる。既定の設定で、Low に分類される sudo の形（`'sudo' id`、`s\udo id`、`/usr/bin/sudo\tid`）がキューに入ることも確かめる。WriteOp=false のケースでは `rm -rf /tmp/x` も扱う。同じスクラッチコピーで `risk == "High"` のチェック（approval.go:274）を外しても全テストが通ったので、仕様の「High は常に承認が必要」という規則にも、効いているテストがない（#55）。

#### #48 [major/bypass-risk] mcp_server.go:604 【P3】（グループ E）

**内容**: classifyRisk の High の一覧は、各コマンドについて決まった1つの綴りにしか一致しません。難読化していない、ふつうの破壊的コマンドでも、書き方が違えば Low になります。承認省略の権限を持つエージェントは、設計上「High は常にキュー」であるはずのものを承認なしで実行できます。

**再現シナリオ**: 既定の設定（RequireApprovalWriteOp/Sudo/Prod が ON）、Bearer 有効、エージェント 'claude-desktop' に承認省略、接続先に prod タグなし。エージェントが execute_command で `dd of=/dev/sda if=/dev/zero` を送る。パターンの 'dd if=' は if= が先に来ることを前提にしているので一致しない。ほかの例: `systemctl poweroff`、`poweroff`、`systemctl  stop sshd`（空白2つ）、`systemctl disable --now sshd`、`shred -u /etc/x`、`crontab -r`、`find / -delete`。classifyRisk をスクラッチプログラムにコピーして実行すると（`/private/tmp/claude-501/-Users-katouyoshiya-Documents-ssh-gate-oss/d3371860-2ba6-4405-9062-4dd18830d8d2/scratchpad/cr/main.go`）、どれも Low を返す。risk が Low なので needsHumanApproval は false を返し、executeCommandAndLog がすぐに auto_running として実行する。

**修正案**: コマンドをトークンに分け（空白をまとめた後）、決まった「名前 + 引数」の文字列ではなくコマンド名で照合する。順番に関係なく if= か of= を持つ dd、poweroff/init/telinit、状態を変える systemctl のすべての動詞、shred、crontab -r、find -delete を High にする。これらの形のテストを追加する。

#### #49 [major/bypass-risk] approval.go:277 【P3】

**内容**: RequireApprovalWriteOp は `risk != "Low"` として実装されていますが、classifyRisk はふつうのファイル書き込みを Low と判定します。スイッチが ON（既定）でも、承認省略の権限を持つエージェントは承認なしでファイルを上書きできます。

**再現シナリオ**: 既定の設定で RequireApprovalWriteOp=true。Bearer 有効、エージェントに承認省略、接続先に prod タグなし。エージェントが `echo 'ssh-ed25519 AAAA... attacker' > /home/u/.ssh/authorized_keys`、`cp /tmp/x /etc/sudoers`、`chmod 777 /etc/shadow`、`tee /etc/hosts < /dev/null` を送る。シェルのリダイレクト、tee、-r なしの cp、-R なしの chmod/chown はどちらのパターン一覧にもないので、classifyRisk はどれも Low を返す。277行の WriteOp チェックは発火せず、needsHumanApproval は false を返し、書き込みは auto_succeeded として実行される。書き込み操作に人間の承認を保証するための設定が、最もよくあるファイルの書き方を捕まえていない。

**修正案**: WriteOp のチェック自体が書き込みを検出するようにする（コマンドをトークンに分けたうえで、`>`、`>>`、tee、cp、install、chmod、chown、ln、sed -i など）。または classifyRisk がこれらを少なくとも Medium と判定し、`risk != Low` が成り立つようにする。

#### #50 [major/storage] storage.go:150

**内容**: 移行前のバックアップを取ったかどうかを、`<db>.bak-v<N>` ファイルが存在するかどうかだけで判断しています。そのため、途中で切れた・失敗した VACUUM INTO の出力が正常なバックアップとして扱われ、以後の起動ではバックアップも chmod 0600 もスキップされます。

**再現シナリオ**: v0/v1 の DB で移行を始める。VACUUM INTO が `<db>.bak-v0` を書いている途中でプロセスが落ちる（アプリの強制終了、クラッシュ、停電、I/O エラーやディスクフル）。modernc sqlite v1.52.0 で再現した。VACUUM INTO の最中に kill -9 すると 273MB のファイルが残り、開くと 'no such table' になった。154行の os.Chmod が走らなかったので、ファイルのモードは -rw-r--r-- だった。移行トランザクションがコミットされていないので、次回の起動でも prevVersion は 0 のまま。fileExists(backup) が true なので VACUUM INTO も Chmod もスキップされ、v2 の移行は、戻せるのが壊れたバックアップだけという状態でコミットされる。途中までのバックアップにはパスワードと Bearer トークンのページが入っていて、誰でも読める状態（umask 022 で 0644）のまま残る。restrictDBFilePermissions は .bak ファイルを対象にしていないので、これが直ることはない。

**修正案**: バックアップを一時的な名前（例: .bak-vN.tmp）に書き、検証し（PRAGMA integrity_check / quick_check）、0600 に chmod してから、os.Rename で最終的な名前にする。検証されていないバックアップがすでにあれば、削除して作り直す。restrictDBFilePermissions に .bak ファイルも加える。

#### #52 [major/concurrency] mcp_server.go:89 【P3】（グループ G）

**内容**: リクエストは、リスナーを作ったときに取り込んだ `settings`/`token`（newMCPHandler、httpguard.go:230）で認証されます。一方、承認省略の判断（approval.go:270 の needsHumanApproval、GetMCPSettings().BypassAllowed 経由）は現在の a.mcp を読みます。GenerateMCPToken と SaveMCPSettings（app.go:612-631 と 577-600）は、restartMCPServer が古い Bearer なしのリスナーを止める前に、BearerEnabled=true と新しいトークンを a.mcp に反映して a.mu を放します。そのあと Shutdown は処理中のハンドラを最後まで走らせます。そのため、古い認証なしのチェックを通ったリクエストが、新しい「Bearer 有効」の状態で判断されます。

**再現シナリオ**: 前提として Bearer は OFF で、エージェント 'claude-desktop' 向けの ApprovalBypass ポリシーが保存されている（Bearer が ON のときに付与し、その後 Bearer を OFF にした。Bearer が OFF の間はこのポリシーは効かない）。認証のないローカルプロセスが、agent_name=claude-desktop で execute_command を送り続ける。ユーザーが「トークン生成」を押す。古いリスナーが受け付けたリクエストのうち、needsHumanApproval の呼び出しが a.mcp の更新後に走ったものは、Authorization ヘッダーなしで decided_by=bypass:claude-desktop として自動実行される。GenerateMCPToken の Lock の後ろで a.mu.RLock を待っていたリクエストもこれに含まれる（RWMutex は書き込み側を優先するので、更新が終わってから解放される）。スクラッチコピーで再現した。古い設定でハンドラを作り、GenerateMCPToken の a.mcp/mcpToken の更新を適用してから、古いハンドラ経由で Authorization なしの POST を送ると、state=auto_succeeded になり、SSH テストサーバーが 'echo pwned' を受け取った。

**修正案**: 承認省略は、このリクエストが実際に証明したもので判断する。例えば handleMCP が「トークン T で Bearer を検証した」（checkBearer が成功した）ことを記録して executeCommandTool/needsHumanApproval に渡し、現在の a.mcp を読み直すのではなく、そのフラグが true のときだけ承認省略を許す。または、新しい Bearer 設定を反映する前に古いリスナーを止める。

#### #53 [major/frontend-xss] frontend/src/main.js:1063 【P2】【P3】（グループ A）

**内容**: キューの行をクリックしていないとき、詳細ペインにはリストの先頭にある pending 要求が表示され、その選択は保存されません。承認・承認省略つき承認・拒否は、ポーリングで取り直したばかりのデータから要求を探し直します。ところが承認と承認省略つき承認は、画面に残っている textarea からコマンド文字列を取ります。そのため1回のクリックで、要求 A の（編集したかもしれない）コマンドが要求 B の id で送られて B のホストで実行されたり、ユーザーが一度も見ていない要求が拒否されたりします。

**再現シナリオ**: バックエンドは要求を新しい順に返す（storage.go:476 ORDER BY created_at DESC）。ユーザーがコマンド画面を開くと、既定で pending 要求 A（ホスト dev）が表示され、state.selectedRequestId は `''` のまま。ユーザーが #approval-command のコマンドを編集すると state.dirty.approval が立ち、refreshData は再描画をやめる。5秒のポーリングの間にエージェントが pending 要求 B（ホスト prod）を出し、ポーリングがそれを state.data.requests の A より前に入れる。ユーザーが「承認」を押すと、selectedRequest = selectedCommandRequest() は B（最初の pending）を返し、コマンドは古い DOM にある A の編集済みテキストになる。`ApproveCommand(B.id, editedA)` が A のコマンドを B の接続先（prod）で実行する。approve() が編集を止めるのは、その risk が B の元の risk より高いときだけなので、同じか低い risk のコマンドは通る。reject-command にも同じずれがあり、画面には A が出ているのに B を拒否する。編集していないときは、ポーリングで再描画されてペインが黙って A から B に入れ替わるので、5秒の区切りの直後にクリックすると B を承認してしまう。

**修正案**: ペインを描画するときに選択を確定させる（renderCommands/commandDetail の中で state.selectedRequestId を selected.id にする）。または承認・拒否ボタンと textarea に data-request-id を付け、ポーリングした state ではなくクリックした要素から id を読む。その id が textarea を描画したときの id と違えば、操作を拒否する。フェーズ2の「draft と editor.selectedId を id にする」「dataGen」と、フェーズ3の「承認画面に解決済みの実行先と fp の変化を出す」の中で直す。

#### #55 [major/tests-gaps] mcp_server_test.go:456 【P3】

**内容**: TestBypassPolicyLimits の「high risk はキューに入る」というアサーションは、「High は常に承認が必要」の規則が壊れても検出できません。RequireApprovalWriteOp は既定で true で、Low 以外のコマンドはすべてそれでキューに入るので、High の分岐があってもなくても `rm -rf /tmp/x` はキューに入ります。報告済みの sudo チェック（#46）と同じパターンですが、こちらはポリシーの中心となる不変条件を隠しています。

**再現シナリオ**: a6f2706 のスクラッチコピーでミューテーションを試した。approval.go:274 を `if false && risk == "High"` に変えても、go test の全体が緑のままだった。本番で、ユーザーが RequireApprovalWriteOp を OFF にし（サポートされている設定）、承認省略を信頼された Bearer のエージェントが `rm -rf /`、`shutdown`、`iptables ...`、`systemctl stop` を送る。High の分岐が将来壊れると、これらが人間の承認なしに実行され、それを捕まえるテストはない。

**修正案**: RequireApprovalWriteOp=false（RequireApprovalSudo/Prod も OFF）のケースを追加し、High のコマンドが pending のままで、srv.Execs() が空であることを確かめる。

### 3.3 minor

#### #4 [minor/approval-cas] approval.go:237 【P2】（グループ C）

**内容**: DeleteCommandRequest は、ロックなしで取ったスナップショット（findRequest）で状態を確認し、そのあと DB の行とメモリのエントリを WHERE status なしで無条件に削除します。transitionRequest の CAS を通らないので、approve と同時に走ることがあります。また、実行済みの記録（succeeded/failed/auto_*）も物理削除し、二度と戻せません。

**再現シナリオ**: 競合は DeleteCommandRequest の中にある。findRequest が pending を返す。削除の前に approve の transitionRequest が pending→running に移し、SSH の実行が始まる。そのあと `DELETE FROM command_requests WHERE id=?` とメモリからの削除が走る。コマンドは監査記録のないまま実行され、finishRequest は 'request not found' をログに出すだけで、エージェントの get_request_status は not_found を返す（この区間はマイクロ秒単位で、Approve+Delete を400回並行で走らせても再現しなかったが、コード上は起こりうる）。それとは別に、実際に実行された完了済みの記録を、GUI の操作1回で履歴から消せる。

**修正案**: a.mu の中で CAS として削除する（`DELETE ... WHERE id=? AND status IN (終了状態)`）。行が消えたときだけメモリを更新する。仕様の「監査記録は削除できない」に合わせ、論理削除（deleted_at）にして、実行済みの記録の削除は拒否する。

#### #6 [minor/bypass-risk] approval.go:294 【P3】（グループ F）

**内容**: containsWord は、空白・`;` `|` `&` `(` `)`・改行で分けたフィールドを完全一致で比べるだけです。RequireApprovalSudo が ON でも、sudo にパスやクォートを付けるとチェックを通り抜け、classifyRisk（Medium か Low）によってはそのまま自動実行されます。

**再現シナリオ**: Bearer 有効、エージェント X に承認省略、RequireApprovalSudo=true、RequireApprovalWriteOp=false。X が `/usr/bin/sudo cat /etc/shadow` を送ると、classifyRisk は Medium、containsWord("sudo") は false で、自動実行される。`"sudo" id` は risk=Low、containsWord=false で、これも自動実行される。どちらも関数のコピーを実行して確認した。ユーザーが「sudo は常に承認」を ON にしていても、特権コマンドが承認なしに実行される。

**修正案**: 各単純コマンドの先頭の単語について、クォートを外して basename を取ったうえで sudo を検出する。doas、su、pkexec も同じように扱う。クォートや展開を含むコマンドはキューに入れる。

#### #8 [minor/ssh-exec] ssh_exec.go:151 【P3】

**内容**: strict なホスト鍵モードでも、ClientConfig.HostKeyAlgorithms が、そのホストについて known_hosts に記録された鍵の種類に絞られていません。Go の既定の順序（x/crypto v0.58.0 の defaultHostKeyAlgos）は ECDSA と RSA を ED25519 より前に置くので、正当なホストが鍵の不一致として拒否されます。

**再現シナリオ**: ~/.ssh/known_hosts に、ホスト X について ssh-ed25519 のエントリだけがある（OpenSSH が ed25519 を先に交渉した）、または古い ssh-rsa のエントリだけがある。sshd は ecdsa-sha2-nistp256 も提示する（Ubuntu/Debian は既定で rsa、ecdsa、ed25519 の鍵を作る）。Go は ecdsa-sha2-nistp256 で交渉し、knownhosts は KeyError{Want:[ed25519]} を返す。strict モードでは、正しく登録されたホストへの接続がすべて拒否され、ユーザーは strict モードを切るしかなくなる。

**修正案**: ダイヤルの前に、ダミーの鍵で knownhosts のコールバックを一度呼ぶ（または knownhosts の KeyError.Want を使う）ことで、その addr について既知の鍵の種類を集め、それを config.HostKeyAlgorithms に設定する。ssh-rsa には rsa-sha2-256/512 の変種も含める。フェーズ3の TOFU（`knownhosts.Normalize`、Want による判定）と同時に直す。

#### #9 [minor/ssh-exec] ssh_exec.go:294 【P3】

**内容**: expandPath は鍵のパスに対して、常に os.ExpandEnv と expandWindowsEnv をかけます。そのため、実在するファイルパスに含まれる `$` や2つの `%` が黙って書き換えられ、別のファイルを読んだり、読み込みに失敗したりします。

**再現シナリオ**: 認証情報 `/Users/u/keys/id$prod` は、$prod が空に展開されるので `/Users/u/keys/id` になる。そのファイルが読まれて ID として提示される（または読み込みが失敗して agent へのフォールバックになる）ので、認証に間違った鍵が使われる。macOS/Linux では `/keys/50%a%b` が `/keys/50${a}b` になり、さらに `/keys/50b` になる。

**修正案**: 展開するのは `~`（と Windows の %VAR%）だけにする。または、設定されている変数だけを展開し、書いたとおりのパスが存在するなら `$` には触らない。

#### #12 [minor/storage] storage.go:151

**内容**: VACUUM INTO で作る移行前のバックアップは既定の権限（0644）で書かれ、os.Chmod(0600) はその後でしか走りません。バックアップには平文の認証情報と MCP トークンが入っていて、chmod までの間は他のユーザーから読めます。

**再現シナリオ**: 確認済み。元の DB が 0600 でも、VACUUM INTO の直後のファイルモードは -rw-r--r-- になる。MkdirAll(0700) は、すでにあるディレクトリの権限を締めない。設定ディレクトリが誰でも読める状態（例: SSH_GETE_CONFIG_DIR に 0755 のディレクトリを指定）なら、別のローカルユーザーが作成から chmod までの間に ssh-gete.sqlite.bak-v1 を open() できる。開いたファイル記述子は chmod の後も有効なので、すべてのパスワードとトークンを読める。chmod が失敗してもエラーは無視され、ファイルは 0644 のまま残る。

**修正案**: VACUUM INTO の前後で umask を 077 にする。または、O_EXCL で作った 0600 のファイル（または 0700 のディレクトリ内の一時ファイル）に VACUUM INTO してからリネームする。すでにある設定ディレクトリも 0700 に chmod する。

#### #13 [minor/storage] approval.go:235 【P2】（グループ C）

**内容**: DeleteCommandRequest はメモリ上の状態（findRequest）を確認してから削除し、SQL の deleteCommandRequest は無条件の `DELETE ... WHERE id = ?` です。compare-and-swap になっていないので、approve の pending→running の遷移と競合し、実行中の要求の監査記録を削除することがあります。

**再現シナリオ**: Wails はバインドされた呼び出しを並行に動かす。DeleteCommandRequest(id) は pending を見てチェックを通る。その間に approve() が DB で要求を running に移し、SSH を開始する。DeleteCommandRequest はそのあと DB とメモリから行を削除する。コマンドはリモートで実行され、finishRequest は 'request not found' で失敗し、実行された記録は DB に何も残らない。

**修正案**: SQL で条件付きの削除にする（`DELETE FROM command_requests WHERE id=? AND status NOT IN ('running','auto_running')`）。transitionRequest と同じく、メモリの確認と削除を a.mu の中で行う。

#### #14 [minor/secrets-leak] app.go:1087 【P3】

**内容**: maskToken は、8文字より長いトークンの先頭4文字と末尾4文字を表示します。ユーザーが TokenInput に入れた短いトークンや、SSH_GETE_TOKEN で設定したトークンでは、表示だけで Bearer トークンのほぼ全体が分かってしまいます。sanitizeMCPSettings はこの表示を TokenPreview に入れ、GetInitialData と GetMCPSettings のたびにフロントエンドへ送ります。

**再現シナリオ**: ユーザーが TokenInput に9文字のトークン 'hunter2pw' を入れて保存する。設定画面の「保存済みトークン」欄には 'hunt************************r2pw' と表示される。9文字中8文字が見えていて、隠れているのは真ん中の 'e' だけ。設定画面のスクリーンショットを見た人や、フロントエンドの state にアクセスできる人は、多くても数十回試せばトークンを割り出せる。その後、認証済みのエージェントとして execute_command や /audit/events を呼べる。Bearer 認証は承認省略を有効にする条件でもあるので、Low/Medium のコマンドを承認なしで実行するのにも使える。10〜15文字のトークンでも、長さの半分以上が見える。GenerateMCPToken が作るトークン（16進64文字）は実質的な影響を受けない。

**修正案**: 固定のマスクだけを表示するか、表示するとしても末尾4文字までにし、それも len(token) >= 32 のときだけにする。短いトークンについては、長さの約10%を超えて見せない。

#### #16 [minor/mcp-protocol] approval.go:396 【P3】

**内容**: 重複排除は、メモリ上の a.requests しか探しません。起動時、このリストは loadCommandRequests(500)（app.go:278）で作られ、未完了の要求と、新しい順に500件しか読み込みません。そのため再起動の後は、24時間の冪等性の約束が、その期間内でも古い完了済みの要求には効きません。

**再現シナリオ**: 24時間以内に500件を超える要求があり、そのあとアプリを再起動する。エージェントが、完了済みの要求が新しい500件に入っていない client_request_id を再送する。findRequestByClientID は何も見つけず、新しい要求が作られ、承認省略のコマンドが2回目に実行される。これはツールの説明「同じ値での再送は新規実行せず既存の request_id を返します（24時間）」と矛盾する。

**修正案**: メモリでの検索に外れたら、仕様がすでに予定しているインデックスを使って、SQLite を (requested_by, client_request_id, created_at >= now-24h) で検索する。または、client_request_id を持つ直近24時間の要求を常に読み込む。

#### #20 [minor/concurrency] terminal.go:146 【P2】（グループ H）

**内容**: startSSHSession と startSerialSession は termMu を持たずにダイヤル・オープンし、そのあと setTerminalSession でセッションを登録します。その間に走った DeleteConnection や DisconnectConnection（closeTerminalsForConnection）にはそのセッションが見えないので、削除・切断した接続のターミナルが生き残ります。

**再現シナリオ**: ユーザーが handshake の遅いホスト（最大 ConnectTimeout = 10秒）でターミナルを開き、待っている間にその接続を削除する。DeleteConnection の closeTerminalsForConnection は a.term==nil を見て何もしない。そのあと handshake が完了し、setTerminalSession がセッションを登録する。削除した接続への PTY が開いたまま残り、入力もできる。terminalCounts も、もう存在しない connID を報告し続ける。

**修正案**: setTerminalSession で登録する前に termMu を取り、connectionByID で接続がまだ存在して接続先が同じであることを確かめる（または世代カウンタを確認する）。そうでなければすぐに閉じる。または、削除・切断で進行中の開始処理をキャンセルする。

#### #21 [minor/concurrency] app.go:647 【P3】

**内容**: DeleteMCPSettings は、a.mu を取って設定をリセットする前に stopMCPServer を呼ぶので、この2つの処理が直列になっていません。その間に並行して SaveMCPSettings や GenerateMCPToken が restartMCPServer を呼ぶと、a.mcp と DB は「無効」なのに、動いているリスナーが残ります。

**再現シナリオ**: DeleteMCPSettings がサーバーを止める。次に SaveMCPSettings が a.mcp を enabled に更新して restartMCPServer を実行し、古いトークンと設定を取り込んだ新しいリスナーが起動する。そのあと DeleteMCPSettings が Enabled=false と mcpToken="" を a.mcp と DB に書く。UI はリセット後に「停止中」と表示するが、ポートは開いたままで、次の再起動まで古いトークンで execute_command を受け付け続ける。

**修正案**: DeleteMCPSettings で最初に lifecycleMu を取り、それを持ったまま stopMCPServerLocked を呼んで a.mcp と DB を更新する（再起動の経路と同じ順序にする）。

#### #23 [minor/windows-unix] app.go:845

**内容**: appConfigDir() は、UserConfigDir と UserHomeDir の両方が失敗すると `filepath.Join(os.TempDir(), "ssh-gete")` にフォールバックします。Linux ではこれは共有の、誰でも書き込める /tmp の中です。openStore（storage.go:47-58）は、そこにすでにあるディレクトリと DB ファイルをそのまま使います。他のユーザーが所有する既存のディレクトリに対しても MkdirAll(0o700) は成功し、ファイルがすでにあれば 0o600 での作成はスキップされます。ファイルやディレクトリの所有者は誰も確認しません。

**再現シナリオ**: Linux で、HOME と XDG_CONFIG_HOME が設定されていない状態でアプリを起動する（例: systemd ユニットや `env -i`）。その前に、別のローカルユーザーが /tmp/ssh-gete/ssh-gete.sqlite（その人の所有、モード 0666）を作っておく。中には、その人が知っているトークンを入れた mcp_settings の行と、approval_bypass=1 の agent_policies の行を入れておく。fresh=false なので、アプリはその DB を自分の設定として開く。攻撃者は既知の Bearer トークンで 127.0.0.1:8787 に execute_command を送り、被害者が登録した SSH ホストで承認なしにコマンドを実行できる。被害者が後から登録するパスワードや鍵パスも、攻撃者が読めるファイルに書き込まれる。被害者が自分でファイルを作った場合でも、ディレクトリを所有する攻撃者は DB とその -wal/-shm ファイルを差し替えたり削除したりできる（macOS の TempDir は /var/folders 下のユーザーごとの場所なので影響を受けない。Windows の TempDir もユーザーごと）。

**修正案**: TempDir をフォールバックに使わない。エラーを返して起動を拒否する。どうしてもフォールバックを残すなら、os.MkdirTemp などで新しいディレクトリを作り、ディレクトリと DB ファイルの両方を Lstat して、所有者が現在の uid で、モードが 0700/0600 であることを確かめる。そうでなければ起動を拒否する。

#### #26 [minor/ci-release] .github/workflows/test.yml:42 【P2】（グループ K）

**内容**: govulncheck ジョブは ubuntu-latest で既定の GOOS=linux でしか走らず、ツールも `@latest` でインストールされています。govulncheck は現在のビルドタグで選ばれたファイルしか解析しないので、Windows でだけ到達するコード（go-winio と x/sys/windows を使う ssh_agent_windows.go）と darwin でだけ到達するコード（darwin_frameworks.go）は一度も検査されません。一方、リリースには Windows と macOS のバイナリが含まれています。

**再現シナリオ**: go-winio の DialPipe か x/sys/windows に、Windows のビルドだけが到達するコード経路の脆弱性が公開される。linux だけの govulncheck はそのシンボルを到達不能とみなして通すので、CI は警告を出さず、脆弱な Windows バイナリがリリースされる（a6f2706 のスクラッチコピーでは GOOS=linux でも GOOS=windows でも現在の影響は0件なので、いま何かが露出しているわけではない。これは検査の欠落であって、現在の脆弱性ではない）。

**修正案**: govulncheck を GOOS が linux、windows、darwin のマトリクスで走らせる（例: `GOOS=windows govulncheck ./...`）。govulncheck の版を固定する。フェーズ2の Integrator が test.yml を触るときに一緒に直す。

#### #27 [minor/ci-release] .github/workflows/release.yml:3

**内容**: release.yml は test.yml に依存しておらず、自分でも go test や govulncheck を走らせません。v* タグを push する（または workflow_dispatch を実行する）と、テストの結果に関係なくビルドして公開します。

**再現シナリオ**: `go test -race` か govulncheck が失敗しているコミット（例: 承認ゲートのセキュリティ上の後退）にタグを push する。それでも build と release のジョブは成功し、壊れた・脆弱なバイナリが、自動生成のリリースノート付きで GitHub Releases に公開される。

**修正案**: release.yml にテストのジョブ（go vet、go test、govulncheck）を追加し、build を `needs` でそれに依存させる。

#### #30 [minor/tests-gaps] mcp_server.go:305 【P3】（グループ D）

**内容**: client_request_id の冪等性チェック（findRequestByClientID）は要求の記録と不可分になっておらず、(requested_by, client_request_id) に UNIQUE インデックスもありません。同じキーで並行に再送すると、すべて実行されます。TestClientRequestIDIsIdempotent は順番に送るだけです。

**再現シナリオ**: 承認省略のエージェントが、同じ client_request_id=k1 で execute_command を5本同時に送る（クライアントが並列に再試行する、または送信が重複する）。5本とも、どれかが記録される前に findRequestByClientID を通り、5本とも実行される。スクラッチコピーで3回試し、毎回 execs=5、requests=5 になった。再送は1回しか実行しないという約束が破れている。承認省略がなければ、同じ pending 要求が5件たまる。

**修正案**: 検索と recordCommandRequest を1つの a.mu のクリティカルセクションで行う（または SQLite に requested_by + client_request_id の UNIQUE 部分インデックスを足し、衝突したら重複として扱う）。execs==1 を確かめる並行テストを追加する。

#### #35 [minor/secrets-leak] frontend/src/main.js:2177 【P2】

**内容**: 認証方式をパスワードから鍵に変えるとき、フォームは認証情報を残すかどうかをパスの正規表現 `/^(~|\/|[A-Za-z]:\\|%)/` で決めています。'/'・'~'・'%'・'X:\' で始まるパスワードはこのチェックを通るので、残され、平文の鍵パス欄に移ります。

**再現シナリオ**: ユーザーがパスワード接続に新しいパスワード '/Tr0ub4dor&3' を入力し、認証方式を「SSH鍵認証」に切り替える。値は残され、render() がそれを見える状態の鍵パス <input> に平文で入れる。そのまま保存（またはテスト）すると、バックエンドはそれを鍵パスとして保存する。鍵パスは書き込み専用ではないので、publicConnections が GetInitialData のたびに UI に返す。ssh-agent が動いていなければ、sshAuthMethods も "read ssh key /Tr0ub4dor&3: open ...: no such file" で失敗する。finishRequest はこのエラーを Stderr に付けるので、command_requests に保存され、get_request_status、get_command_history、/audit/events を通して MCP エージェントに返る。接続テストのメッセージにも同じ文字列が出る。

**修正案**: 認証方式を切り替えたら、形から推測せずに認証情報を消す（または、仕様の項目15のとおり、パスワードと鍵パスを別の欄で持つ）。入力されたパスワードを鍵パス欄に移さない。フェーズ2の編集画面（editor.js）で直す。

#### #36 [minor/secrets-leak] app.go:367 【P2】

**内容**: 空のパスワードは「保存済みのものを使う」という意味ですが、保存済みのパスワードは、それを保存したときのホストと結び付いていません。TestConnectionConfig は、host/port/user が変わっているかもしれない編集中のフォームに保存済みのパスワードを埋め、SaveConnection（app.go:514）も接続先が変わった後もそれを残します。キャッシュした鍵のパスフレーズは接続先の変更で捨てられる（sameTarget、app.go:537）のに、実際にネットワークに送られるパスワードは捨てられません。

**再現シナリオ**: 既存のパスワード接続 prod（ホスト 10.0.0.5）をフォームで開く。ユーザーがホストを新しいアドレスか打ち間違えたアドレス（例: 10.0.0.50）に変え、UI の案内（「設定済み（変更する場合のみ入力）」）どおりパスワードを空のままにして「接続テスト」を押す。TestConnectionConfig が saved.Credential を編集中の接続にコピーし、openSSHClient が ssh.Password(prod のパスワード) を 10.0.0.50 に送る。既定のホスト鍵ポリシーは InsecureIgnoreHostKey なので、そのホストを運用している人や経路上の誰かが、本番のパスワードを平文で受け取る。保存も同じで、何も聞かれずにパスワードが新しい接続先に使い回される。

**修正案**: 保存済みのパスワードを使い回すのは、host・port・user が保存済みの接続と一致するとき（認証情報を除いた sameTarget）だけにする。そうでなければ、鍵のパスフレーズと同じようにパスワードの再入力を求める。

#### #37 [minor/mcp-protocol] approval.go:136 【P3】

**内容**: transitionRequest で store の CAS UPDATE が失敗すると、finishRequest はエラーをログに出すだけです。要求はメモリ上で auto_running/running のまま残り、SSH の結果（stdout、stderr、終了コード）は捨てられます。MCP 層はその止まった記録を、成功した、まだ最終でない結果として報告します。

**再現シナリオ**: 承認省略を持つエージェントが execute_command を呼ぶ。executeCommandAndLog が SSH でコマンドを実行し、finishRequest が updateCommandRequestFrom を呼ぶが、これが失敗する。失敗しうる例: 別のプロセスが書き込みロックを持っていて busy_timeout=5000ms を超える SQLITE_BUSY、ディスクフル、I/O エラー。transitionRequest は古い記録（status auto_running）を返し、a.requests を更新しない。executeCommandTool は、コマンドがもう実行されたのに、stdout も exit_code もない toolResult(requestView(auto_running), isError=false) を返す。それ以降、get_request_status(wait_seconds=60) は最終状態を見ないので、エージェントはいつまでも long-poll を続ける。同じ client_request_id での再試行は、24時間、同じ止まった auto_running の記録に重複排除される。実行中の要求は保護されているので、GUI から削除もできない。出力は再起動まで失われ、再起動すると、実際には完了していても interrupted に変えられる。approve() の経路も同じで、要求は running のまま止まる。

**修正案**: 最終の遷移が保存に失敗しても、最終状態（と結果）はメモリに反映し、notifyRequestsChanged を呼ぶ。または書き込みを再試行する。どちらの場合も isError=true、error_kind=storage を返し、すでに実行されたコマンドについて、エージェントに最終でない状態を見せないようにする。

#### #39 [minor/concurrency] terminal.go:146 【P2】（グループ H）

**内容**: startSSHSession がまだダイヤルしている間に出した切断・削除は、開こうとしているセッションに効きません。startSSHSession は接続を解決したあと、termMu を持たずに最大 ConnectTimeout の時間を openSSHClient/NewSession/Shell に使い、その後で無条件に setTerminalSession を呼びます。closeTerminalsForConnection（DeleteConnection と DisconnectConnection が使う）はその間に a.term を見ても何も見つけずに戻ります。遅れて呼ばれた setTerminalSession が、削除・切断された接続の生きたセッションを登録します。

**再現シナリオ**: ユーザーが接続 X でターミナルを開き（遅いホストへの SSH ダイヤルに数秒かかる）、そのあと X の削除を押す。DeleteConnection は X を SQLite とメモリから消し、closeTerminalsForConnection(X.ID) を呼ぶが、a.term はまだ nil か前のセッションなので何もしない。そのあとダイヤルが完了し、setTerminalSession が削除済みのホストへの対話シェルを登録する。シェルは開いたまま terminal:data を流し続け、terminalCounts/list_hosts は、もう存在しない接続 ID についてターミナルが開いていると報告する。DisconnectConnection でも同じで、ダイヤル中にターミナルのタブを離れた場合も同じ（state.terminal.connected がまだ false なので、teardownTerminal は CloseTerminal をスキップする）。

**修正案**: closeTerminalsForConnection や DeleteConnection が走ったときに、termMu の中で接続ごとの世代か「閉じている」の印を記録する。ダイヤルの後、termMu の中で、接続がまだ存在し、開始してから切断されていないことを確かめ直す。そうでなければ、登録せずに新しいクライアントを閉じる。フェーズ2の terms map と CloseAll/ByConnection で同時に直す。

#### #41 [minor/windows-unix] storage.go:53

**内容**: Windows では、平文の SSH パスワード、鍵のパスフレーズ、MCP トークンが入った SQLite DB を所有者だけに限る保護が、何もしていません。restrictDBFilePermissions とバックアップでの `os.OpenFile(...,0o600)`、`os.Chmod(p,0o600)`、`os.MkdirAll(dir,0o700)` は、Windows では読み取り専用の属性を切り替えるだけで ACL を設定しません。ファイルは親ディレクトリから継承した DACL をそのまま持ちます。

**再現シナリオ**: Windows で、ユーザーが SSH_GETE_CONFIG_DIR=C:\ssh-gete を設定する（appConfigDir はそのまま受け入れる）。例えば、ポータブル版とサービスアカウントで設定を共有するため。C:\ssh-gete は C:\ の下に作られ、その既定の継承 ACL はサブフォルダとファイルについて Authenticated Users に変更権限を与える。そのため ssh-gete.sqlite、-wal/-shm、*.bak-vN は、ローカルのすべてのユーザーが読み書きできる。他のローカルアカウントは、保存された SSH パスワードと MCP の Bearer トークンを読んだり、agent_policies を書き換えて approval_bypass=1 にしたりできる。コードのコメント（'owner-only permissions'）はこれを防いでいると書いているが、この OS では何も起きていない。リネームされた旧 JSON の *.migrated ファイル（app.go:914）も同じ。

**修正案**: Windows では、設定ディレクトリと DB ファイルに明示的な保護 DACL（所有者 + SYSTEM のみ）を設定する（例: golang.org/x/sys/windows の SetNamedSecurityInfo と PROTECTED_DACL_SECURITY_INFORMATION）。または、設定ディレクトリの DACL が他のプリンシパルにアクセスを与えている場合は、起動を拒否するか警告する。

#### #42 [minor/windows-unix] ssh_agent_windows.go:24

**内容**: Windows の agent パイプの判定は、大文字小文字を区別した `\\.\pipe\` で始まる SSH_AUTH_SOCK しか受け付けません。別の有効な Win32 の書き方（`//./pipe/x`、`\\.\PIPE\x`、`\\?\pipe\x`）のパイプパスは、黙って既定の openssh-ssh-agent パイプに置き換えられ、別の agent が使われます。

**再現シナリオ**: ユーザーが既定以外の agent ブリッジ（例: \\.\pipe\my-agent で動く KeePass/gpg/1Password 型の agent）を使っていて、バックスラッシュが崩れる Git Bash から SSH_AUTH_SOCK=//./pipe/my-agent を export する。CreateFile/winio.DialPipe はこのパスを受け付けるが、dialSSHAgent はそれを無視して \\.\pipe\openssh-ssh-agent にダイヤルする。そのサービスが止まっていれば（既定の状態）、agent の認証は、誤解を招く 'read ssh key' やパスフレーズのエラーで黙って失敗する。動いていて別の鍵が読み込まれていれば、ssh-gete はユーザーが設定したものとは違う ID の組を提示する（鍵ファイルがないときは PublicKeysCallback(agentClient.Signers) ですべて）。その結果、意図しない鍵で認証されたり、MaxAuthTries を使い切ったりすることがある。

**修正案**: 判定の前に値を正規化する（'/' を '\\' に置き換え、大文字小文字を区別せずに比べ、`\\.\pipe\` と `\\?\pipe\` の両方を受け付ける）。既定のパイプにフォールバックするのは本当に Unix 形式の値のときだけにし、環境変数の値を無視したときはログに出す。

#### #45 [minor/ci-release] .github/workflows/test.yml:41 【P2】（グループ K）

**内容**: govulncheck は ubuntu-latest で、既定の GOOS=linux で一度しか走りません。ビルドタグ付きの Windows・macOS のファイルからしか到達しないコードの脆弱性は、一度も報告されません。一方で、リリースするのは darwin と windows のバイナリだけです。ssh_agent_windows.go は github.com/Microsoft/go-winio を使い、Windows 版の Wails は go-webview2、go-winloader、go-toast を取り込みます。

**再現シナリオ**: go-winio の名前付きパイプのコード（ssh_agent_windows.go が Windows OpenSSH の agent に届くために使う）か、wails の Windows 専用のフロントエンドパッケージについて GO の脆弱性が公開されたとする。linux の `govulncheck ./...` はそれらのファイルを読み込まないので、CI は緑のままで、脆弱な Windows 版がリリースされる。darwin_frameworks.go と darwin 専用の wails のコードも同じ。

**修正案**: govulncheck を GOOS=windows/darwin/linux のマトリクスで走らせる（例: `GOOS=windows govulncheck ./...`）か、既存の3 OS の go マトリクスの中で走らせる。govulncheck の版を固定する。

#### #47 [minor/approval-cas] storage.go:212

**内容**: v1→v2 の移行は、旧版の「承認待ち」の行をすべて pending に対応付けます。v2 より前の ApproveCommand は、行が「承認待ち」のまま SSH コマンドを実行し、終わってから「実行済み」/「実行失敗」を書いていました。そのため、コマンドが実際に実行中だった行（または実行されたが結果の書き込みに失敗した行）が、interrupted ではなく、承認できる pending 要求として戻ってきます。

**再現シナリオ**: 旧ビルド（a6f2706~1）で、ユーザーが REQ-X（'systemctl restart app' や 'rm -rf /tmp/build'）を承認する。executeSSH の最中にアプリが終了するかクラッシュする、または updateRequestResult が失敗すると、SQLite の行は status='承認待ち' のまま残る。アップグレード後、migrateToV2 がそれを 'pending' に書き換える（legacyStatusCodes）。recoverInterruptedRequests は running/auto_running しか見ないので、この行は捕まらない。REQ-X は再び承認待ちとして表示され、承認するとコマンドが2回目に実行される。これは仕様の56行目が Critical として挙げている「再起動後の二重実行」で、209行目もそのような要求を再承認できてはならないとしている。比較すると、「自動実行中」は interrupted に対応付けられているのに、手動承認の同等の状態はそうなっていない。

**修正案**: migrateToV2 で、すでに実行の痕跡がある旧版の「承認待ち」の行（stdout/stderr が空でない、duration が '-' 以外、executed_command が入っている）を interrupted に移す。または旧版のすべての pending の行にフラグを付け、GUI で明示的な再確認を必須にする。少なくとも、アップグレードより前の pending の行は、承認できるまま残さず、interrupted か expired に対応付ける。

#### #51 [minor/mcp-protocol] mcp_server.go:116 【P3】

**内容**: handleMCP が 202 を返すのは `id` がないときだけです。クライアントからの JSON-RPC レスポンス（`id` と `result` か `error` を持ち、`method` を持たない）には、400 と -32600 "invalid request" のエラーを返します。MCP の Streamable HTTP の仕様では、サーバーはこのようなレスポンスに本文なしの 202 Accepted を返さなければなりません（MUST）。設計の75行目もまさにこの扱いを求めていて、コードのコメントにも「stray responses get no JSON-RPC reply」とありますが、コードはそうなっていません。

**再現シナリオ**: ツリーのスクラッチコピーと、ハンドラ自身の serveTest ハーネスで再現した。Content-Type application/json で、本文 `{"jsonrpc":"2.0","id":1,"result":{}}` を POST /mcp すると、HTTP 400 と `{"jsonrpc":"2.0","id":1,"error":{"code":-32600,"message":"invalid request"}}` が返る。`{"jsonrpc":"2.0","id":2,"error":{...}}` も同じ。レスポンスを POST する準拠したクライアントや SDK（例: サーバーからの要求と解釈したものへのエラー返信や、レスポンスを中継するプロキシ）はプロトコルエラーを受け取る。トランスポートの失敗としてログに出したりセッションを捨てたりするかもしれず、自分が要求として送っていない id についての JSON-RPC メッセージも受け取ることになる。

**修正案**: `result` と `error` を json.RawMessage としてデコードする。`method` が空で `id` があり、`result` か `error` もあるときは、本文をレスポンスとして扱い、本文なしの 202 を返す。400 / -32600 は、要求・通知・レスポンスのどれでもない本文に限る。

#### #54 [minor/ci-release] frontend/package-lock.json:17 【P2】

**内容**: CI は npm の依存を何も検査していません。govulncheck は Go モジュールしか見ず、test.yml の frontend ジョブは `npm ci && npm run build` しか走らせません。そのため lockfile には、high の勧告4件を持つ vite 8.0.16 と source-map-js 1.2.1 が入っていて、CI は緑のままです。

**再現シナリオ**: a6f2706 で `npm audit --package-lock-only` を実行すると、high の勧告が4件報告される。vite 8.0.0-8.1.5（GHSA-rq7h-c2jc-7f22: server.fs.allow の外のファイルが配信されうる、GHSA-9jrq-w75r-8gcw）と、source-map-js <=1.2.1（GHSA-68fv-2mgg-jv7q: イベントループの DoS）。どれも devDependencies なので（`--omit=dev` では0件）、配布するバイナリには影響しない。露出しているのは、`wails dev` が起動する Vite の開発サーバー（`vite --host 127.0.0.1`）と、release.yml のビルドツールです。CI は `npm audit` も npm 向けの Dependabot/OSV も走らせないので、これも今後の脆弱なフロントエンド依存も、何の兆候もなくマージ・リリースされる。govulncheck の「呼ばれている脆弱性0件」は、ツリー全体について誤った安心感を与える。

**修正案**: test.yml の frontend ジョブに `npm audit --audit-level=high`（または package-lock.json に対する osv-scanner）を追加し、`npm audit fix` で vite を >=8.1.6 に上げる。フェーズ2で package.json/package-lock.json（F4）と test.yml（Integrator）を触るときに一緒に直す。

#### #56 [minor/tests-gaps] approval.go:123 【P3】

**内容**: risk_increased のガード（人間の編集で risk が上がったら、例えば Low→High なら、ApproveCommandConfirmed を使わない限り拒否する）をテストするものがありません。`!allowRiskIncrease` を `false` に置き換えても、テストはすべて緑のままです。

**再現シナリオ**: `echo hi`（Low）を積み、`ApproveCommand(id, "rm -rf /")` を呼ぶ。実行せずに Code=risk_increased を返さなければならない。ガードを無効にしたミューテーションでも全テストが通り、編集した High のコマンドがすぐに実行される。ApproveCommandAndBypassAgent も同じガードに頼っているので、承認省略のボタンも risk が上がった編集を実行してしまう。

**修正案**: risk が上がる編集で ApproveCommand が risk_increased を返し、srv.Execs() が空であること、そのあと ApproveCommandConfirmed なら実行されること、ApproveCommandAndBypassAgent も risk_increased を返すことをテストする。

#### #57 [minor/tests-gaps] approval.go:267 【P3】

**内容**: MCP の force_queue フラグを試すテストがありません。needsHumanApproval から `forceQueue ||` を外してもテストはすべて緑のままなので、文書化された逃げ道が、気づかないうちに動かなくなりえます。

**再現シナリオ**: Bearer で認証され、承認省略が保存されたエージェントが execute_command `{command:"echo ok", force_queue:true}` を送る。スキーマはキューに入ると約束している。`forceQueue ||` の項を外すと（ミューテーションで確認済み）自動で実行され（state=auto_succeeded）、どのテストも失敗しない。

**修正案**: TestBypassPolicyLimits に、Low のコマンドを force_queue:true で呼ぶケースを追加し、state=pending で新しい実行がないことを確かめる。

#### #58 [minor/tests-gaps] approval.go:206 【P3】

**内容**: コマンドがタイムアウトなど終了コード以外のエラーになると、finishRequest は cappedBuffer の切り詰めの印の後に execErr の文字列を Stderr に付けます。requestView（mcp_server.go:384）は stderr_truncated を HasSuffix で計算するので、stderr が切り詰められていても false を報告します。タイムアウトのテストは executeSSH が直接返す Stdout しか見ておらず、保存された要求や requestView は見ていません。

**再現シナリオ**: 承認省略の、または承認済みのコマンドが MaxOutputKB を超えて stderr に書き、そのあとタイムアウトを超える。保存される Stderr は `<capped>\n[ssh-gete: output truncated]\ncontext deadline exceeded` になる。get_request_status は stderr_truncated=false、timed_out=true を返し、エージェントは途中までの stderr を完全なものとして扱う。それとは別に、リモートの出力がたまたまこの印の文字列で終わっていると、truncated のフラグが誤って true になる。

**修正案**: truncated のフラグを、末尾から推測せずに commandResult/CommandRequest のフィールドとして持つ。またはエラーを印の前に付ける。stderr を大量に出したうえでタイムアウトさせ、finishRequest と requestView を通してテストする。

## 4. フェーズ2/3 で同時に直すもの

### 4.1 【P2】フェーズ2（マルチタブ・サイドバー・編集画面）と同時に

| 担当（phase2-contract.md §0） | 指摘 | 対応するフェーズ2の作業 |
|---|---|---|
| B1（terminal.go） | #38 | セッションごとの入力 goroutine（送信順の保証）、リサイズはまとめて最新のものだけ |
| B1（terminal.go） | #20, #39 | terms map と CloseAll/ByConnection に、開始中のセッションの世代確認を入れる |
| B2（app.go, approval.go） | #19 | atomic の実行中カウンタ、appCtx/wg、終了の順序 |
| B2（approval.go） | #4, #13, #18 | 実行トラッカーを組み込むときに DeleteCommandRequest を CAS にし、論理削除にする |
| B2（app.go） | #3 | 起動処理の単一インスタンスロック（storage.go の回収には触らず、app.go 側でロックを取る） |
| B2（app.go） | #36 | 接続先が変わったら保存済みパスワードを使い回さない |
| F3（editor.js）/ Integrator（main.js） | #35 | 編集画面で、認証方式を切り替えたら認証情報を消す |
| Integrator（main.js） | #22, #40, #53 | draft と editor.selectedId を id にし、dataGen を入れるときに、承認ボタンに表示中の要求 id を持たせる |
| F4 / Integrator（package*.json, test.yml） | #26, #45, #54 | test.yml の frontend ジョブに npm audit を足し、govulncheck を GOOS のマトリクスにする。vite を上げる |

### 4.2 【P3】フェーズ3（MCP の充実と認証の強化）と同時に

| 領域 | 指摘 |
|---|---|
| client_request_id（受け入れ条件「同じ client_request_id を再送しても実行は1回だけ」） | #15, #17, #30, #16 |
| 承認省略・Bearer（受け入れ条件「共有トークンだけでは承認省略を名乗れない」） | #32, #52, #14, #21, #1 |
| risk 分類・承認ポリシー | #5, #28, #48, #49, #6, #29, #46, #55, #56, #57 |
| 承認画面で解決済みの実行先と fp の変化を出す | #2, #11, #31（名前フォールバックの廃止）、#22, #40, #53 の仕上げ |
| TOFU・ホスト鍵（ssh_exec.go を触る） | #8, #7, #33, #9 |
| 出力の統一・long-poll・JSON-RPC | #37, #58, #51 |

### 4.3 印なし（フェーズを待たずに直す）

- 永続化・移行: #10, #34, #50, #12, #47（フェーズ1の「旧 JSON の一度きり移行」「user_version v2」の取りこぼし。既存の利用者が更新するときに認証情報が消える・漏れる可能性があるので、次のリリースより前に直す）
- OS 依存: #23, #41, #42
- リリース経路: #24, #44, #25, #43, #27（次にタグを push する前に直す）
