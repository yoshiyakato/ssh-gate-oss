# フェーズ4 UI 統合設計（サーバータブ・serverFilter・ショートカット。シリアル以外）

design-v1.md §6.4 と §7 フェーズ4 のうち、シリアルを除く frontend の統合部分を、フェーズ2の契約（phase2-contract.md）の名前で決め切ったものです。シリアルの仕上げは phase4-serial-patch.md が扱います。

- 前提: フェーズ2（phase2-contract.md）が本体にマージ済みで、`main.js` / `shell.js` / `terminal-tabs.js`（F1）/ `sidebar.js`（F2）/ `editor.js`（F3）/ `model.js`（F4）が契約どおりに存在すること。契約と実装が食い違っていたら、まず契約側の名前に合わせて読み替え、§8 に記録する。
- 部品:
  - `frontend/src/servertabs.js`（別チーム、作成済み）: 上部タブとシェルのサブタブの view model と DOM 差分描画。
  - `frontend/src/shortcuts.js`（別チーム、作成済み）: キー判定と xterm 用ハンドラ。
  - `frontend/src/serverfilter.js`（フェーズ3の別チーム、未作成）: `filterByServer` ほか。§1.3 の名前を仮定する。
- 本書で新しく作るファイル: `frontend/src/serverview.js`（main.js から切り出す純関数。§5 のテストのため）と、その `serverview.test.js`。
- 用語: 「接続先」は保存済みの Connection。「不明な接続先」は、レコードの `connectionId` が空か、現在の `state.data.connections` に無いこと。

---

## 0. 全体の形

```
#app（起動時に1回だけ描く。shell.js の renderSkeleton）
├ aside.app-sidebar … #sidebar-status          ← F2。絞り込まない（§2.5）
└ main.app-main
   ├ .app-content-header（タイトル）
   ├ #server-tabs        ← 新規。renderServerTabs で差分描画。全画面で常に出す
   ├ #shell-subtabs      ← 新規。renderShellSubtabs で差分描画。ターミナル画面のときだけ表示
   ├ #page-root          ← 画面ごとに innerHTML を描き直す（フェーズ2どおり）
   └ #terminal-dock      ← F1。描き直さない
```

`#server-tabs` と `#shell-subtabs` を `#page-root` の外に置くのは、5秒ごとのポーリングで `renderPage()` が `#page-root` を描き直しても、タブ列の DOM（とキーボードのフォーカス）を保つためです。servertabs.js の `renderServerTabs` / `renderShellSubtabs` は data-key による差分描画なので、同じコンテナに何度呼んでもノードは使い回されます。

データの流れは一方向にします。

```
state.serverFilter ──(読む)──> deriveServerView(...)（serverview.js、純関数）
                                 ├ tabs        → renderServerTabs(#server-tabs)
                                 ├ queue / history / selectedRequest → renderCommands
                                 ├ subtabs / stackVisible → renderShellSubtabs(#shell-subtabs), setTerminalPageVisible
                                 └ hiddenPending → 承認キューの注記
setServerFilter(connId, opts) ← 上部タブ / ショートカット / 一覧の選択 / サイドバーの行
```

`state.serverFilter` を書き換えるのは main.js の `setServerFilter` だけです。各モジュール（F1〜F3）は読みも書きもしません（契約 §2.1 の「モジュールは state の自分の担当分しか書かない」を守るため）。

---

## 1. state.serverFilter の置き場所・保存・復元

### 1.1 state への追加（契約 §2.2 への追記）

```js
const state = {
  // … フェーズ2の項目はそのまま …
  serverFilter: '',          // '' = すべて（servertabs.js の ALL_SERVERS）。それ以外は Connection.id（connId）
  serverFilterReady: false,  // 最初の refreshData 成功までは false。false の間は正規化も保存もしない
};
```

- 値は **connId だけ**を持つ。名前・host・index では持たない（リネームで外れないため）。
- 「すべて」は空文字 `''`。servertabs.js の `ALL_SERVERS`、shortcuts.js の `cycleServer` の `''` と同じ表現にする。serverfilter.js も `''` を「すべて」とみなす前提（§1.3）。
- ctx に1つ足す（契約 §2.2 への追記）:
  ```js
  ctx.serverFilter = () => state.serverFilter;          // 読み取り専用。モジュールが必要になったとき用（フェーズ4では未使用）
  ctx.setServerFilter = (connId, opts) => setServerFilter(connId, opts);
  ```

### 1.2 保存と復元

`localStorage` に、言語（`sshgate-lang`）と同じ流儀で保存します。設定ファイル（SQLite）には入れません（端末ごとの表示状態であり、MCP からも参照しないため）。

| 項目 | 値 |
|---|---|
| キー | `sshgate-server-filter` |
| 値 | connId の文字列。「すべて」は `''` を保存する（キーを消さない） |
| 例外 | `localStorage` が使えないときは try/catch で黙って無視する（`setLang` と同じ） |

serverview.js に、Storage を引数で受ける純関数として置きます（テストで差し替えるため）。

```js
export const SERVER_FILTER_STORAGE_KEY = 'sshgate-server-filter';
export function loadServerFilter(storage): string;               // 取れなければ ''。文字列でなければ ''
export function saveServerFilter(storage, connId: string): void; // 例外は握りつぶす
```

復元の手順:

1. `state` の初期化で `serverFilter: loadServerFilter(safeLocalStorage())`、`serverFilterReady: false`。**この時点では検証しない**（接続先の一覧がまだ無いので、正しい値まで消してしまうため）。
2. `refreshData()` が最初に成功し `state.data` を取り込んだ直後に `reconcileServerFilter({ reason: 'init' })`（§3.1）を呼び、`serverFilterReady = true` にする。
3. 以後は `setServerFilter` と `reconcileServerFilter` が変えるたびに `saveServerFilter` する。

`GetInitialData` が失敗した回（`catch` に入った回）は正規化しません。空の一覧で正規化すると、保存値が「すべて」に上書きされるためです。

### 1.3 serverfilter.js に仮定する API

フェーズ3のチームが作る `serverfilter.js` に、次の名前と意味を仮定します。実物の名前が違えば、**serverview.js の import 行と呼び出し部だけ**を直します（main.js からは serverfilter.js を直接呼ばず、必ず serverview.js を通す）。

```js
// records のうち、serverFilter に属するものを返す。
//   serverFilter === ''  → records をそのまま（不明な接続先も含む）
//   serverFilter !== ''  → record.connectionId === serverFilter のものだけ（不明な接続先は含まない）
export function filterByServer(records, serverFilter, connections): Record[];

// 不明な接続先か（connectionId が空、または connections に無い）
export function isUnknownConnection(record, connections): boolean;

// 承認待ちの件数を接続先ごとに数える。
//   { byConn: { [connId]: number }, unknown: number, total: number }
export function countPendingByServer(requests, connections): { byConn, unknown, total };
```

- 照合は `CommandRequest.connectionId`（app.go の json タグ）と `Connection.id` の完全一致だけ。host での推測一致はしない（同じ host の接続先が複数あると誤爆するため）。serverfilter.js が host で補う実装になっていたら §8 の論点として扱う。
- `countPendingByServer` が無い場合は、serverview.js の中で `filterByServer` と `isUnknownConnection` から同じ形を組み立てる。

---

## 2. 各画面への適用

### 2.0 main.js に足す関数（共通）

```js
// 唯一の書き込み口。変わったら true。
async function setServerFilter(connId, { source = 'tab', render = true } = {}): Promise<boolean>;

// データ更新後に呼ぶ。削除された connId なら '' に戻す（§3）。
function reconcileServerFilter({ reason }: { reason: 'init' | 'refresh' | 'sessions' }): void;

// 上部タブとサブタブだけを描き直す（#page-root は触らない）。
function renderServerTabsBar(): void;
```

`setServerFilter` の手順:

1. `next = normalizeServerFilter(connId, state.data.sessions)`（servertabs.js）。`state.serverFilterReady` が false なら何もせず false。
2. `next === state.serverFilter` なら、`source === 'tab'` で接続先管理画面にいるときの「選び直し」（§2.3）だけ行って false。
3. **接続先管理画面で `next !== ''` のとき**は、先に `await selectConnection(next, ctx)`（F3）を呼ぶ。false（破棄確認でキャンセル）なら **serverFilter を変えずに** `renderServerTabsBar()` して false を返す（タブの見た目を元に戻す）。`source === 'editor'`（一覧側から来た）のときはこの手順を飛ばす。
4. `state.serverFilter = next`、`saveServerFilter(storage, next)`。
5. ターミナル画面なら §2.4 の「アクティブなタブの付け替え」を行う。
6. `render` が true なら `renderServerTabsBar()` と `renderPage()`。

servertabs.js の `createServerFilterSetter(state, { onChange })` は同期で、破棄確認を待てないため**使いません**。`renderServerTabs` の `onSelect` には `(connId) => setServerFilter(connId, { source: 'tab' })` を直接渡します。

`renderServerTabsBar()`:

```js
const view = deriveServerView(state, getTerminalState());     // serverview.js
renderServerTabs(qs('#server-tabs'), view.tabs, { t, lang: state.lang, onSelect, labelKeyOf: connStateLabelKey }); // model.js
const sub = qs('#shell-subtabs');
sub.hidden = state.active !== 'terminal';
if (!sub.hidden) renderShellSubtabs(sub, view.subtabs, { t, lang: state.lang, onActivate, onNewShell });
```

呼ぶ場所: `renderAll()` の中（`renderSidebar()` の直後）、`refreshData` の取り込み後（`renderSidebar()` と同じく**常に**。編集中ガードの対象外）、`sessions:changed` の取り込み後、F1 のタブ開閉後（`renderPage()` の直後。F1 は内部で `ctx.renderPage()` を呼ぶので、`renderPage()` の末尾から `renderServerTabsBar()` を呼べば足りる）。

### 2.1 serverview.js（純関数）

```js
export function deriveServerView(state, terminalState): {
  filter: string,                 // 正規化済みの serverFilter
  tabs: ReturnType<buildServerTabs>,
  queue: CommandRequest[],        // 絞り込み済みの承認待ち（requestedAt の既存の並び）
  history: CommandRequest[],      // 絞り込み済みの全件
  selectedRequest: CommandRequest | null,
  hiddenPending: number,          // 絞り込みで見えなくなった承認待ちの件数（不明な接続先を含む）
  unknownIds: Set<string>,        // 不明な接続先のレコード id（行のラベル用）
  subtabs: ReturnType<buildShellSubtabs>,
  stackVisible: boolean,          // ターミナルの pane を見せてよいか（§2.4）
  activeTermId: string,           // 絞り込み後に active にすべきタブ（'' なら無し）
};
```

- `tabs` は `buildServerTabs(sessionsWithPending, { selected: filter, extraPending: counts.unknown, symbolOf: connStateSymbol })`（`connStateSymbol` は model.js。記号をサイドバーと揃える）。`sessionsWithPending` は `state.data.sessions` の各要素の `pending` を `countPendingByServer(state.data.requests, state.data.connections).byConn[connId] ?? 0` で**上書き**したコピー（理由は §8-6。キューの件数とタブのバッジを同じ情報源にする）。
- `queue` は `filterByServer(requests.filter(r => r.status === 'pending'), filter, connections)`、`history` は `filterByServer(requests, filter, connections)`。
- `selectedRequest` は `history` の中から `state.selectedRequestId` を探し、無ければ `queue[0]`、それも無ければ `null`。**絞り込みの外のレコードを選択状態にしない**（見えていない要求を承認ボタンで承認してしまわないため）。
- `hiddenPending = counts.total - queue.length`。
- `subtabs = buildShellSubtabs(terminalState, { serverFilter: filter, sessions: state.data.sessions, terminals: state.data.terminals })`。`terminals` を渡すとシリアルのサブタブに `/dev/cu.usbserial-10 @9600 7E1`（phase4-serial-patch.md §10.6）が付き、sessions:changed の再描画で追従する。
- `activeTermId`: `terminalState.activeId` が `subtabs.items` にあればそれ。無ければ `subtabs.items` の中で一番最近使ったもの（F1 の `usedSeq` があればそれ、無ければ `items` の末尾）。items が空なら `''`。
- `stackVisible = state.active === 'terminal' && activeTermId !== ''`。

### 2.2 承認キューと履歴（`renderCommands` / `requestRow` / `historyTable` / `selectedCommandRequest`）

| 関数 | 変更 |
|---|---|
| `selectedCommandRequest()` | 中身を `deriveServerView(...).selectedRequest` に置き換える。承認・拒否・承認省略・コピー・削除の各ハンドラはこの関数で対象を引くので、絞り込みの外は触れなくなる |
| `renderCommands()` | `requests` の代わりに `view.queue` と `view.history` を使う。キューの件数バッジは `view.queue.length`。`view.hiddenPending > 0` のとき、キューのカードの上に注記 `cmd.hiddenPending`（「ほかの接続先に承認待ちが {n} 件あります［すべて表示］」）を出し、ボタンは `data-action="srvfilter-show-all"` |
| `requestRow(request, selectedId)` | 第3引数 `{ unknown }` を足す。`unknown` なら host の前に `<span class="badge text-bg-secondary unknown-conn">不明な接続先</span>`（`cmd.unknownConnection`）を付ける。host はそのまま出す（`request.host || '-'`） |
| `historyTable(requests, selectedId)` | 第3引数 `unknownIds` を足し、host 列に同じバッジを付ける |
| 空表示 | 絞り込み中で0件のときは `cmd.queueEmptyFiltered`（「{name} の承認待ちはありません」）/ `cmd.historyEmptyFiltered` を使う。「すべて」のときは既存の `cmd.queueEmpty` / `cmd.historyEmpty` |
| 左メニューの承認待ちバッジ（`renderNav`） | **絞り込まない**（全体の件数のまま）。承認待ちを見落とさないため |
| `[data-select-request]` のクリック | 変更なし（絞り込み済みの行しか描かれないので、選べるのは絞り込みの中だけ） |

`srvfilter-show-all` は main.js が処理し、`setServerFilter('', { source: 'commands' })` を呼ぶ。

承認キューで `state.dirty.approval` が true（textarea を編集中）のときにタブを切り替えた場合は、絞り込みで選択が外れると編集内容が消えるので、`setServerFilter` の手順3と同じ位置で `window.confirm(t('cmd.discardEditOnFilter'))` を出し、OK なら `dirty.approval = false` にして進む、キャンセルなら serverFilter を変えない。

### 2.3 接続先管理（タブ選択と一覧選択の双方向連動）

一覧は**絞り込みません**（全件を出し、選択だけを連動させる）。§6.4 の「タブを選ぶと一覧の選択が連動」はこの意味に取ります。

タブ → 一覧:

- `setServerFilter(connId, { source: 'tab' })` の手順3で `await selectConnection(connId, ctx)`（F3）。破棄確認（§6.5）を通るので、未保存の変更はここで守られる。キャンセルされたらタブは元に戻る。
- 「すべて」を選んだときは一覧の選択を変えない。
- 既に同じタブが選ばれていて、一覧側の選択が別の行（例: 一覧で新規 draft を選んだ後）のときは、タブを押し直すと `selectConnection(connId, ctx)` だけを行う（手順2）。

一覧 → タブ:

- F3 には「選択が変わった」コールバックが無い（§8-4）。main.js の振り分けで、`handleEditorClick` / `handleEditorChange` / `handleEditorSubmit` が true を返した後に `syncServerFilterFromEditor()` を呼ぶ。
  ```js
  function syncServerFilterFromEditor() {
    const id = state.editor.selectedId;
    if (!id || id === state.serverFilter) return;              // 新規 draft（''）のときはタブを動かさない
    if (!state.data.sessions.some((s) => s.connId === id)) return; // 保存直後で sessions がまだ古い場合
    setServerFilter(id, { source: 'editor' });                // 手順3を飛ばす
  }
  ```
- 保存（新規）成功時は F3 が `selectedId = result.id` にしてから `await ctx.refresh({silent:true})` するので、ハンドラが戻った時点で sessions に新しい id がある。タブは新しい接続先に移る。
- 削除時は §3.1。

画面に入るとき:

- `navigate('connections')` が成功した後、`state.serverFilter !== ''` かつ `state.editor.selectedId !== state.serverFilter` なら `await selectConnection(state.serverFilter, ctx)`。他の画面から来た時点では editor に未保存の変更は無いはず（離れるときに `confirmLeaveEditor` を通っている）なので、通常は確認なしで移る。

`reconcileEditor(state)`（データ更新後に F3 が選択を直す処理）で選択が変わっても、serverFilter は追従させない（ユーザー操作ではないため）。serverFilter 側は §3.1 で独立に正規化する。

### 2.4 ターミナル（サブタブと「すべて」表示のラベル）

フェーズ2の F1 は `renderTerminalPage(state, t)` で `#page-root` にタブ列（`.term-tabs`）と「新しいシェル」（`.term-new`）を描きます。フェーズ4では**タブ列を `#shell-subtabs`（servertabs.js の `renderShellSubtabs`）に置き換え**、`#page-root` には操作ボタンと空表示だけを描きます。

`renderPage()` のターミナル分岐を `renderTerminalScreen()`（main.js、新規）にします。

```html
<div class="term-toolbar">
  <div class="term-active-actions">                       <!-- active なタブがあるときだけ -->
    <button data-action="term-reconnect" data-term-id="{activeTermId}">再接続</button>  <!-- exited のときだけ -->
    <button data-action="term-close" data-term-id="{activeTermId}">閉じる</button>
  </div>
  {serverFilter === '' のときだけ renderTerminalNewShell(state, t)}   <!-- F1 の .term-new 部分 -->
</div>
{subtabs.items が空のとき: 空表示}
```

- `term-close` / `term-reconnect` / `term-new` / `term-new-select` は F1 の `handleTerminalClick` / `handleTerminalChange` がそのまま処理する（`data-term-id` を closest の要素から読むため）。
- F1 の `renderTerminalPage` から `.term-new` 部分だけを `export function renderTerminalNewShell(state, t): string` として切り出す（terminal-tabs.js への小さな変更。§6 の C3）。`renderTerminalPage` は残してよいが main.js からは呼ばない。
- 空表示: 「すべて」で0件なら F1 の `tabs.empty`。絞り込み中で0件なら `term.emptyFiltered`（「{name} のシェルはありません」）。新しいシェルはサブタブ列の `＋新しいシェル`（servertabs.js の `newShell`）から開く。

サブタブ（`renderShellSubtabs(#shell-subtabs, view.subtabs, { t, lang, onActivate, onNewShell })`）:

- `onActivate(termId)` → `activateTerminalTab(termId)`（F1）→ `renderServerTabsBar()` と `renderPage()`。
- `onNewShell(connId)` → `ctx.openTerminal(connId)`（= `openTerminalTab`）。
- ラベル: servertabs.js の `buildShellSubtabs` が決める。絞り込み中は先頭だけ `web-01 #1`、2つ目以降は `#2`。「すべて」では全タブを `web-01 #2` の形にする。名前は `state.data.sessions` の `connName`（現在名）を優先するので、リネームに追従する。
- exited / starting は `is-exited` / `is-starting` と aria-label の「終了」「接続処理中」で示す（servertabs.js が出す）。

フィルタとアクティブなタブ・pane の表示:

- `setServerFilter` の手順5、`renderPage()` のターミナル分岐の先頭、`sessions:changed` の後で次を行う。
  ```js
  const view = deriveServerView(state, getTerminalState());
  if (view.activeTermId && view.activeTermId !== getTerminalState().activeId) activateTerminalTab(view.activeTermId);
  setTerminalPageVisible(view.stackVisible);
  ```
- **絞り込み先にタブが無いときは `#terminal-stack` ごと隠す**（`setTerminalPageVisible(false)`）。隠さないと、別のサーバーのアクティブな pane が見えたままになり、そこに打鍵してしまう。
- `activateConnection(connId)`（F1）は「その接続先の最後に使ったタブを active にして true」なので、手順5は `filter && activateConnection(filter)` でも同じ結果になる。ただし `activateConnection` は内部で `ctx.renderPage()` を呼ぶため、`setServerFilter` の中では `deriveServerView` の `activeTermId` を使い、`activateTerminalTab` を直接呼ぶ。

### 2.5 サイドバーとの関係

- サイドバー（F2 の `renderSidebarStatus`）は**絞り込まない**。全体の状況を常に見せる場所なので、件数も行も全体のまま。
- 行クリック（`side-open-conn`）は `ctx.showConnection(connId)` を呼ぶ。フェーズ4の `showConnection` は次の順にする。
  1. `await setServerFilter(connId, { source: 'sidebar', render: false })`。接続先管理画面にいて破棄確認でキャンセルされたら、ここで止める。
  2. フェーズ2どおり: `activateConnection(connId)` が true なら `navigate('terminal')`、false なら `navigate('connections')` の後に `selectConnection(connId, ctx)`。
  3. `renderAll()`。
- 「承認待ち N →」（`side-approvals`）は `navigate('commands')` のまま。絞り込み中で見えない分は §2.2 の注記で示す（サイドバーの件数と画面の件数が違って見える理由を、画面側で説明する）。

### 2.6 設定画面

絞り込みの対象外。上部タブは出したままにし（全画面で共有のため）、選ぶと serverFilter は変わるが画面の中身は変わらない。

---

## 3. 接続先の削除・リネームへの追従と、不明な接続先

### 3.1 削除

`reconcileServerFilter({ reason })` を、`refreshData` の取り込み後（`reconcileEditor` の直後）と `sessions:changed` の取り込み後に呼ぶ。

```js
function reconcileServerFilter({ reason }) {
  if (reason === 'init') state.serverFilterReady = true;
  if (!state.serverFilterReady) return;
  const next = normalizeServerFilter(state.serverFilter, state.data.sessions); // servertabs.js
  if (next === state.serverFilter) return;
  const removed = state.serverFilter;
  state.serverFilter = next;                  // '' に戻る
  saveServerFilter(storage, next);
  if (reason !== 'init') showToast(t('srvfilter.removed'));   // 「選んでいた接続先が無くなったため、すべてに戻しました」
}
```

- 起動時（`init`）は、保存値の接続先が既に消えていても黙って「すべて」にする。
- 削除された接続先の承認待ち・履歴は、以後「すべて」に「不明な接続先」として残る（§3.3）。非表示にはしない。
- 削除で閉じたターミナルは exit(user) で F1 が消す。exited のまま残っていたタブ（削除前に既に終了していたもの）は「すべて」にだけ出る。ラベルは F1 が開いた時点の名前（`buildShellSubtabs` が sessions に名前を見つけられないため）。再接続は `not_found` のトーストになる。これは許容し、閉じるボタンで消してもらう。
- 判定に `state.data.sessions` を使うのは servertabs.js の `normalizeServerFilter` に合わせるため。sessions は保存済みの全接続先を `a.conns` の順で持つ（契約 §1.3）ので、`connections` と同じ集合になる。

### 3.2 リネーム

- serverFilter は connId なので、リネームでは変わらない。
- 上部タブの名前は `SessionSummary.connName`、サブタブは `buildShellSubtabs` が sessions の `connName` を優先するので、次のポーリング（または `sessions:changed`）で新しい名前になる。
- 承認キュー・履歴の host 欄は、要求を受けた時点の `request.host` をそのまま出す（記録としての値なので書き換えない）。

### 3.3 不明な接続先のレコード

| 場所 | 「すべて」 | 接続先を選んだとき |
|---|---|---|
| 承認キュー・履歴 | 出す。host の前に「不明な接続先 / Unknown connection」のバッジ | 出さない（`filterByServer` の規則） |
| 「すべて」タブの ⏳ バッジ | 含める（`buildServerTabs` の `extraPending`） | — |
| 各接続先タブの ⏳ バッジ | — | 含めない |
| §2.2 の注記 `hiddenPending` | — | 含める（「ほかの接続先に承認待ちが n 件」に数える） |
| 左メニューの承認待ちバッジ | 含める（絞り込まない） | 含める |

不明な接続先の承認待ちも、「すべて」で選べば承認・拒否できる（フェーズ1の状態遷移に任せる。実行先が無ければバックエンドが失敗を返す）。

辞書（main.js の BASE_I18N に足す。接頭辞 `srvfilter.` と既存の `cmd.` / `term.`）:

| キー | JA | EN |
|---|---|---|
| `cmd.unknownConnection` | 不明な接続先 | Unknown connection |
| `cmd.hiddenPending` | ほかの接続先に承認待ちが {n} 件あります | {n} more awaiting approval on other connections |
| `cmd.showAll` | すべて表示 | Show all |
| `cmd.queueEmptyFiltered` | {name} の承認待ちはありません | Nothing awaiting approval on {name} |
| `cmd.historyEmptyFiltered` | {name} の履歴はありません | No history for {name} |
| `cmd.discardEditOnFilter` | 編集中のコマンドを破棄して切り替えますか？ | Discard the edited command and switch? |
| `term.emptyFiltered` | {name} のシェルはありません | No shells on {name} |
| `srvfilter.removed` | 選んでいた接続先が無くなったため「すべて」に戻しました | The selected connection was removed; showing all |

---

## 4. servertabs.js と shortcuts.js の結線

### 4.1 import と I18N

```js
import {
  SERVERTABS_I18N, ALL_SERVERS, normalizeServerFilter,
  renderServerTabs, renderShellSubtabs,
} from './servertabs';
import { SHORTCUTS_I18N, createShortcutHandler, cycleIndex, resolveTabIndex, cycleServer, detectPlatform } from './shortcuts';
import { deriveServerView, loadServerFilter, saveServerFilter } from './serverview';

const I18N = mergeI18n(BASE_I18N, TERMINAL_I18N, SIDEBAR_I18N, EDITOR_I18N, MODEL_I18N, SERVERTABS_I18N, SHORTCUTS_I18N);
```

- 接頭辞は servertabs.js が `srvtab.`、shortcuts.js が `keys.`、main.js の追加分が `srvfilter.`（と既存の `cmd.` / `term.`）。契約 §2.1 の接頭辞表に3行足す。`mergeI18n` が重複と ja/en の不一致で例外を投げるので、`npm test` の起動テストで検出できる。
- serverview.js は servertabs.js と serverfilter.js を import してよい（main.js 側の補助モジュールであり、F1〜F3 のモジュール間 import 禁止の対象外）。servertabs.js / shortcuts.js 自身は何も import しない（両ファイルの冒頭コメントどおり）。

### 4.2 骨格（shell.js）

`renderSkeleton()` の `<main class="app-main">` に2つ足す（契約 §2.2 の骨格への追記）。

```html
<div class="app-content-header">…</div>
<div id="server-tabs" class="server-tabs-bar"></div>
<div id="shell-subtabs" class="shell-subtabs-bar" hidden></div>
<div id="page-root"></div>
<div id="terminal-dock"></div>
```

`shell.test.js` の JA / EN スナップショットを更新する。`role="tablist"` と aria-label は `renderServerTabs` が付けるので、骨格には書かない。

### 4.3 data-action の名前空間

| 名前 | 処理する所 |
|---|---|
| `srvtab-select`、`srvtab-sub-activate`、`srvtab-new-shell` | servertabs.js がコンテナに付けた自前のリスナ（`wireTablist`）。**main.js の click 振り分けは `[data-action^="srvtab-"]` を見たら何もせず return する**（二重処理を防ぐ） |
| `srvfilter-show-all` | main.js |

`#server-tabs` と `#shell-subtabs` は `#app` の中にあるので、`app.addEventListener('click', …)` にもイベントが届く。振り分けの順序（契約 §3-11）の先頭に `srvtab-*` の早期 return を入れる。

### 4.4 ショートカット

ハンドラを2つ作る。

```js
const platform = detectPlatform();
const modalOpen = () => Boolean(document.querySelector('#editor-modal-root')?.childElementCount);

// 1) ターミナル画面用（全アクション）。xterm と document の両方に同じインスタンスを付ける
const termShortcuts = createShortcutHandler({
  platform,
  enabled: () => state.active === 'terminal' && !modalOpen(),
  actions: {
    newShell:   () => openTerminalTab(state.serverFilter || activeTabConnId() || ''),  // 接続先が決まらなければ何もしない
    closeTab:   () => { const id = getTerminalState().activeId; if (id) return closeTerminalTab(id); },
    nextTab:    () => moveSubtab(+1),
    prevTab:    () => moveSubtab(-1),
    selectTab:  ({ index }) => pickSubtab(index),
    nextServer: () => stepServer(+1),
    prevServer: () => stepServer(-1),
    allServers: () => setServerFilter(ALL_SERVERS, { source: 'shortcut' }),
  },
  onError: (err) => showToast(String(err?.message ?? err)),
});

// 2) ほかの画面用（サーバー移動だけ）。テキスト入力中は横取りしない
const pageShortcuts = createShortcutHandler({
  platform,
  enabled: () => state.active !== 'terminal' && !modalOpen() && !isTextInputFocused(),
  actions: {
    nextServer: () => stepServer(+1),
    prevServer: () => stepServer(-1),
    allServers: () => setServerFilter(ALL_SERVERS, { source: 'shortcut' }),
  },
});

document.addEventListener('keydown', (ev) => (state.active === 'terminal' ? termShortcuts : pageShortcuts).onDocumentKeydown(ev));
```

- `moveSubtab(delta)`: `items = deriveServerView(...).subtabs.items`、`cur = items.findIndex(i => i.active)`、`next = cycleIndex(cur, items.length, delta)`。`next >= 0` なら `activateTerminalTab(items[next].id)` → `renderServerTabsBar()` → `renderPage()`。**絞り込み後のサブタブの並びで移動する**（F1 の `order` 全体ではない）。
- `pickSubtab(index)`: `resolveTabIndex(index, items.length)` で同様。
- `stepServer(delta)`: `ids = deriveServerView(...).tabs.tabs.filter(t => t.connId).map(t => t.connId)`、`setServerFilter(cycleServer(state.serverFilter, ids, delta), { source: 'shortcut' })`。接続先管理画面では破棄確認を通る。
- `activeTabConnId()`: `getTerminalState().tabs.get(getTerminalState().activeId)?.connId`。
- `isTextInputFocused()`: `document.activeElement` が `input` / `textarea` / `select` / `[contenteditable]`。
- ツールチップ: サブタブの `＋新しいシェル` と上部タブの `title` に `shortcutLabel(type, platform)` を足すのは任意（servertabs.js が `title` を上書きするので、足すなら servertabs.js 側に引数を足す。フェーズ4の必須ではない）。

xterm への取り付け（`attachCustomKeyEventHandler`）:

- xterm は F1 の `createTab` の中で作られ、契約に取り付け口が無い（§8-3）。F1 に1か所だけ足す（§6 の C3）:
  ```js
  // terminal-tabs.js createTab の xterm.open(pane) の直後
  if (typeof ctx.terminalKeyHandler === 'function') xterm.attachCustomKeyEventHandler(ctx.terminalKeyHandler);
  ```
  main.js は `ctx.terminalKeyHandler = termShortcuts` を `initTerminalTabs(ctx)` より前に入れる。`reconnectTerminalTab` は xterm を使い回すので付け直し不要。
- F1 を変えられない事情があれば、代わりに main.js の `renderServerTabsBar()` の末尾で `for (const e of getTerminalState().tabs.values()) if (!attached.has(e.xterm)) { e.xterm.attachCustomKeyEventHandler(termShortcuts); attached.add(e.xterm); }`（`attached` は WeakSet）。タブを開いた直後の1回目の描画までは横取りされない隙間があるので、こちらは次善の策。
- 同じ `termShortcuts` を xterm と document の両方に付けるのは必須。shortcuts.js の `seen`（WeakSet）はインスタンスごとなので、別インスタンスにすると同じ打鍵が2回動く。

---

## 5. テスト計画

受け入れ条件: 「タブを切り替えると全画面が絞り込まれ、不明な接続先は『すべて』に残る」。

### 5.0 テストの実行環境

- 契約 §2.6 どおり vitest（`environment: 'jsdom'`、`include: ['src/**/*.test.js']`）。
- `shortcuts.test.js` と `servertabs.test.js` は現状 `node:test` で書かれていて vitest では動かない（§8-11）。フェーズ2のマージ後に `import { test } from 'vitest'` と `assert` → `expect` に直すか、`vitest.config.js` の `exclude` に入れて `node --test` を別に回すかを決める。本書は前者を推す（CI の frontend ジョブが `npm test` 1本で済む）。

共通のフィクスチャ（`frontend/src/__fixtures__/phase4.js`）:

```js
connections: [ { id: 'c-web', name: 'web-01' }, { id: 'c-db', name: 'db-stg' }, { id: 'c-rt', name: 'router-01' } ]
sessions:    上の3件（c-web は connected・shells 2、他は disconnected）
requests: [
  { id: 'R1', connectionId: 'c-web', status: 'pending', host: 'web-01.local' },
  { id: 'R2', connectionId: 'c-web', status: 'succeeded' },
  { id: 'R3', connectionId: 'c-db',  status: 'pending' },
  { id: 'R4', connectionId: '',      status: 'pending', host: 'legacy-host' },   // 旧レコード
  { id: 'R5', connectionId: 'c-gone', status: 'failed' },                         // 削除済みの接続先
]
terminalState: tabs = { T1: c-web #1, T2: c-web #2, T3: c-db #1(exited) }, order = [T1, T3, T2], activeId = T3
```

### 5.1 純関数（DOM 無し）

`serverview.test.js`（新規、表駆動）:

| ケース | 期待 |
|---|---|
| filter `''` | `queue` = R1, R3, R4。`history` = R1〜R5。`unknownIds` = {R4, R5}。`hiddenPending` 0。「すべて」タブの pending = 3 |
| filter `c-web` | `queue` = R1。`history` = R1, R2。R4・R5 を含まない。`hiddenPending` 2。c-web タブ pending 1、`tabs.selected === 'c-web'` |
| filter `c-db` | `queue` = R3。`history` = R3 |
| filter `c-rt` | `queue` / `history` 空。`hiddenPending` 3 |
| filter `c-gone`（存在しない） | `filter === ''` として扱う（正規化）。結果は `''` と同じ |
| `selectedRequestId = 'R3'`、filter `c-web` | `selectedRequest` は R1（R3 は選ばれない） |
| `selectedRequestId = 'R4'`、filter `''` | `selectedRequest` は R4 |
| サブタブ filter `c-web` | items = T1, T2、表示は `web-01 #1`, `#2`。`activeTermId` は T1 か T2（T3 ではない）。`stackVisible` true（active==='terminal' のとき） |
| サブタブ filter `''` | items = T1, T3, T2、すべて `name #n`。`activeTermId` = T3 |
| サブタブ filter `c-rt` | items 空、`activeTermId ''`、`stackVisible` false |
| リネーム（sessions の c-web の connName を `web-01b`） | サブタブ表示が `web-01b #1`。filter は `c-web` のまま |
| sessions の pending と requests がずれている | タブの pending は requests から数えた値（§2.1） |

`loadServerFilter` / `saveServerFilter`: 値あり / 値なし / Storage が例外を投げる / 非文字列。

既存部品の確認（それぞれのチームのテストに無ければ足す）: `filterByServer` の表（serverfilter.js。`''` で全件、connId で一致のみ、不明は `''` にだけ）、`normalizeServerFilter`、`buildServerTabs` の `extraPending`、`cycleServer(current, ids, ±1)` の端の折り返し。

### 5.2 jsdom（DOM あり）

`servertabs.dom.test.js`（servertabs.js のチームが持っていなければ本書の担当が書く）:

- `renderServerTabs` を同じコンテナに2回呼んでもボタンのノードが同一（`toBe`）。2回目の `onSelect` が使われる。
- タブのクリックと ArrowRight で `onSelect` に connId が渡る。`aria-selected` が1つだけ true。
- 記号・名前・⧉・⏳ の span がそれぞれ出て、0 件のバッジは `hidden`。

`main.phase4.test.js`（新規。main.js の結線を通しで確かめる）:

- 準備: `document.body.innerHTML = '<div id="app"></div>'`、`vi.mock('../wailsjs/go/main/App')`（`GetInitialData` がフィクスチャを返す。`ApproveCommand` 等は spy）、`vi.mock('../wailsjs/runtime/runtime')`（`EventsOn` は登録を記録）、`vi.mock('@xterm/xterm')` と `@xterm/addon-fit`（F1 の terminal-tabs.test.js と同じ mock）、`vi.useFakeTimers()`、`localStorage.clear()`。その後 `await import('./main.js')` して最初の `refreshData` を待つ。
  - main.js が import 時に起動処理を走らせる形のままだと、テストごとに `vi.resetModules()` が要る。可能なら main.js の末尾を `export async function boot()` にして、`if (!import.meta.vitest) boot()` で起動する形にする（フェーズ4の作業 C1）。
- **受け入れ条件そのもの**（1本のテストで画面を巡る）:
  1. 承認キュー画面（`[data-nav="commands"]` をクリック）。「すべて」で `[data-select-request]` のキュー行が R1, R3, R4、R4 の行に `.unknown-conn` がある。履歴の行が5件で、R4・R5 に `.unknown-conn`。
  2. `#server-tabs` の web-01 のボタンをクリック。キュー行が R1 だけ、履歴が R1, R2。`cmd.hiddenPending` の注記に 2。
  3. 接続先管理画面へ移る。一覧の選択（F3 の選択行のクラス）が web-01。一覧の行数は3のまま（絞り込まない）。
  4. 一覧で db-stg の行をクリック。`#server-tabs` の db-stg が `aria-selected="true"`。
  5. ターミナル画面へ移る。`#shell-subtabs` に db-stg のタブだけ（T3）。`#terminal-stack` が表示。
  6. router-01 タブ。`#shell-subtabs` には `＋新しいシェル` だけ、`#terminal-stack` が hidden、空表示に `term.emptyFiltered`。
  7. 「すべて」タブ。サブタブが3つで、全部 `name #n` の形。承認キュー画面に戻ると R4 が見える（**不明な接続先は「すべて」に残る**）。
- 保存と復元: タブで c-db を選ぶと `localStorage['sshgate-server-filter'] === 'c-db'`。モジュールを読み直す（`vi.resetModules()` → import）と c-db が選ばれた状態で始まる。
- 起動時の削除: `localStorage` に `c-gone` を入れて起動すると「すべて」で始まり、トーストは出ず、保存値が `''` になる。
- 実行中の削除: c-db を選んだ状態で、次の `GetInitialData` から c-db を消して `vi.advanceTimersByTime(5000)`。serverFilter が `''`、`srvfilter.removed` のトースト、R3 は「すべて」に「不明な接続先」として出る。
- 破棄確認: 接続先管理画面で入力して dirty にした後、タブで別の接続先を押し、3択でキャンセル → serverFilter も一覧の選択も元のまま、タブの `aria-selected` も元に戻る。
- 承認の対象: c-web を選んでいる間に `state.selectedRequestId` を R3 にしても（R3 の行は描かれないので内部から設定）、承認ボタンで呼ばれる `ApproveCommand` の id は R1。
- ポーリングで壊れない: `#server-tabs` のボタンにフォーカスを置いて 5 秒進めても、同じノードにフォーカスが残る。
- ショートカット: ターミナル画面で `keydown`（mac: `metaKey + altKey + ArrowRight`）を document に送ると serverFilter が次の接続先になる。xterm の mock に渡された `attachCustomKeyEventHandler` の関数が、同じイベントで2回動かない。`Cmd+2` で絞り込み後の2番目のサブタブが active になる。接続先管理画面の input にフォーカスがあるときは `Cmd+Alt+→` を横取りしない。

### 5.3 手動確認（Wails 実機。CI では回らない）

- macOS で Cmd+T / Cmd+W / Cmd+Shift+] / Cmd+1〜9 / Cmd+Alt+←→0 がシェルに文字を送らない。Windows で Ctrl+Shift の同等キー。
- 日本語 IME の変換中に Cmd+数字を押しても横取りしない。
- 上部タブが多いときに横スクロールで全部に届く（style.css の担当と確認）。

---

## 6. 作業チェックリスト（フェーズ2のマージ後）

順番どおりに進め、各段で `npm test` と `npm run build` を通す。

**A. 準備**

- [ ] A1. フェーズ2をマージした main からブランチを切り直し、`servertabs.js` / `shortcuts.js` / `serverfilter.js`（フェーズ3）を取り込む。`serverfilter.js` が無ければ §1.3 の3関数を serverview.js の中に暫定で書き、TODO を残す。
- [ ] A2. 本書 §1.3 の仮定と、実際の `serverfilter.js` の export 名・引数・`''` の扱い・host 補完の有無を突き合わせる。違えば serverview.js の呼び出しだけで吸収する。
- [ ] A3. 契約 §2.2 と実装の `state` / `ctx`、F1 の export（`renderTerminalPage`、`activateTerminalTab`、`getTerminalState`、`createTab` の位置）を確認する。
- [ ] A4. `shortcuts.test.js` と `servertabs.test.js` を vitest に直す（§5.0）。

**B. 純関数**

- [ ] B1. `frontend/src/serverview.js` を作る: `SERVER_FILTER_STORAGE_KEY`、`loadServerFilter`、`saveServerFilter`、`deriveServerView`（§2.1）。
- [ ] B2. `serverview.test.js`（§5.1 の表）を書いて通す。

**C. 他モジュールへの小さな変更**

- [ ] C1. main.js の起動処理を `boot()` にまとめ、テストから import できるようにする（§5.2）。
- [ ] C2. `shell.js` の `renderSkeleton` に `#server-tabs` と `#shell-subtabs` を足し、`shell.test.js` のスナップショットを更新する（§4.2）。
- [ ] C3. `terminal-tabs.js`: (a) `createTab` で `ctx.terminalKeyHandler` を `attachCustomKeyEventHandler` する、(b) `.term-new` 部分を `renderTerminalNewShell(state, t)` として export する。既存の `terminal-tabs.test.js` を通し、(a) の mock 確認を1件足す。
- [ ] C4. `style.css`: `.server-tabs-bar`、`.server-tab`（`.is-active` / `.is-all` / `.is-serial` / `.is-connected` ほか）、`.server-tab-symbol|name|shells|pending`、`.shell-subtabs-bar`、`.shell-subtab`（`.is-active` / `.is-exited` / `.is-starting`）、`.shell-subtab-new`、`.term-active-actions`、`.unknown-conn`。横に溢れたら `overflow-x:auto`。色だけに頼らない。

**D. main.js の結線**

- [ ] D1. import と `mergeI18n` に `SERVERTABS_I18N` / `SHORTCUTS_I18N` を足す。§3.3 の辞書キーを BASE_I18N に足す。
- [ ] D2. `state.serverFilter` / `state.serverFilterReady` を足し、`loadServerFilter` で初期化する（§1.1, §1.2）。`ctx.serverFilter` / `ctx.setServerFilter` / `ctx.terminalKeyHandler` を足す。
- [ ] D3. `setServerFilter`、`reconcileServerFilter`、`renderServerTabsBar`、`syncServerFilterFromEditor` を書く（§2.0, §2.3, §3.1）。
- [ ] D4. `refreshData`: 取り込み → `reconcileEditor` → `reconcileServerFilter({ reason: 初回なら 'init' / 以後 'refresh' })` → `reconcileTerminalTabs` → `renderSidebar` / `renderNav` / `renderServerTabsBar`（常に）→ 見送り条件に当たらなければ `renderPage`。
- [ ] D5. `sessions:changed` の受け口に `reconcileServerFilter({ reason: 'sessions' })` と `renderServerTabsBar()` を足す。
- [ ] D6. `renderAll()` に `renderServerTabsBar()` を足す。`renderPage()` の末尾でも呼ぶ。
- [ ] D7. 承認キュー: `selectedCommandRequest`、`renderCommands`、`requestRow`、`historyTable` を §2.2 のとおりに変える。`srvfilter-show-all` を処理する。承認・拒否・承認省略・削除・コピーの各ハンドラが `selectedCommandRequest()` を通っていることを確認する。
- [ ] D8. 接続先管理: `setServerFilter` の手順3（`selectConnection`）、editor ハンドラの後の `syncServerFilterFromEditor()`、`navigate('connections')` 後の選択合わせ（§2.3）。
- [ ] D9. ターミナル: `renderTerminalScreen()` に置き換え、`renderShellSubtabs` の `onActivate` / `onNewShell`、アクティブなタブの付け替えと `setTerminalPageVisible(view.stackVisible)`（§2.4）。フェーズ2の `setTerminalPageVisible(state.active === 'terminal')` の呼び出しを置き換える。
- [ ] D10. `showConnection` を §2.5 の順に直す。
- [ ] D11. click 振り分けの先頭に `[data-action^="srvtab-"]` の早期 return を足す（§4.3）。
- [ ] D12. ショートカット: `termShortcuts` / `pageShortcuts` と document の keydown リスナを足す。`ctx.terminalKeyHandler = termShortcuts` を `initTerminalTabs(ctx)` より前に入れる（§4.4）。
- [ ] D13. 起動順序: `renderSkeleton()` → `initTerminalTabs(ctx)` → `await refreshData()`（ここで `init` の正規化）→ `await restoreTerminalTabs()` → `renderAll()` → `setInterval(...)`。

**E. テストと確認**

- [ ] E1. `main.phase4.test.js` と（無ければ）`servertabs.dom.test.js` を書く（§5.2）。
- [ ] E2. `npm test`、`npm run build`、`go vet ./...`、`go test -race -count=1 ./...` を通す。
- [ ] E3. §5.3 の手動確認を macOS と Windows で行う。
- [ ] E4. 使われなくなった I18N キーと CSS クラスを grep で確かめて消す。契約 §2.1 の接頭辞表と §2.2 の state / ctx / 骨格に、本書の追加分を反映する。

---

## 7. やらないこと

- サイドバーの絞り込み（§2.5）。接続先管理の一覧の絞り込み（§2.3）。
- serverFilter のバックエンド保存。MCP 側での絞り込み。
- host による不明レコードの推測一致（§1.3）。
- シリアル固有の表示（`device_removed` の［再接続］表示など）。phase4-serial-patch.md の範囲。

---

## 8. フェーズ2の契約との食い違い・不足

| # | 内容 | 本書での扱い |
|---|---|---|
| 1 | 契約 §2.2 の `state` に `serverFilter` が無く、`ctx` に設定口も無い（契約の冒頭でスコープ外と明記） | §1.1 で `state.serverFilter` / `serverFilterReady` と `ctx.serverFilter` / `ctx.setServerFilter` を足す |
| 2 | design-v1 §7 フェーズ4のテスト欄は「model.js の filterByServer」、今回の前提は「serverfilter.js」。model.js は F4 の所有で、契約 §2.1 の「import してよいのは model.js だけ」とも関わる | main.js 側の serverview.js からだけ serverfilter.js を import する。F1〜F3 は import しない |
| 3 | xterm は F1 の `createTab` 内で作られ、`attachCustomKeyEventHandler` を付ける口が契約に無い | C3 で `ctx.terminalKeyHandler` を足す（次善の策は §4.4） |
| 4 | F3 に「一覧の選択が変わった」通知が無い。契約では main.js が `state.editor` を書けない | ハンドラが true を返した後に `state.editor.selectedId` を読んで追従（§2.3）。書くときは `selectConnection` を通す |
| 5 | F1 の `renderTerminalPage(state, t)` は全タブを `#page-root` に描き、serverFilter を知らない。servertabs.js の `renderShellSubtabs` には閉じる・再接続のボタンが無い | タブ列は `#shell-subtabs`（servertabs.js）、閉じる・再接続は active なタブに対して `#page-root` に出す。`.term-new` 部分を F1 から `renderTerminalNewShell` として切り出す（C3） |
| 6 | `SessionSummary.pending` はバックエンドが数えるが、契約 §1.4 では `sessions:changed` を承認要求の追加・決定では送らない。キュー（requests から数える）とタブのバッジが一時的に食い違いうる | タブの pending は requests から数え直して上書き（§2.1） |
| 7 | 不明な接続先の承認待ちは、どの `SessionSummary.pending` にも入らない | 「すべて」タブに `extraPending` で足す（servertabs.js の引数をそのまま使う） |
| 8 | `CommandRequest.connectionId` が空の旧レコードを host で補うかどうかが、契約にも design-v1 にも無い | connectionId の一致だけで判定し、空は不明扱い（§1.3）。serverfilter.js の実装と要確認 |
| 9 | `setTerminalPageVisible(visible)` は画面単位の想定。絞り込み先にタブが無いと、別サーバーの active な pane が見えたままになる | `stackVisible` で隠す（§2.4）。API の追加は不要 |
| 10 | 骨格（契約 §2.2）に `#server-tabs` / `#shell-subtabs` が無い。data-action の名前空間表（§2.1）に `srvtab-*` / `srvfilter-*` が無い。I18N 接頭辞表に `srvtab.` / `keys.` / `srvfilter.` が無い | §4.2 / §4.3 / §4.1 で追記。`srvtab-*` は main.js の振り分けで素通しにする |
| 11 | `shortcuts.test.js` と `servertabs.test.js` は `node:test` で書かれており、契約 §2.6 の vitest（`include: src/**/*.test.js`）に拾われると失敗する | A4 で vitest に直す |
| 12 | servertabs.js の `createServerFilterSetter` は同期で、接続先管理画面の破棄確認（非同期の3択）を待てない | 使わず、main.js の async な `setServerFilter` を `onSelect` に渡す |
| 13 | 参考: 本体で進行中の sidebar.js に、契約に無い `updateSidebarStatus(root, data, options)` が export されている | 本書では使わない。統合時に契約へ反映するかを Integrator が決める |
| 14 | §3.3 の `cmd.unknownConnection` / `cmd.hiddenPending` / `cmd.showAll` / `cmd.queueEmptyFiltered` / `cmd.historyEmptyFiltered` と `term.emptyFiltered` | 統合では承認キューが approvals.js に移ったので `appr.unknownConnection` / `appr.hiddenPending` / `appr.showAll` / `appr.queueEmptyFiltered` / `appr.historyEmptyFiltered`、ターミナルの空表示は F1 の `tabs.emptyFiltered`。BASE_I18N に残したのは `srvfilter.removed` と `cmd.discardEditOnFilter` だけ |
| 15 | §1.3「host での推測一致はしない」と serverfilter.js の `recordConnId`（connectionId が空の旧レコードを、SSH の接続先**名**がちょうど1件一致するときだけその接続先とみなす） | serverfilter.js の規則を採用。旧レコードの `host` 列には接続先名が入っており、バックエンドの `resolveRequestTarget` も同じ名前で実行先を解決するため、画面の絞り込みと承認時の実行先が一致する。IP や host 名での推測はしない（同名が2件以上なら不明扱い） |
| 16 | §6 C1（main.js の `boot()` 化） | 見送り。main.js は import 時に起動し、`main.phase4.test.js` は `vi.resetModules()` で読み直す。document の keydown リスナが読み直しのたびに増えるため、ショートカットのテストは最初のインスタンスで行う |
| 17 | 記号表 | `missing` を ▲ から ○ に変更（phase4-serial-patch §10-7）。model.js の `connStateSymbol` を唯一の置き場にし、serverfilter.js と servertabs.js はそれに合わせた |

