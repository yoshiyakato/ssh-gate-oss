# マージ監査報告書: 52d876a

対象: `52d876a`（Merge feat/v1-phase3: MCP 充実・TOFU・エージェント専用トークンをフェーズ2の実行トラッカーに統合）
比較基準: HEAD^1（フェーズ2）、HEAD^2（フェーズ3）、仕様 `docs/spec/design-v1.md`

## 概要

このマージで、フェーズ3の MCP 実行経路をフェーズ2の実行トラッカー（`beginExec` / `finishExec` / `cancelExecs`）に載せ替えた。監査で確定した指摘は2件で、どちらも minor。critical と major はない。反証された指摘もない。

- シャットダウン順序がフェーズ2の「即キャンセル」に揃ったため、フェーズ3と仕様 §5.0 手順2 にあった「猶予内に終わるコマンドは完了させる」動作が本番経路から消えた。
- `approve()` が状態遷移を確かめる前に `beginExec` を呼ぶので、承認が失敗したときに実行中件数が一瞬多く数えられ、余分な `sessions:changed` イベントが出る。

| 重要度 | 件数 |
|---|---|
| critical | 0 |
| major | 0 |
| minor | 2 |

## 確定指摘（重要度順）

| # | 重要度 | 分類 | 場所 | 要約 |
|---|---|---|---|---|
| 1 | minor | exec-tracking | `app.go:531` | シャットダウン時に全実行を先にキャンセルするので、drain の猶予待ちが機能しない（仕様 §5.0 手順2 から逸脱） |
| 2 | minor | mcp-terminal | `approval.go:334` | pending を確かめる前に `beginExec` を呼ぶため、承認失敗時に実行数が一瞬過大になり、余分なイベントが出る |

---

### 指摘1 [minor/exec-tracking] シャットダウン時の猶予待ちが失われた

**場所:** `app.go:531`（`App.shutdown`、`app.go:529-538`）

**内容:**
マージ後の `shutdown` は、`a.appCancel()` に続いて `a.cancelExecs(all, errShuttingDown)` を呼び、追跡中のすべての実行をキャンセルする。`stopMCPServer` と `drainMCPCommands(shutdownExecGrace)` はその後に呼ばれる。そのため `drainMCPCommands` が始まった時点で、キャンセルされずに動いている実行はもう残っていない。

- drain の最初の待機（猶予まで待ってからキャンセル）は、キャンセル済みの実行が終わるのを待つだけになる。
- 猶予の後の `cancelExecs`（`app.go:568`）は何もしない。
- `drainMCPCommands` を呼ぶのは `shutdown` とテストだけなので、「待ってからキャンセルする」動作は本番では無くなっている。最初の猶予が効くのは、直接呼ぶテスト（`TestShutdownDrainsGUIApprovedRun`）の中だけである。

**比較:**
- HEAD^2（フェーズ3）: `stopMCPServer` → `drainMCPCommands(shutdownDrainTimeout)` → `CloseTerminal` → `appCancel` の順。実行中のコマンドは猶予内なら完了でき、終わらなかったものだけキャンセルされる。仕様 §5.0 手順2（「実行中コマンドを最大 N 秒待ち、終わらないものはキャンセルして interrupted を永続化」）どおりの動作。
- HEAD^1（フェーズ2）: `appCancel` → `cancelExecs` を先に行う。マージ後の順序はこれと同じ。

したがって両ブランチに共通する欠陥ではない。マージの際に、仕様どおりだったフェーズ3の順序ではなくフェーズ2の順序が採られた。

**シナリオ:**
承認バイパス、または GUI で承認されたコマンドが動いているときにユーザーがアプリを終了する。コマンドは約1秒で終わるもの（`sleep 1; echo ok` のようなもの、または完了間近のもの）とする。
- HEAD^2: `shutdownDrainTimeout`（5秒）の間にコマンドが完了し、出力つきで `auto_succeeded` / `succeeded` として記録される。
- マージ後: `appCtx` がすぐにキャンセルされ、`runSSHCommand` が SIGKILL を送る。`finishRequest` は `interrupted`（`error_kind` = canceled）を記録し、エージェントまたは GUI は結果を受け取れない。

**反証者の所見:**
事実関係はすべて確認された。ただし元の指摘にあった「黙って（silently）」という表現は正しくない。マージコミットのメッセージにはシャットダウン順序が書かれている。コードコメントにも、ウィンドウを猶予の間ずっと開いたままにしないために「§5.0 手順2 を文字どおりに」先に待つ方式は採らなかった、と明記されている。つまり意図して選ばれた順序である。それでも、5秒以内に終わるはずのコマンドが kill されて interrupted と記録され、フェーズ3と仕様の動作が失われるという結果は指摘のとおりである。

**修正案（どちらか）:**
1. `shutdown` の先頭にある `appCancel` / `cancelExecs` を外し、処理を `drainMCPCommands` に任せる。`drainMCPCommands` は `execsClosed` と `mcpRuntime.close` で受付を閉じ、猶予まで待ち、`errShuttingDown` でキャンセルして、もう一度待つ。その後に `appCancel` を呼ぶ。`execsClosed` が立った後は `beginExec` が新しい実行を受け付けないので、新規受付は止まったままになる。
2. 即キャンセルが意図した設計なら、ssh-gete は意図して即時キャンセルすることを `design-v1.md` §5.0 に書き、仕様をそれに合わせる。

---

### 指摘2 [minor/mcp-terminal] 承認の状態遷移より前に実行がカウントされる

**場所:** `approval.go:334`（`approve()`）、`app.go:715`（`beginExec`）

**内容:**
`approve()` は `a.transitionRequest(id, []string{statePending}, ...)` でリクエストがまだ pending かを確かめるより前に、`a.beginExec(a.appCtx, conn.ID)` を呼ぶ。`beginExec` は次のことを行う。
- 実行を `a.execs` に登録する（`runningCounts` が数える対象）
- `runningTotal` を1増やす
- `sessionsChanged()` を呼ぶ

`transitionRequest` が失敗すると、defer された `finishExec` がこれらを元に戻し、もう一度 `sessionsChanged` を出す。そのため遷移に失敗した承認（`errStateConflict` / `not_pending`、または `updateCommandRequestFrom` のストレージエラー）は一瞬だけ実行中として数えられ、余分な `sessions:changed` が2回出る。

HEAD^1 では `trackExec` を `executeSSH` の中（`approval.go:346`、running への遷移の後でしか到達しない）でしか呼んでいなかったので、これはマージで新しく入った挙動である。

`!connOK` の経路では `conn` がゼロ値なので、実行は connID `""` で登録され、`runningTotal` にも数えられる。マージ前、この経路は追跡されていなかった。

**シナリオ（反証者の訂正を反映）:**
元の指摘は「ダブルクリック」で再現すると書いていたが、その順序では起きない。`approve()` は最初に `findRequest` を行い、状態が pending でなければ `approval.go:294` で `not_pending` を返すからである。1回目の呼び出しが R を running に移した後に来た2回目の呼び出しは、`beginExec` まで届かない。

実際に起きる条件は次の2つ。
- 2つの承認呼び出し（2つのウィンドウから、またはほぼ同時）がどちらも294行のチェックを通り、まだどちらも `transitionRequest`（`a.mu` を取る）を終えていない場合。後から来た方は `beginExec` で `runningTotal` と、接続 C の `SessionSummary.Running` を2にし、`running=2` の `sessions:changed` を出す。その後 `errStateConflict` で失敗し、`finishExec` が1に戻して、もう一度イベントを出す。
- 遷移中にストレージエラーが起きた場合。こちらは競合がなくても起きる。

この間、サイドバーには一瞬「実行2」と表示される。`activeWork().Running`（`OnBeforeClose` の終了確認）も実際より多く数える。

**影響:**
同じ呼び出しの中で元に戻るので、状態がずれたまま残ることはない。起きるのは短時間の件数の誤りと余分なイベントだけで、重要度は minor。

**反証者の所見:**
反証はできなかった。中心となる主張はコードと一致する。訂正はシナリオの起き方（上に書いた294行のチェック）だけである。

**修正案:**
シャットダウン処理には `beginExec` を先に呼ぶ順序が必要なので、その順序は残す。そのうえで、カウンタの加算と `sessionsChanged` を running への遷移が成功した後に回す。
1. `beginExec` を2段に分ける。reserve（`execMu` の下で `execs` / `execWG` に登録）と、`transitionRequest` が成功した後の commit（`runningTotal` の加算と `sessionsChanged`）。
2. もっと簡単な方法として、`beginExec` の前に `a.mu` の下で `req.Status == statePending` を確かめ直し、残る小さな競合は許容する。

あわせて、`!connOK` の経路では `beginExec` を呼ばない（または connID `""` を数えない）ようにすることが望ましい。

---

## 反証された指摘

なし。
