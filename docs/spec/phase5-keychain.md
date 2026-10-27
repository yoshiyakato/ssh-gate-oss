# フェーズ5 設計：パスワード・パスフレーズを OS キーチェーンへ移す

この文書は design-v1.md §3（「OS キーチェーン：後回し（第5段）」）、§5.0（所有権とロック）、§5.5（データモデル、パスフレーズのキャッシュキー）、§7 フェーズ5 を受けて、キーチェーン対応の設計を決め切るためのものです。実装はまだ行いません。最後の §10 にユーザーが判断すべき点を並べます。

- 基準: ブランチ `feat/v1-phase5-pool`（`a6f2706` 起点）。現行コードの事実は `app.go` / `storage.go` / `ssh_exec.go` / `approval.go` から読み取っています。
- 方針: プールと同じく、キーチェーンも新規ファイル（`secret_store.go` ほか）で完結させ、既存ファイルへの組み込みは §6 の「差し込み点」だけにします。

---

## 0. 現状（コードから確認した事実）

| 項目 | 現状 | 出典 |
|---|---|---|
| パスワードの保存先 | `connections.credential` 列に**平文**。password 認証のときはパスワード、鍵認証のときは鍵のパス | `storage.go:105`, `app.go:70-72` |
| 仕様との名前のずれ | design-v1 §5.5 は `password` 列と書いているが、実装は `credential` 1列で兼用している。本書は実装に合わせて `credential` と呼ぶ | `storage.go:326` |
| UI への露出 | 書き込み専用。`CredentialSet` だけ返し、password のときは `Credential` を空にして返す | `app.go:338-341` |
| 空で保存したとき | 既存の値を維持する | `app.go:514-515` |
| パスフレーズ | **永続化していない**。`a.sshPassphrases[connID]` にメモリだけで持つ。対象が変わる・削除で消す | `app.go:50, 414, 538, 565` |
| DB ファイル | 0600。WAL/SHM も 0600 | `storage.go:74-80` |
| 移行時の退避 | `VACUUM INTO <db>.bak-v<N>` を 0600 で作る。**この退避ファイルにも平文パスワードが残る** | `storage.go:147-156` |
| 他の秘密 | `mcp_settings.token`（MCP の Bearer トークン）も DB に平文 | `storage.go` mcp_settings |

---

## 1. 候補ライブラリの比較（ソースを取得して確認）

`go mod download` で取得したソースを読み、`CGO_ENABLED=0 GOOS={darwin,windows,linux}` で試しにビルドして確かめました（2026-10-27）。

| 観点 | **github.com/zalando/go-keyring v0.2.8** | **github.com/99designs/keyring v1.2.2** |
|---|---|---|
| 最終リリース | 2026-03-23（v0.2.x を継続して出している） | 2022-12-19。以降リリースなし |
| API | `Set/Get/Delete(service, user[, password])`、`DeleteAll(service)`、`ErrNotFound`、`ErrSetDataTooBig`。`MockInit()` あり（ただしパッケージのグローバル変数を差し替える方式） | `keyring.Open(Config)` → `Keyring{Get, Set, Remove, Keys, GetMetadata}`。バックエンドを複数選べる |
| macOS | `/usr/bin/security` を **exec** する（`keyring_darwin.go`）。秘密は argv ではなく `security -i` の stdin で渡す。値は base64 で前置詞付き保存 | `github.com/99designs/go-keychain`（Security.framework を **cgo** で呼ぶ）。ビルドタグ `darwin && cgo`。go-keychain は 2019 年のフォーク版に固定 |
| Windows | `github.com/danieljoos/wincred`（syscall、cgo 不要）。TargetName は `service + ":" + user`、`PersistLocalMachine`（ローミングしない）。パスワード上限 2560 バイト | 同じく wincred v1.1.2（古い） |
| Linux | Secret Service を D-Bus で直接呼ぶ（`godbus/dbus/v5`、cgo 不要）。login コレクションを Unlock してから読み書き | Secret Service（`gsterjov/go-libsecret`、2016年）、KWallet、keyctl、pass、暗号化ファイル（jose2go） |
| cgo 要否 | **不要**。3 OS とも `CGO_ENABLED=0` でクロスビルドできることを確認 | macOS の Keychain バックエンドは cgo 必須。`CGO_ENABLED=0` だと静かに Keychain が無くなり、file/pass にしか落ちない |
| 追加される依存 | `zalando/go-keyring`、`danieljoos/wincred v1.2.3`、**`godbus/dbus/v5` が v5.1.0 → v5.2.2 に上がる**（Wails の間接依存。go-keyring 側が v5.2.2 を要求するため、MVS で上がる） | go-keychain、wincred、`godbus/dbus`（**v5 ではない旧パス**。Wails の v5 と二重に入る）、go-libsecret、jose2go、percent、x/term、testify |
| 未署名ビルドの許可ダイアログ（§3 の懸念） | 項目の ACL が信頼するのは `/usr/bin/security`（Apple 署名）なので、**ssh-gete を再ビルドしても許可ダイアログは出ない**（ソースからの推論。実機の Keychain には書いていません。§9.3 の手動確認で確かめる） | 項目の ACL は呼び出したアプリを信頼する（`TrustedApplications: nil`）。未署名・ad-hoc 署名のビルドは毎回コード署名が変わるため、**ビルドのたびに「キーチェーンへのアクセスを許可」ダイアログが出る**。これが §3 で後回しにした理由そのもの |
| 隔離の強さ（macOS） | 弱い。同じユーザーで動く任意のプロセスが `security find-generic-password -s ssh-gete -a … -w` を叩けば、ダイアログなしで読める | 強い。他アプリは許可ダイアログを経ないと読めない（ただし上記の UX 問題と引き換え） |
| Linux で無応答になる箇所 | `handlePrompt` が Prompt の完了シグナルを**タイムアウトなしで待つ**（`secret_service.go:187-` の `<-promptSignal`）。呼び出し側でタイムアウトが必要 | 同種（go-libsecret 経由）。さらにバックエンドの自動選択で pass や file のパスワード入力（端末）に落ちる可能性がある |
| Wails ビルドへの影響 | macOS: Wails 自体が cgo を使うが、go-keyring は cgo を足さない。App Sandbox を有効にしない限り `/usr/bin/security` の exec は可能（ssh-gete は現状 Sandbox なし）。Windows/Linux: 影響なし | macOS: cgo とリンクするフレームワーク（Security, CoreFoundation）が増える。`build/darwin/Info.plist` の `CFBundleIdentifier` が `com.wails.{{.Name}}` のままだと ACL・アクセスグループの扱いが曖昧になる |

**他の選択肢（採らない）**
- `github.com/keybase/go-keychain` を macOS だけ直接使い、Windows は wincred、Linux は go-keyring の secret_service を使う「自前の合成」。隔離は強いが、cgo と §3 の許可ダイアログ問題をそのまま抱えるので、署名・公証の体制ができるまで採りません。
- Windows DPAPI（`CryptProtectData`）で DB 内の値を暗号化する案。Windows だけの解になり、他 OS と仕組みが分かれるので採りません。

---

## 2. 推奨

**`github.com/zalando/go-keyring` v0.2.8 を採用し、自前の薄いインターフェース `SecretStore` で包む。**

理由:
1. cgo が要らず、3 OS とも同じコードで動く。CI の matrix（ubuntu/macos/windows）とクロスビルドが今のまま通る。
2. 未署名ビルドでも許可ダイアログが出ない。§3 の「UX が未設計」という懸念が、ライブラリの選択だけで消える。
3. メンテが続いている。依存が少なく、godbus は Wails が既に使っている v5 系を上げるだけで済む。

受け入れる弱点（§10 で確認したい点）:
- macOS では、同じユーザーの他プロセスがダイアログなしで読める。「0600 の DB ファイル」と同じ脅威モデルで、改善点は「平文が DB・バックアップ・WAL・Time Machine・クラウド同期フォルダに残らない」ことに限られる。
- `security` の exec は 1 回 20〜50ms 程度かかる（実測は §9.3）。メモリキャッシュで吸収する。

go-keyring の `MockInit()` はパッケージのグローバル変数を書き換えるので、並行テスト（`-race -count=20`）で競合します。**テストでは使わず**、`SecretStore` の注入で差し替えます。

---

## 3. 公開 API（新規ファイル `secret_store.go`）

```go
// SecretStore はパスワード・パスフレーズの保存先。実装は OS キーチェーンかメモリ。
// すべてのメソッドは ctx の期限で必ず戻る（Linux の Prompt 待ちで固まらない）。
type SecretStore interface {
	Get(ctx context.Context, account string) (string, error)
	Set(ctx context.Context, account, secret string) error
	Delete(ctx context.Context, account string) error // 無いときは nil
}

var (
	ErrSecretNotFound    = errors.New("secret not found")      // 項目が無い
	ErrSecretUnavailable = errors.New("secret store unavailable") // 未対応 OS、D-Bus なし、ロック解除の拒否、タイムアウト
	ErrSecretTooBig      = errors.New("secret too big")
)

// newOSSecretStore は go-keyring を包む。service は "ssh-gete"。
// 各呼び出しを goroutine で実行し、ctx が先に終わったら ErrSecretUnavailable を返す
// （go-keyring 自体は ctx を受けないため、取り残された goroutine は完了を待って捨てる）。
// 同じ account への同時 Get は singleflight でまとめる。
func newOSSecretStore(service string) SecretStore

// newMemorySecretStore はテストとフォールバック用。並行安全。
func newMemorySecretStore() *memorySecretStore

// faultSecretStore はテスト用。Set/Get/Delete ごとにエラー・遅延・ハングを注入できる。
type faultSecretStore struct{ /* inner SecretStore; setErr, getErr, delay ... */ }

// キー命名（§4）
func passwordAccount(connID string) string
func passphraseAccount(connID, keyPath string) string
```

App 側に増やすもの（§6 で差し込む。実装は本フェーズの統合担当）:

```go
// App のフィールド（フェーズ0の差し替え口と同じ流儀）
secrets     SecretStore           // 既定 newOSSecretStore("ssh-gete")。テストで差し替え
secretCache map[string]string     // account → 値。a.mu で保護。値の取得は a.mu の外
```

```go
// resolveCredential は接続に必要な秘密を埋めた Connection のコピーを返す。
// a.mu を持たずに呼ぶ（§5.0）。プールの dial 関数・executeSSH・ターミナルを開く処理から呼ぶ。
func (a *App) resolveCredential(ctx context.Context, conn Connection) (Connection, error)
```

プールとの関係: プールの dial は `func(ctx, Connection) (*ssh.Client, error)` を注入する設計なので、App が渡す dial 関数の中で `resolveCredential` を呼びます。こうすると `pool.mu` の外で秘密を取得でき（§5.1「dial は pool.mu の外」）、プール自身は平文を保持しません。pooledClient の fp に入れる credential は `sha256(processSalt || secret)` にします。

---

## 4. キー命名（connId 基準）

| 種類 | service | account |
|---|---|---|
| パスワード | `ssh-gete` | `conn/<connId>/password` |
| 鍵のパスフレーズ | `ssh-gete` | `conn/<connId>/passphrase/<hex(sha256(expandPath(keyPath)))[:16]>` |

- `connId` は不変の UUID（§2 の 5 番）なので、名前を変えてもキーは変わりません。キーチェーンの一覧に**ホスト名やユーザー名が出ない**のも利点です。
- パスフレーズは §5.5 の「`(connId, keyPath のハッシュ)` をキーにする」に合わせます。鍵のパスを変えたら別のキーになり、古いキーは §5.2 の `secret_gc` で消します。
- Windows の TargetName は `ssh-gete:conn/<id>/password` になります（go-keyring が `service:user` に連結する）。
- テスト・開発ビルドは service を変えられるようにします（環境変数 `SSH_GETE_KEYRING_SERVICE`。既定 `ssh-gete`。newTestApp は `SSH_GETE_*` を隔離するので本番の項目に触れません）。

---

## 5. 保存・読み出し・削除の流れ

### 5.1 DB 側の変更（schema v3）

- `connections` に `credential_store TEXT NOT NULL DEFAULT 'db'` を足します。値は `db`（従来どおり列に平文）か `keyring`（列は空、値はキーチェーン）。
- `connections` に `passphrase_saved INTEGER NOT NULL DEFAULT 0` を足します（パスフレーズを覚える機能を入れる場合のみ。§10-3）。
- `secret_gc(account TEXT PRIMARY KEY, created_at INTEGER)` を足します。キーチェーンから消し損ねた項目の再試行キューです。
- `CredentialSet` は `credential_store='keyring'` か `credential != ''` で真にします。

### 5.2 保存（SaveConnection）

順序は §5.0 の「DB に書けたらメモリに反映する」を守り、さらに「キーチェーンに書けたら DB から消す」を加えます。

1. password 認証で新しい値が入力されたら、`a.mu` を持たずに `secrets.Set(passwordAccount(id), pw)` を呼ぶ（タイムアウト 10 秒）。
2. 成功したら `Get` で読み戻して一致を確かめる（macOS の base64 前置詞などで化けないことの保証）。
3. 一致したら DB を `credential='', credential_store='keyring'` で更新する。新規作成のときは、id を先に採番して 1〜3 を行う。
4. DB 更新が成功したら `secretCache` を更新する。
5. 1〜2 が失敗したら、§8 のフォールバックに従う。

認証方式を key に変えた・Serial に変えた・接続先を削除したときは、DB を更新した後に `secrets.Delete` を呼び、失敗したら `secret_gc` に入れます（起動時と1時間ごとに再試行）。

### 5.3 読み出し（接続テスト・ターミナル・MCP 実行）

1. `a.mu.RLock` 中に `credential_store` と `credential` をコピーして解放する。
2. `keyring` なら `secretCache` を見て、無ければロックの外で `secrets.Get`（GUI 起点はタイムアウト 60 秒＝ロック解除のダイアログを待つ。MCP 起点は 5 秒）。
3. 結果をキャッシュに入れる。キャッシュは保存・削除・認証方式の変更・接続テストの認証失敗で消す。

### 5.4 パスフレーズ

- 既定は今と同じくメモリだけ（`sshPassphrases`）。
- 入力モーダルに「キーチェーンに保存」のチェックボックスを付けた場合（§10-3）だけ、`passphraseAccount(id, keyPath)` に保存し、`passphrase_saved=1` にします。
- 鍵のパスや接続先を変えたとき（今 `delete(a.sshPassphrases, id)` している箇所）は、キーチェーンの項目も消します。

---

## 6. 既存コードへの差し込み点（統合担当が行う。本タスクでは触らない）

| ファイル | 箇所 | 変更 |
|---|---|---|
| `storage.go` | `migrate` | schema v3 の列追加、`secret_gc` テーブル、`PRAGMA secure_delete=ON` |
| `storage.go` | `loadConnections` / `insertConnection` / `updateConnection` | `credential_store` 列の読み書き |
| `app.go` | `SaveConnection`（514 行付近の「空なら既存を維持」） | §5.2 の順序。`a.mu` を持ったまま Set しない |
| `app.go` | `DeleteConnection`、`!sameTarget` の分岐 | キーチェーンの Delete と `secret_gc` |
| `app.go` | `TestConnectionConfig`（367 行付近の保存済みパスワードの補完） | `resolveCredential` に置き換え |
| `approval.go` | `executeSSH`（345 行付近） | `resolveCredential(ctx, conn)` を `a.mu` の外で呼ぶ |
| `terminal.go` | SSH ターミナルを開く処理 | 同上 |
| `ssh_pool.go`（フェーズ5の新規） | dial 注入 | App が渡す dial の中で `resolveCredential` |
| `main.js` | needs_passphrase と同じ共通モーダル | `needs_password`、`keyring_unavailable` の扱い |

---

## 7. DB の credential 列からの移行手順

起動時に 1 回、`store.Open` と旧 JSON の移行が終わった後、MCP の受付を始める**前**に行います（MCP が移行途中の接続先を読まないようにするため）。

1. `user_version` を 3 に上げる前に、既存の仕組みで `VACUUM INTO <db>.bak-v2` を作る。
2. `SELECT id, credential FROM connections WHERE auth_method='password' AND credential_store='db' AND credential != ''` を全件読む。
3. 1 件ずつ、ロックの外で `Set` → `Get` で読み戻して一致を確認 → 1 行の `UPDATE connections SET credential='', credential_store='keyring' WHERE id=? AND credential=?`（値が変わっていないことを条件にする）。
4. 途中で落ちても安全です。`Set` は上書き（macOS は `-U`、Linux は replace=true、Windows は CredWrite）なので、次回の起動で同じ手順を繰り返せば済みます。DB の平文は、キーチェーンへの書き込みと読み戻しが確認できるまで消しません。
5. 失敗した行は `db` のまま残し、DashboardData の `storageError`（フェーズ2で追加済みの欄）に「n 件のパスワードをキーチェーンへ移せませんでした（DB に残しています）」と出す。次回起動で再試行する。
6. 全件が移ったら、平文の残りを消す。
   - `PRAGMA secure_delete=ON` を常時有効にする（以後、削除したページを 0 で埋める）。
   - `PRAGMA wal_checkpoint(TRUNCATE)` の後に `VACUUM` を実行する。
   - `<db>.bak-v1`、`<db>.bak-v2` には平文が残っている。**自動では消さず**、設定画面に「古いバックアップに平文のパスワードが残っています［削除］」を出す（§10-4）。
7. ロールバック: 旧版に戻すと `user_version=3 > 2` で書き込みを拒否する（既存の仕組み）。旧版で使うには `.bak-v2` を戻す。キーチェーンの項目は残っても害はない。

逆方向（キーチェーン → DB）の書き戻しは用意しません。§8 のフォールバックで新規保存が DB に落ちる場合を除き、平文を DB に戻す経路は作りません。

---

## 8. 失敗時のフォールバック

| 状況 | 検出 | 動作 |
|---|---|---|
| 保存時に Set が失敗（未対応 OS、D-Bus なし、ロック解除の拒否、タイムアウト、サイズ超過） | `ErrSecretUnavailable` / `ErrSecretTooBig` | 既定: **保存を失敗させる**（`ActionResult{OK:false, Code:"keyring_unavailable"}`）。UI は「このセッションだけ使う」か「DB に保存（0600）」を選ばせる（§10-2）。「このセッションだけ」はメモリだけに持つ |
| 読み出しで項目が無い（ユーザーがキーチェーンアクセスで消した、別マシンへ DB をコピーした） | `ErrSecretNotFound` | `Code:"needs_password"` を返し、needs_passphrase と同じ共通モーダルで再入力させる。入力された値は §5.2 の手順で保存し直す |
| 読み出しでキーチェーンが使えない・ロック中 | `ErrSecretUnavailable` | GUI 起点: `Code:"keyring_unavailable"` を返し、モーダルで一回限りの入力を許す。MCP 起点: 待たずに `error_kind:"credential_unavailable"` で失敗させる（エージェント側で再試行させる） |
| 削除時に Delete が失敗 | 任意のエラー | DB からは消し、`secret_gc` に入れて後で再試行する。接続先の削除そのものは成功させる |
| Linux の Prompt が戻らない | ctx の期限 | 包み関数が `ErrSecretUnavailable` を返す。取り残された goroutine は完了を待って捨てる。同じ account の次の呼び出しは singleflight で合流させ、goroutine を増やさない |
| 認証失敗 | SSH の認証エラー | キャッシュを捨て、パスワードが古い可能性を示して再入力を促す（キーチェーンの値は消さない） |

ロック規約: キーチェーンの呼び出しは `a.mu`、`termMu`、`pool.mu` のどれも持たずに行います（§5.0）。macOS の `security` は数十 ms、Linux はダイアログ待ちで数十秒かかりうるためです。

---

## 9. テスト方法

### 9.1 単体テスト（CI の 3 OS で常時。実キーチェーンには触れない）

- `secret_store_test.go`
  - `memorySecretStore` の Set/Get/Delete、NotFound、並行の Set/Get を `-race -count=20`。
  - `newOSSecretStore` の包み関数のタイムアウト: 内側をハングする fake に差し替え、`ctx` の期限から 50ms 以内に `ErrSecretUnavailable` で戻ること。同じ account に 10 並列で Get しても、内側の呼び出しが 1 回であること（singleflight）。
  - `passwordAccount` / `passphraseAccount` の表駆動テスト（`~` 展開の有無で同じハッシュになる、keyPath が違えば別キー、connId が違えば別キー）。
- 移行のテスト（`secret_migrate_test.go`）
  - v2 の DB に平文のパスワードを 3 件入れる → 移行 → `credential` が空、`credential_store='keyring'`、ストアに値がある。
  - Set が 2 件目で失敗する fault ストア → 1 件目だけ移り、2・3 件目は `db` のまま、`storageError` が出る。再実行で全件移る。
  - Set は成功し UPDATE の直前で落ちる（fault で panic を模す）→ 再実行して二重にならず、平文が消える。
  - **平文が DB ファイルに残らないこと**: 移行後に DB 本体と `-wal` のバイト列を読み、パスワード文字列が含まれないこと（`secure_delete` と `VACUUM` の効果の確認）。
- App のテスト（`newTestApp` で `secrets` を差し替え）
  - 保存 → MCP 実行で sshtest サーバーに password 認証で入れる。
  - ストアから項目を消す → 接続テストが `needs_password` を返す。
  - ストアを Unavailable にする → MCP 実行が `credential_unavailable` で即座に失敗し、承認キューが「中断」で永続化される。
  - キーチェーン呼び出し中に `a.mu` を持っていないこと: 内側の fake が Get の中で `a.mu.Lock()` を試み、デッドロックしないこと。

### 9.2 実キーチェーンの結合テスト（任意。ビルドタグ `keyring_integration`）

- service は `ssh-gete-test-<乱数>`、終了時に `keyring.DeleteAll(service)` で片付ける。
- macOS / Windows の GitHub Actions ランナーは、そのまま実行できる見込み（login keychain / Credential Manager がある）。
- Linux は `dbus-run-session -- sh -c 'echo -n test | gnome-keyring-daemon --unlock --components=secrets; go test -tags keyring_integration ./...'` で Secret Service を立てる（go-keyring 本家の CI と同じ方式）。
- 確かめること: 改行・非 ASCII・2000 バイトのパスワードが往復で一致する。上書きできる。無い項目の Delete が nil になる。

### 9.3 手動確認（リリース前に 1 回）

- macOS: 未署名で `wails build` → 保存 → 再ビルド → 起動して接続し、**許可ダイアログが出ない**こと。キーチェーンアクセスで `ssh-gete` の項目の名前が UUID だけであること。1 回の Get にかかる時間（目安 20〜50ms）を測る。
- Windows: 資格情報マネージャーの「Windows 資格情報 → 汎用資格情報」に `ssh-gete:conn/...` が出ること。
- Linux（GNOME）: キーリングをロックした状態で GUI から接続 → ロック解除ダイアログが出る。MCP から実行 → 5 秒以内に `credential_unavailable` で戻る。

---

## 10. ユーザーが判断すべき点

1. **macOS の隔離の強さと UX のどちらを取るか**
   - (a) go-keyring（`/usr/bin/security` 経由）。ダイアログなし。同じユーザーの他プロセスはダイアログなしで読める。（本書の推奨）
   - (b) cgo の Keychain API で「ssh-gete だけを信頼」する。他アプリからの読み出しはダイアログが必要になる。代わりに開発者 ID での署名と公証が前提になり、未署名ビルドはビルドのたびにダイアログが出る。
2. **キーチェーンが使えない環境（ヘッドレスの Linux、D-Bus なし）での保存**
   - (a) 保存を失敗させ、「このセッションだけ使う」を選ばせる。（推奨。平文を増やさない）
   - (b) 警告を出したうえで、従来どおり DB（0600）に保存できるようにする。
   - (c) 設定で「キーチェーンを使わない」を選べるようにする（既定は使う）。
3. **鍵のパスフレーズを永続化するか**
   - (a) 今のままメモリだけ（再起動のたびに入力）。
   - (b) 入力モーダルに「キーチェーンに保存」を付ける。既定は OFF。（推奨）
   - (c) 既定で保存する。
4. **移行前のバックアップ（`.bak-v1`、`.bak-v2`）に残る平文**
   - (a) 自動では消さず、設定画面に警告と［削除］ボタンを出す。（推奨）
   - (b) 移行が全件成功したら自動で消す（ロールバックの手段が減る）。
   - (c) 何もしない。
5. **MCP の Bearer トークン（`mcp_settings.token`）も同じ仕組みに移すか**
   - 同じ `SecretStore` で `mcp/token` として扱えます。ただしキーチェーンが使えないと MCP が起動できなくなるので、2 と同じ判断が要ります。
6. **移行を「次の起動で自動」にするか「ユーザーの操作で開始」にするか**
   - 自動にすると、Linux ではアップデート直後の起動でいきなりロック解除ダイアログが出ます。macOS・Windows ではダイアログは出ない見込みです。
7. **フェーズ5の前提「実測で必要になってから」をキーチェーンにも適用するか**
   - プールと違い、キーチェーンは性能ではなく秘密の置き場所の問題です。プールとは切り離して先に入れることもできます。
