# REQ-017 シグナル送信ボタン: つなぎ込み手順

ブランチ `feat/v1-terminal-signals`（beb3128 から分岐）は新しいファイルだけで実装してある。既存ファイルは本体側で別のチームが書き換え中なので、つなぎ込みはマージの後に本体側で行う。この文書はその手順書。

## 新しいファイル

| ファイル | 中身 |
|---|---|
| `terminal_signal.go` | `App.SendTerminalSignal(id, sig) ActionResult`、`App.TerminalSignalNames()`、`registerTerminalSignaler(ts, sender)` |
| `terminal_signal_test.go` | 制御文字の順序、SSH signal（4種）、拒否と無応答、`signal_busy`、BREAK、Close との競合 |
| `frontend/src/signals.js` | `renderSignalMenu`、`createSignalSender`、`confirmSignalDialog`、`signalResultMessage`、`signalResultToast`、`normalizeSignalResult`、`signalItems`、`SIGNALS_I18N` |
| `frontend/src/signals.test.js` | vitest（jsdom）65件 |

`sig` に渡せる名前（バックエンドとフロントエンドで同じ名前を使う）:

| 種類 | 名前 | 送り方 |
|---|---|---|
| SSH / シリアル | `INT_CHAR` `TSTP_CHAR` `QUIT_CHAR` `EOF_CHAR` | 入力キュー `ts.input` に制御文字（0x03 / 0x1a / 0x1c / 0x04）を入れる。順番が保たれるのは `ts.input` に届いた入力との間だけ。ツールバーからはこの名前を使わず、`createSignalSender` が `xterm.input` で送る（5.3） |
| SSH | `TERM` `KILL` `HUP` `INT` | RFC 4254 §6.9 の `signal` リクエストを want_reply=true で送る。拒否と3秒の無応答はどちらも `signal_unsupported` |
| シリアル | `BREAK` | `writeMu` を持ち、出力の排出（`drainWithTimeout`、最大 `serialDrainTimeout`）を待ってから `serial.Port.Break(250ms)` を呼ぶ |

メニューは SSH タブに制御文字4つと SSH signal 4つ、シリアルタブに BREAK だけを出す。バックエンドはシリアルへの制御文字も受け付けるが、メニューには出していない。

失敗コード: `invalid_signal` `no_session` `signal_not_applicable` `not_ready` `signal_unavailable` `signal_unsupported` `signal_busy` `signal_failed` `break_unsupported` `break_failed` `input_timeout`。どれも `SIGNALS_I18N` に JA/EN の文言がある。成功は `{ ok: true, code: '' }`。

## 1. terminal.go（必須、1行）

`openSSHTerminal` で、stdin を設定した直後（`commitTerminal` の前）に登録する。これが無いと SSH signal はすべて `signal_unavailable` になる。制御文字と BREAK はこの行が無くても動く。

```diff
 	ts.stdinWriter = stdin
+	registerTerminalSignaler(ts, session)
 	nowCols, nowRows, ok := a.commitTerminal(ts,
```

`session` は `*ssh.Session`。登録はタブが閉じる（`ts.done` が閉じる）と自動で消える。`commitTerminal` が失敗した場合も `done` が閉じるので、後始末は要らない。

テストとの関係: `terminal_signal_test.go` の `TestSignalThroughStartTerminal` は、この行が無い間は `t.Skip` する（`StartTerminal` を通したタブが登録されていないため）。この行を入れると、`StartTerminal` → `openSSHTerminal` を通した端から端までのテスト（制御文字と TERM / KILL / HUP / INT）として動く。マージ後に `go test -run TestSignalThroughStartTerminal -v .` が **SKIP ではなく PASS** になることを確かめる。SKIP のままなら、この行が入っていない。`TestSignalErrors` の `signal_unavailable` の確認は登録を手で外してから行うので、この行の有無に関係なく通る。本体の作業ツリーの写しにこの行を入れて、`go test -race ./...` が通ることを確かめてある。

前提にしているフィールドは次のとおり。本体側の書き換えで名前が変わったら `terminal_signal.go` を合わせる。

- `terminalSession`: `id`、`kind`、`status`、`reason`、`input`、`done`、`serial`
- `serialHandle`: `port`、`writeMu`
- 定数: `terminalStatusOpen`、`terminalInputTimeout`
- テスト: `newTerminalTestApp`、`mustStart`、`mustSend`、`waitUntil`（terminal_test.go）、`newTestHostKey`、`sendExitStatus`、`sshTestUser`、`sshTestPassword`（sshtest_test.go）

`*ssh.Session` と `serial.Port` が必要なインターフェースを満たすことは、`terminal_signal.go` の中でコンパイル時に確かめている。

任意: `serial_mode.go` の `serialPort` インターフェースに `Break(time.Duration) error` を足すと、型アサーションが要らなくなる。足す場合は、テスト用の偽ポートにも `Break` が必要になる。

## 2. Wails バインディング

```sh
wails generate module
```

`frontend/wailsjs/go/main/App.{js,d.ts}` に `SendTerminalSignal` と `TerminalSignalNames` が出る（生成物は gitignore 対象）。このブランチで `wails generate module` を実行して、`App.d.ts` に両方が出ることを確かめてある。

## 3. i18n.js

```diff
 import { CREDENTIALS_I18N } from './credentials.js';
+import { SIGNALS_I18N } from './signals.js';
 ...
   SHORTCUTS_I18N,
   CREDENTIALS_I18N,
+  SIGNALS_I18N,
 );
```

キーはすべて `sig.` で始まり、既存の辞書とは重ならない。`mergeI18n` は重なりや JA/EN の欠けがあると例外を投げるので、壊れていれば shell.test.js で分かる。

## 4. 確認ダイアログ

KILL と TERM は確認してから送る。`confirm` が無いと送らない（`cancelled`）。

`signals.js` の `confirmSignalDialog` を `confirm: (o) => confirmSignalDialog({ ...o, t })` として渡す（5.3）。editor.js の変更は要らない。

editor.js の `confirmModal` は使わない。`showModal` は最初のフォーカスを OK（`[data-modal-value]`、ここでは「KILL を送る」）に置くので、メニューで KILL を Enter で選んだ後、Enter の押しっぱなしや二度押しでそのまま KILL が送られる。`confirmSignalDialog` は次のようにしてある。

- 最初のフォーカスは「キャンセル」
- 押しっぱなし（`event.repeat`）の Enter / Space ではボタンが反応しない
- キーボードで起きたクリック（`detail === 0`）は、ダイアログの中で Enter / Space を新しく押した後にだけ受け付ける（メニューを Space で選んだときの keyup がボタンに当たっても何も起きない）
- Esc と背景のクリックはキャンセル。Tab はダイアログの中で回る
- 文言はすべて `textContent`。置き場所は `#editor-modal-root`（まだ無ければ editor.js の `modalRoot()` と同じように `body` の下に作る）。main.js の `MODAL_ROOTS` に入っている要素なので、ダイアログが出ている間は `modalOpen()` が true になり、Cmd+W / Cmd+T / Cmd+1..9 などのターミナルのショートカットは効かない。さらにダイアログ内の keydown はすべて `stopPropagation` するので、document のショートカット処理には届かない。Bootstrap の `modal` / `modal-backdrop` のクラスを使うので、見た目は既存のダイアログと同じ

editor.js の `modalChain` には入らない。ほかのダイアログと同時に出ることは、メニューが開いた状態でしか起きず、メニューは外にフォーカスが移ると閉じる（5.4）ので、実際にはほぼ無い。

## 5. main.js: ツールバーへの配置

Phase 4 の画面では、ターミナルのツールバーは terminal-tabs.js の `renderTerminalPage` ではなく main.js の `renderTerminalScreen` が描く。メニューは `.term-active-actions` の中、「閉じる」ボタンの前に置く。

### 5.1 import

```diff
 import {
   ...
+  SendTerminalSignal,
 } from '../wailsjs/go/main/App';
+import { confirmSignalDialog, createSignalSender, renderSignalMenu, signalResultToast } from './signals.js';
```

### 5.2 入れ物を置く（renderTerminalScreen）

```diff
         ${stale ? `<button class="btn btn-sm btn-warning" ...` : ''}
+        <div class="term-signal-slot"></div>
         <button class="btn btn-sm btn-outline-secondary" type="button" data-action="term-close" ...
```

終了したタブ（`exited`）にも入れ物は置いてよい。メニューは `disabled` で押せなくなる。

### 5.3 メニューを1つだけ作り、描き直しのたびに移す

`#page-root` はポーリングのたびに `innerHTML` で描き直される。毎回作り直すと開いたメニューが閉じるので、メニューは1つだけ作り、描き直した入れ物へ `mount()` で移す。

```js
let signalMenu = null;

function signalMenuOptions() {
  const entry = getTerminalState().tabs.get(currentView().activeTermId);
  return {
    kind: entry?.kind === 'serial' ? 'serial' : 'ssh',
    target: entry?.id ?? '',
    disabled: !entry || entry.status !== 'open',
    t,
    lang: state.lang,
  };
}

function mountSignalMenu(root) {
  const slot = root.querySelector('.term-signal-slot');
  if (!slot) return;
  if (!signalMenu) {
    signalMenu = renderSignalMenu(slot, {
      ...signalMenuOptions(),
      // 制御文字は xterm.input -> onData -> queueInput の順に流す（必須）。
      // SSH signal と BREAK は SendTerminalSignal。
      onSend: createSignalSender({
        getEntry: (id) => getTerminalState().tabs.get(id),
        sendSignal: (id, wire) => SendTerminalSignal(id, wire),
      }),
      confirm: (o) => confirmSignalDialog({ ...o, t }),
      focusTerminal: () => {
        const entry = getTerminalState().tabs.get(currentView().activeTermId);
        if (entry && entry.status !== 'exited') entry.xterm?.focus();
      },
      // 失敗は warning。SSH signal の成功も知らせる（対話シェルは TERM / INT を
      // 無視するので、知らせないと何も起きなかったように見える）。
      onResult: (result, item) => {
        const toast = signalResultToast(result, item, t);
        if (toast) showToast(toast.text, { variant: toast.variant });
      },
    });
  } else {
    signalMenu.mount(slot);
    signalMenu.update(signalMenuOptions());
  }
}
```

`renderPage` の terminal 分岐で、描いた直後に呼ぶ。

```diff
-  else if (state.active === 'terminal') root.innerHTML = renderTerminalScreen(view);
+  else if (state.active === 'terminal') {
+    root.innerHTML = renderTerminalScreen(view);
+    mountSignalMenu(root);
+  }
```

注意:

- `status` の値は `'starting' | 'open' | 'exited'`（terminal-tabs.js）。送れるのは `'open'` のときだけにする。
- `onSend` は `createSignalSender` で作る。`onSend: ({ target, wire }) => SendTerminalSignal(target, wire)` のように制御文字までバックエンドへ直接送ると、フロントエンドの `pendingInput` / `sendQueue` に残っている打鍵より先に届く。長い貼り付けの途中に Ctrl-D が入り、heredoc や `cat` がそこで終わって、残りがシェルのコマンドとして実行される。`createSignalSender` は制御文字を `entry.xterm.input(data, true)` で入れるので、xterm の `onData` から既存の `queueInput` に入り、打った文字の後ろに並ぶ（xterm 6 の API。terminal-tabs.js の変更は要らない）。
- `onSend` には、メニューが受け取った `target`（選んだ時点のアクティブなタブの id）を使う。`terminal.activeId` を送る時点で読み直さない。KILL の確認中にタブを切り替えた場合、メニューは送らずに `cancelled` を返す。種類（ssh/serial）が変わった場合も同じ。
- 言語の切り替えは `lang` で伝える。`t` は同じ関数のままなので、`signalMenuOptions()` に `lang: state.lang` を入れておけば、`update` のたびに言語の変化を見て項目の文言を描き直す。
- 本体側のメッセージ（`result.message`）は日本語だけなので、EN では `signalResultMessage` の文言を出す。JA で本体のメッセージを出したい場合は、既存の `startErrorMessage` と同じく `state.lang === 'en'` で分ける。

### 5.4 ポーリングでメニューを壊さない（pageIsBusy）

メニューがキーボードのフォーカスを持っている間、確認ダイアログの間、送信の応答待ちの間は描き直さない。マウスで開いただけのメニュー（フォーカスは xterm のまま）は `isBusy()` に数えない。描き直しても `mount()` で同じメニューが移るので開いたまま残り、セッションが終わればツールバーは終了の表示に変わる（`update({ disabled: true })` でメニューも閉じる）。

開いている間は、メニューの外でキーを押すか、フォーカスが外へ移ると閉じる。Esc はそこで使い切り、シェルには送らない（document の capture で xterm より先に受ける）。ほかのキーは閉じたうえでそのまま xterm に届く。非同期のダイアログ（承認、パスフレーズなど）がフォーカスを取った時点でメニューは閉じる。

```diff
 function pageIsBusy() {
   if (state.active === 'connections') return editorHasFocus();
   if (state.active === 'settings') return state.dirty.settings || agentsSectionBusy();
   if (state.active === 'commands') return state.dirty.approval;
+  if (state.active === 'terminal') return !!signalMenu?.isBusy();
   return false;
 }
```

描き直しを飛ばしても、`#shell-subtabs` は `renderServerTabsBar` で更新され続ける。ユーザーがタブを選んだときは `selectTerminalTab` が `renderPage` を呼ぶ。その前に `signalMenu.close()` を呼ぶ必要はない。メニューが開いてフォーカスを持ったままショートカット（Cmd+Shift+] など）でタブが変わっても、`mount()` はフォーカスしていた項目にフォーカスを戻し、`update()` で種類が変わったときは先頭の項目にフォーカスする。

### 5.5 ショートカットとの関係

メニューはグローバルなショートカットを持たない。メニュー内のキー（矢印、Home/End、Enter/Space、Esc）は `stopPropagation` するので、main.js の document の keydown（shortcuts.js）には届かない。Tab はメニューを閉じて、そのまま伝える。

ボタンと項目の mousedown は `preventDefault` するので、押しても xterm からフォーカスが移らない。送った後とキャンセルした後は `focusTerminal()` を呼ぶ。xterm にフォーカスが無くても送れる（キーボードではボタンにフォーカスして ArrowDown / Enter）。

## 6. terminal-tabs.js

Phase 4 の画面では変更は要らない。Phase 2 の `renderTerminalPage`（`.term-toolbar` の `.term-tabs` と `.term-new`）を使い続ける画面があれば、`.term-tabs` の後に同じ入れ物 `<div class="term-signal-slot"></div>` を置き、5.3 と同じ `mountSignalMenu` を呼ぶ。`kind` は `entry.kind`、`disabled` は `entry.status !== 'open'`、`focusTerminal` は `() => terminal.tabs.get(terminal.activeId)?.xterm?.focus()`。

制御文字は `createSignalSender` が `entry.xterm.input(data, true)` で入れるので、既存の `queueInput` / `flushInput` をそのまま通り、打った文字や貼り付けの残り（`pendingInput`）の後ろに並ぶ。terminal-tabs.js の変更は要らない。`xterm.input` が無い場合（xterm 5.4 より前）だけ `SendTerminalSignal` に戻り、そのときは `pendingInput` より先に届くことがある。

## 7. style.css

メニューは Bootstrap の dropdown のクラス（`dropdown`、`dropdown-menu`、`dropdown-item`、`dropdown-header`、`dropdown-divider`）を付けているが、Bootstrap の JS は使っていない。表示は `hidden` 属性と `.show` で切り替える。次を足す。

```css
.term-signal {
  position: relative;
  display: inline-block;
}

.term-signal-menu {
  position: absolute;
  top: 100%;
  left: 0;
  z-index: 1000; /* xterm より上、Bootstrap の modal（1055）と backdrop（1050）より下 */
  min-width: 14rem;
  margin-top: .125rem;
}

.term-signal-menu[hidden] {
  display: none !important;
}

.term-signal-menu.show {
  display: block;
}

.term-signal-item {
  display: flex;
  align-items: center;
  gap: .5rem;
}

.term-signal-key {
  min-width: 4.25rem;
  font-size: .78rem;
  text-align: center;
}

.term-signal-label {
  margin-left: .25rem;
}

@media (max-width: 576px) {
  .term-signal-label {
    display: none; /* アイコンだけにする。title と aria で名前は残る */
  }
}
```

`.term-active-actions` が右端に寄る画面で、メニューがはみ出す場合は `.term-signal-menu { left: auto; right: 0; }` にする。

## 8. 確かめること（REQ-017 の受け入れ条件）

自動テストで確かめてあること:

- 制御文字4つがアクティブなタブのセッションに、打った文字と同じ順番で届く（`StartTerminal` を通したテストサーバー）
- つなぎ込みの後は、`StartTerminal` を通したタブで制御文字と SSH signal 4つが届く（`TestSignalThroughStartTerminal`。つなぎ込み前は SKIP）
- TERM / KILL / HUP / INT が `signal` リクエスト（want_reply=true）として届く
- サーバーの拒否と無応答が `signal_unsupported` になり、画面に JA/EN の文言が出る
- シリアルの BREAK が `writeMu` を持った状態で、出力の排出を待ってから `Break` を呼ぶ（排出が終わらないポートでも止まらない）
- 制御文字は `xterm.input` を通り、貼り付けの途中に割り込まない
- KILL / TERM は確認してから送り、確認中にタブや種類が変わったら送らない。確認ダイアログの最初のフォーカスはキャンセルで、押しっぱなしの Enter では送らない
- マウスで開いたメニューは xterm での Esc（シェルに送らない）やキー入力、外へのフォーカス移動で閉じ、描き直しを止めない。描き直しでキーボードのフォーカスを失わない
- 項目の文言が JA/EN それぞれで重ならず、実行中のコマンド向けかシェル向けかが分かる

つなぎ込みの後に手で確かめること:

- 実際の OpenSSH で TERM / KILL / HUP / INT が届くか。OpenSSH の sshd は 8.1 から `signal` リクエストに対応し、7.x 以前は拒否する（拒否なら `signal_unsupported` が出ることを確かめる）。3秒の待ち時間（`terminalSignalTimeout`）は仮の値
- 実機のシリアルで BREAK が出るか（例: Cisco の ROMMON に入る）
- ポーリング中もメニューが閉じないこと、xterm のフォーカスが戻ること
- JA/EN の切り替え

## 9. 既知の制限

- SSH の `signal` リクエストは、sshd がセッションの子プロセス（ログインシェル、またはコマンド）に送る。シェルの中で動いている前面のプログラムには届かない。実行中のコマンドを止めるのは制御文字（Ctrl-C など。端末のドライバーが前面のプロセスグループに送る）で、TERM / KILL / HUP はシェルごと終わらせる用途になる。メニューの区切りと確認文はこの違いを前提にしている。OpenSSH は forced command が設定されたセッションや subsystem では `signal` を受け付けない。

- BREAK は入力キューを通らない。ポートに書き終わった出力は排出を待つが、フロントエンドの `pendingInput` やバックエンドの `ts.input` に残っている打鍵より先に出ることがある。
- 応答を返さないサーバーでは、送信の goroutine と `busy` がセッションが閉じるまで残る。その間の SSH signal は `signal_busy` で断る。制御文字は止まらない。
- `TerminalSignalNames` はフロントエンドでは使っていない（メニューの項目は `signals.js` が持つ）。不要なら外してよい。

## 10. 本体へのマージと結線の記録

`fix/v1-safety-multisession` へのマージで、上の手順どおりに結線した。手順と違うところ、足したところは次のとおり。

- `terminal_signal.go` の `sendSerialBreak`: 本体の `drainWithTimeout` は `serialHandle` のメソッド（`h.drainWithTimeout(d)`）になり、fd を使う操作はすべて `acquire` / `release` で使用中の印を持つ規則（N1、最終監査 #5）になっていた。BREAK もその規則に合わせ、`closePort` の後は `Break` を呼ばずに `no_session` を返す。テスト `TestSignalSerialBreakAfterClosePort` を足した。
- `terminal.go`: `registerTerminalSignaler(ts, session)` を `ts.stdinWriter = stdin` の直後に入れた。`TestSignalThroughStartTerminal` は SKIP ではなく PASS になる。
- headless の Web API（`webapi.go`）: `SendTerminalSignal` を許可リストに入れ、呼び出しごとに監査する（target はタブの id、detail は `sig=<名前>`）。`TerminalSignalNames` は監査しない読み取り。テスト `TestWebCallTerminalSignal` を足した。許可リストは48個（phase6-headless.md §3.3）。
- main.js: `pageIsBusy` の terminal 分岐は、本体の「ツールバーにフォーカスがあれば描き直さない」に `signalMenu.isBusy()` を足した（どちらかが true なら描き直さない）。アクティブなタブが無くなって入れ物が描かれないときは、メニューを `disabled` にして閉じる。
- ショートカット: メニューにはグローバルなショートカットを付けていない。メニューが使うキー（矢印、Home/End、Enter/Space、Esc、Tab）と、メニューが送る制御文字（Ctrl-C / Ctrl-Z / Ctrl-\ / Ctrl-D）が、GUI 版（mac / other）とブラウザ版（browser、Alt+Shift）のどのショートカットにも当たらないことを `main.signals.test.js` で確かめている。KILL の確認中は `modalOpen()` でターミナルのショートカットが効かないことも同じテストで確かめている。
- terminal-tabs.js: Phase 2 の `renderTerminalPage` は本体の画面ではもう使っていない（テストだけ）ので、§6 のとおり変更していない。
