# フェーズ4 シリアル仕上げパッチ仕様（terminal.go / app.go / approval.go / storage.go）

design-v1.md §7 フェーズ4 のうち「terminal.go（シリアルの setMode をその場で適用、device_removed の判定）、app.go（Serial に変えたら credential を消す、Host 一致から除外）、storage.go（シリアルの通信パラメータ列）」と、受け入れ条件「USB を抜くとタブに『デバイスが外れました［再接続］』と出る」「cu と tty の二重 open を拒否する」「シリアルの接続先では MCP の実行・承認・読取がいずれも動かない」を、そのまま実装できる粒度に落としたものです。

- 前提: フェーズ2のインターフェース契約 `docs/spec/phase2-contract.md`（以下「契約」）に書かれた形の terminal.go / app.go。フェーズ2の実体はまだ無いので、本書は契約のシンボル名・型・ロック規約だけに依存します。契約と食い違う点は §0.2 に理由つきで書き、ここに書いたものを正とします。
- 基準コミット: `a6f2706`。ライブラリ: `go.bug.st/serial v1.7.1`（go.mod）。API の記述は `$(go env GOMODCACHE)/go.bug.st/serial@v1.7.1` のソースで確認したものです（§1）。
- スコープ外: 上部サーバータブと `filterByServer`（別担当: `frontend/src/serverfilter.js`）、frontend 側のポート名正規化（別担当: `frontend/src/serialport.js`）、RTS/CTS（ライブラリに API が無いので「非対応」と明記するだけ）、MCP の list_hosts のシリアル行の項目削減（フェーズ3）。

---

## 0. まとめ

### 0.1 変更点の一覧

| ファイル | 変更 | 節 |
|---|---|---|
| `serial_port.go`（新規） | 通信パラメータの型・検証・`serial.Mode` への変換、ポートの差し替え口（テスト用）、予約キー、存在確認、エラー分類 | §2〜§5 |
| `terminal.go`（契約版） | シリアルの open（検証 → 予約 → open → エラーコード化）、読み込みループの終了理由（`device_removed` / `io_error`）、書き込みの直列化、`applySerialSettings`（その場適用）、`TerminalInfo.SerialMode` | §3〜§6 |
| `app.go` | `Connection` に `DataBits`/`Parity`/`StopBits`、`normalizeConnection`、`SaveConnection` の検証とその場適用、シリアルの `available`/`missing` のライブ判定 | §7 |
| `approval.go` | `approve` の入口でシリアル宛ての承認を拒否（状態は変えない） | §7.5 |
| `storage.go` | `data_bits` / `parity` / `stop_bits` 列、読み書き | §8 |
| `serial_port_test.go`（新規）、`terminal_serial_test.go`（新規）、`app_serial_test.go`（新規）、`storage_test.go`（追記） | §9 のテスト | §9 |

### 0.2 契約からの変更（ここを正とする）

| # | 契約の記述 | 本書 | 理由 |
|---|---|---|---|
| D1 | §1.4「シリアル: 読み込みエラーのとき、`serial.GetPortsList()` に正規化後のポート名が無ければ `device_removed`」 | 存在確認は `serialPortPresent(goos, name)`（§5.2）で行う。darwin/linux は `os.Stat`、windows だけ `serial.GetPortsList()` | linux の `GetPortsList` は `ttyS*` / `ttyHS*` を**実際に open して**実在を確かめる（`serial_unix.go` の `nativeGetPortsList`）。open は DTR を立てて基板をリセットし得るので、切断判定や状態表示のたびに呼べない。darwin の `GetPortsList` は `/dev` の ReadDir と同じなので `os.Stat` で等価 |
| D2 | §1.2 StartTerminal の失敗コードは `connect_failed` に集約 | シリアルは `invalid_serial_mode` / `port_in_use` / `port_busy` / `permission_denied` / `port_not_found` / `open_failed` に分ける（§3.4） | 「他のアプリが使用中」と「権限が無い」は利用者の対処が違う |
| D3 | §1.3 `TerminalInfo` | `SerialMode string \`json:"serialMode,omitempty"\`` を末尾に追加（例 `"8N1"`） | その場適用の結果をタブとサイドバーに出すため。omitempty なので既存の受け手は壊れない |
| D4 | §1.5 予約表のキーは `normalizeSerialPort(runtime.GOOS, port)` | キーは `serialReservationKey(goos, port)`（§4.1）= 正規化 → シンボリックリンク解決 → 再正規化 | linux の `/dev/serial/by-id/...` と `/dev/ttyUSB0` を同じデバイスとして扱う |
| D5 | §1.1 ロックは4種類 | シリアルセッションごとの `serialHandle.writeMu` を末端ロックとして追加し5種類 | 入力 goroutine の Write と、その場適用の Drain+SetMode を直列化するため |
| D6 | §1.4 `testSerialConnection` は契約で未変更 | 存在確認を §5.2 に、使用中判定を予約キーでの全セッション検索に替える（§7.4） | D1 と同じ。別の接続先が同じデバイスを開いている場合も「使用中」にする |
| D7 | design-v1 §5.5 の列名 `baud` | 既存の `baud_rate` 列をそのまま使う | 既存 DB の列名変更は不要な移行になる |

### 0.3 ロック規約の追補（契約 §1.1 に追加）

- `serialHandle.writeMu` は末端ロック。持っている間は `termMu` / `a.mu` / `outputBuffer.mu` を取らず、`emitEvent` / `sessionsChanged` を呼ばない。持ってよいのは `port.Write` / `port.Drain` / `port.SetMode` の呼び出しの間だけ。
- `serialHandle.settings` と `serialHandle.pending` は `termMu` で保護する（読み書きとも）。
- `serialEndReason` の存在確認（sleep を含む）は、どのロックも持たずに行う。

---

## 1. go.bug.st/serial v1.7.1 で確認した事実（設計の根拠）

| # | 事実 | ソース | 設計への影響 |
|---|---|---|---|
| L1 | `Port` interface: `SetMode(*Mode) error`、`Read`、`Write`、`Drain() error`、`ResetInputBuffer`、`ResetOutputBuffer`、`SetDTR`、`SetRTS`、`GetModemStatusBits`、`SetReadTimeout`、`Close`、`Break` | `serial.go` | その場適用は `SetMode` で可能。RTS/CTS フロー制御を有効にする API は無い |
| L2 | `Mode{BaudRate int; DataBits int; Parity Parity; StopBits StopBits; InitialStatusBits *ModemOutputBits}`。`Parity`: `NoParity, OddParity, EvenParity, MarkParity, SpaceParity`。`StopBits`: `OneStopBit, OnePointFiveStopBits, TwoStopBits` | `serial.go` | §2 の型の対応表 |
| L3 | `Open` の失敗は `*PortError`（値レシーバの `Error()` / `Code()`、**`Unwrap` は無い**）か生の `syscall.Errno`。unix: `EBUSY→PortBusy`、`EACCES→PermissionDenied`、それ以外（`ENOENT` など）は生の errno。windows: `ERROR_ACCESS_DENIED→PortBusy`、`ERROR_FILE_NOT_FOUND→PortNotFound` | `serial_unix.go` `nativeOpen`、`serial_windows.go` `nativeOpen` | §3.4 のエラーコード表。`errors.As(err, &pe)` の target は `*serial.PortError` |
| L4 | unix の `Open` は open 後に `TIOCEXCL` を設定する（エラーは無視）。他プロセスからの同じ tty の open は `EBUSY` → `PortBusy` | `serial_unix.go` `acquireExclusiveAccess` | プロセス外の二重 open は OS が拒否する。アプリ内の cu/tty 二重 open は予約表で拒否する（TIOCEXCL は root には効かず、cu/tty の別ノードでの挙動を当てにしない） |
| L5 | unix の `SetMode`: termios を1回取得 → parity / databits / stopbits / baud を書き換え → `TIOCSETA`(darwin) / `TCSETS`(linux) で1回設定。表に無い baud は設定後に特殊ボーレート ioctl（darwin `IOSSIOSPEED`、linux `TCSETS2`+`BOTHER`）を追加で呼ぶ | `serial_unix.go`、`serial_darwin.go`、`serial_specialbaudrate_linux.go` | 失敗しても port は開いたまま。特殊ボーレートの段で失敗すると termios だけ変わった中間状態になり得るので、失敗時は旧設定で `SetMode` し直す（ロールバック） |
| L6 | unix: `DataBits` の 0 は 8 扱い（`databitsMap[0]=CS8`）、5〜8 のみ有効。`OnePointFiveStopBits` は常に `InvalidStopBits`。darwin/freebsd/openbsd は `tcCMSPAR == 0` なので Mark/Space は `InvalidParity` | `serial_unix.go`、`serial_darwin_64.go`、`serial_linux.go` | §2.3 の OS 別検証表 |
| L7 | **windows の `SetMode` は `GetCommState`/`SetCommState` に失敗すると `port.Close()` してから `InvalidSerialPort` を返す**。`setModeParams` は `stopBitsMap` / `parityMap` を素引きするので、範囲外の値は黙って 0（1ストップ / パリティなし）になる | `serial_windows.go` `SetMode` / `setModeParams` | windows では失敗＝ポート喪失。**SetMode を呼ぶ前に必ず検証する**（§2.3）。それでも失敗したらセッションを `io_error` で閉じる（§6） |
| L8 | windows の DCB は「5 データビット＋2 ストップ」「6〜8 データビット＋1.5 ストップ」を拒否する（`SetCommState` が失敗） | Win32 DCB の仕様、L7 | §2.3 で事前に弾く（L7 によりポートを失わないため） |
| L9 | `SetMode` は DTR/RTS を変えない（unix は termios の CSIZE/PARENB/CSTOPB/速度だけ、windows は BaudRate/ByteSize/StopBits/Parity だけ） | 同上 | その場適用で基板はリセットされない。ここが「再接続ではなくその場適用」にする理由 |
| L10 | unix の `Read`: select で待ち、`unix.Read` の結果をそのまま返す。**n==0 かつ err==nil は `PortError{PortClosed}`**（linux で USB を抜いたときの状態）。自分で `Close` したときも `PortClosed`。errno は `syscall.Errno` のまま（ラップされない） | `serial_unix.go` `Read` | §5.3 の分類表。自分の Close と区別するため `closing` フラグを先に見る |
| L11 | windows の `Read`: `ERROR_OPERATION_ABORTED` は `PortError{PortClosed, causedBy}`、それ以外の失敗は生の `windows.Errno`（= `syscall.Errno`） | `serial_windows.go` `Read` | 同上 |
| L12 | `Drain`: darwin `TIOCDRAIN`、linux `TCSBRK(1)`（= tcdrain）、windows `FlushFileBuffers` | `serial_bsd.go`、`serial_linux.go`、`serial_windows.go` | その場適用の前に送信中のバイトを出し切る。抜けたデバイスで戻らない可能性に備えてタイムアウトを付ける |
| L13 | `GetPortsList`: darwin は `/dev` の `^(cu|tty)\..*`、linux は `/dev` を正規表現で列挙し `ttyS*`/`ttyHS*` は open して確認、windows は `HKLM\HARDWARE\DEVICEMAP\SERIALCOMM` | `serial_unix.go`、`serial_darwin.go`、`serial_linux.go`、`serial_windows.go` | D1 |
| L14 | windows の `Open` は名前に `\\.\` が無ければ付ける | `serial_windows.go` `nativeOpen` | 正規化後の `COM10` をそのまま渡してよい |

---

## 2. 通信パラメータ（新規 `serial_port.go`）

### 2.1 型

```go
// serialSettings is the line configuration of a serial connection. Parity and
// StopBits are strings so the DB, JSON and UI share one vocabulary.
type serialSettings struct {
	Baud     int    // 50..4000000
	DataBits int    // 5..8
	Parity   string // "none" | "odd" | "even" | "mark" | "space"
	StopBits string // "1" | "1.5" | "2"
}

const (
	defaultSerialBaud     = 115200
	defaultSerialDataBits = 8
	defaultSerialParity   = "none"
	defaultSerialStopBits = "1"
	minSerialBaud         = 50
	maxSerialBaud         = 4_000_000
)

// serialModeError is returned by validateSerialSettings; the UI shows Message
// next to Field.
type serialModeError struct {
	Field   string // "baudRate" | "dataBits" | "parity" | "stopBits"（Connection の json 名）
	Message string // 日本語
}

func (e *serialModeError) Error() string { return e.Message }

func serialSettingsOf(conn Connection) serialSettings
func validateSerialSettings(goos string, s serialSettings) error // nil か *serialModeError
func (s serialSettings) mode() *serial.Mode
func (s serialSettings) label() string // "8N1" / "7E1" / "8N2" / "5N1.5"
```

### 2.2 既定値と変換

`serialSettingsOf(conn)`:

```
s.Baud     = conn.BaudRate;  if s.Baud <= 0 { s.Baud = 115200 }
s.DataBits = conn.DataBits;  if s.DataBits == 0 { s.DataBits = 8 }
s.Parity   = lower(trim(conn.Parity));   if s.Parity == ""   { s.Parity = "none" }
s.StopBits = trim(conn.StopBits);        if s.StopBits == "" { s.StopBits = "1" }
```

`mode()`（検証済みの値だけを受ける。未知の値は panic ではなく既定値に落とす）:

| parity | `serial.Parity` | stopBits | `serial.StopBits` |
|---|---|---|---|
| none | `serial.NoParity` | 1 | `serial.OneStopBit` |
| odd | `serial.OddParity` | 1.5 | `serial.OnePointFiveStopBits` |
| even | `serial.EvenParity` | 2 | `serial.TwoStopBits` |
| mark | `serial.MarkParity` | | |
| space | `serial.SpaceParity` | | |

`InitialStatusBits` は nil のまま（ライブラリ既定 DTR=RTS=true。今の挙動を変えない）。

`label()`: `fmt.Sprintf("%d%s%s", DataBits, 頭文字大文字(N/O/E/M/S), StopBits)`。

### 2.3 検証表（`validateSerialSettings`。上から順に最初に当たったエラーを返す）

| # | 条件 | darwin | linux | windows | Field | Message |
|---|---|---|---|---|---|---|
| V1 | `Baud < 50 \|\| Baud > 4000000` | NG | NG | NG | baudRate | `ボーレートは 50〜4000000 の範囲で指定してください` |
| V2 | `DataBits` が 5,6,7,8 以外 | NG | NG | NG | dataBits | `データビットは 5〜8 で指定してください` |
| V3 | `Parity` が none/odd/even/mark/space 以外 | NG | NG | NG | parity | `パリティは none / odd / even / mark / space のいずれかです` |
| V4 | `Parity` が mark か space | NG | OK | OK | parity | `この OS ではマーク/スペースパリティを使えません` |
| V5 | `StopBits` が 1 / 1.5 / 2 以外 | NG | NG | NG | stopBits | `ストップビットは 1 / 1.5 / 2 のいずれかです` |
| V6 | `StopBits == "1.5"` | NG | NG | （V7へ） | stopBits | `この OS では 1.5 ストップビットを使えません` |
| V7 | windows で `StopBits=="1.5" && DataBits!=5` | — | — | NG | stopBits | `1.5 ストップビットはデータビット 5 のときだけ使えます` |
| V8 | windows で `DataBits==5 && StopBits=="2"` | — | — | NG | stopBits | `データビット 5 では 2 ストップビットを使えません（1.5 を選んでください）` |

- `goos` が darwin/linux/windows 以外（freebsd/openbsd）は darwin と同じ規則（`tcCMSPAR==0`）。
- 標準外の baud（例 250000、1000000）は darwin/linux とも特殊ボーレート ioctl で通るので許可する（L5）。デバイスが受け付けない場合は open / SetMode の失敗として扱う。

---

## 3. シリアルの open（terminal.go、契約の StartTerminal に差し込む）

### 3.1 ポートの差し替え口（`serial_port.go`）

```go
// serialPort is the subset of serial.Port the terminal uses; tests swap in a fake.
type serialPort interface {
	io.ReadWriteCloser
	SetMode(mode *serial.Mode) error
	Drain() error
}

var (
	// openSerialPort opens a port. serial.Open returns a nil interface on error,
	// so the conversion below never yields a non-nil serialPort with a nil value.
	openSerialPort = func(name string, mode *serial.Mode) (serialPort, error) {
		p, err := serial.Open(name, mode)
		if err != nil {
			return nil, err
		}
		return p, nil
	}
	serialPortPresent   = defaultSerialPortPresent          // §5.2
	evalSerialSymlinks  = filepath.EvalSymlinks             // §4.1
	serialRemovalProbes = []time.Duration{0, 100 * time.Millisecond, 250 * time.Millisecond, 500 * time.Millisecond}
	serialDrainTimeout  = 500 * time.Millisecond
)
```

### 3.2 セッションに持たせるもの（契約 §1.7 の `terminalSession` に追加）

```go
type terminalSession struct {
	// …契約 §1.7 のフィールド…
	closing atomic.Bool   // close() の Once に入った瞬間に true。読み込みループが自分の Close を判別する
	serial  *serialHandle // kind=="serial" のときだけ非 nil
}

// serialHandle owns the open port of one serial tab.
type serialHandle struct {
	port     serialPort
	key      string          // serialReservationKey。serialOwners のキーと同じ
	writeMu  sync.Mutex      // 末端ロック。Write と Drain+SetMode を直列化する
	settings serialSettings  // termMu で保護。現在ポートに入っている設定
	pending  *serialSettings // termMu で保護。connecting 中に届いたその場適用
}

// Write is the session's stdin for serial; the input goroutine calls it.
func (h *serialHandle) Write(p []byte) (int, error) {
	h.writeMu.Lock()
	defer h.writeMu.Unlock()
	return h.port.Write(p)
}
```

契約 §1.7 の `close(reason, exitCode, message)` の Once の**先頭**で `t.closing.Store(true)` を行う。closer は `func() { _ = h.port.Close() }`。

### 3.3 StartTerminal のシリアル分岐（契約 §1.2 の手順に対する差分）

```
StartTerminal(id, connID, cols, rows):
  1〜3. 契約どおり（id 検証、connectionByID、サイズ丸め）
  3a. [追加] conn.connectionType()=="Serial" なら（ロックの外で）
        portName := conn.SerialPort; if portName == "" → OK:false, Code:"invalid_serial_mode",
                    Message:"シリアルポートが設定されていません"
        settings := serialSettingsOf(conn)
        if err := validateSerialSettings(runtime.GOOS, settings); err != nil →
             OK:false, Code:"invalid_serial_mode", Message: err.Error(), SessionID:id, ID:connID
        key := serialReservationKey(runtime.GOOS, portName)      // os 呼び出しがあるのでロックの外
  4. termMu の下（契約どおり）。予約表は key で引く:
        if owner, ok := serialOwners[key]; ok {
             other := terms[owner]                                // connName を取るだけ
             → OK:false, Code:"port_in_use",
               Message: fmt.Sprintf("%s は「%s」のターミナルで使用中です", portName, other.connName)
               （portName と other.port の正規化前の文字列が違えば「（%s と同じデバイス）」を足す）
        }
        serialOwners[key] = id
        terms[id] = &terminalSession{…, kind:"serial", port: portName, baud: settings.Baud, status:"connecting",
                                     serial: &serialHandle{key: key, settings: settings}}
  5. 解放後に sessionsChanged()
  6. [シリアル] port, err := openSerialPort(normalizeSerialPort(runtime.GOOS, portName), settings.mode())
        ※ open は ctx でキャンセルできない（ブロッキング syscall。O_NDELAY で開くので通常は即時に戻る）。
          戻った後に ctx.Err() != nil なら port.Close() して Code:"canceled"。
  7. 失敗: code, msg := serialOpenError(runtime.GOOS, portName, err)（§3.4）。
        プレースホルダと serialOwners[key] を外す（termMu）→ lastErr[connID]=msg → sessionsChanged()
        → OK:false, Code:code, Message:msg, SessionID:id, ID:connID
  8. 成功: termMu の下で
        h := sess.serial; h.port = port
        if h.pending != nil { apply := *h.pending; h.pending = nil } // 解放後に §6 の applyNow(h, apply)
        sess.stdin = h（serialHandle.Write）; sess.closer = func(){ _ = port.Close() }
        status="open"; startedAt 確定; delete(lastErr, connID)
      解放後: go a.serialReadLoop(sess)（§5.1）; apply があれば a.applyNow(sess, apply)（§6.2）; sessionsChanged()
      → OK:true, Message: fmt.Sprintf("%s @ %d %s に接続しました", portName, settings.Baud, settings.label())
```

`TerminalInfo` の組み立て（契約 §1.5 `listTerminalInfos`）に `SerialMode: h.settings.label()` を足す（termMu の下で読む）。`Baud` は `h.settings.Baud`。

### 3.4 open のエラーコード（`serialOpenError(goos, portName string, err error) (code, message string)`）

上から順に判定する。`var pe *serial.PortError; errors.As(err, &pe)`（L3）、`var errno syscall.Errno; errors.As(err, &errno)`。

| # | 条件 | Code | Message |
|---|---|---|---|
| E1 | `pe.Code()==serial.PortBusy` | `port_busy` | `%s は他のアプリ（screen / minicom / Arduino IDE など）が使用中です` |
| E2 | `pe.Code()==serial.PermissionDenied`、または errno が `EACCES`(13) / `EPERM`(1)（unix） | `permission_denied` | linux: `%s を開く権限がありません（dialout グループへの追加が必要な場合があります）`。それ以外: `%s を開く権限がありません` |
| E3 | `pe.Code()==serial.PortNotFound`、または `errors.Is(err, fs.ErrNotExist)`、または unix で errno が `ENOENT`(2) / `ENXIO`(6) / `ENODEV`(19) | `port_not_found` | `%s が見つかりません。ケーブルの接続を確認してください` |
| E4 | `pe.Code()` が `InvalidSpeed` / `InvalidDataBits` / `InvalidParity` / `InvalidStopBits` | `invalid_serial_mode` | `%s の通信設定がこの OS / デバイスで使えません: %v` |
| E5 | `pe.Code()==serial.InvalidSerialPort` | `open_failed` | `%s はシリアルデバイスとして開けません: %v`（ライブラリは open 中の SetMode 失敗もここに包む） |
| E6 | それ以外 | `open_failed` | `%s を開けません: %v` |

windows の `ERROR_ACCESS_DENIED` はライブラリが `PortBusy` に変換する（L3）ので E1 になる。windows で errno 5 を E2 として扱ってはいけない（E2 の errno 判定は unix のみ）。

---

## 4. cu / tty の二重 open 拒否

### 4.1 予約キー（`serial_port.go`）

```go
// serialReservationKey identifies the physical device behind a port name so
// /dev/tty.X and /dev/cu.X (darwin), COM3 and \\.\com3 (windows) and a
// /dev/serial/by-id symlink and its target (linux) share one reservation.
func serialReservationKey(goos string, name string) string {
	key := normalizeSerialPort(goos, strings.TrimSpace(name)) // 契約 §1.5（B1）
	if goos == "windows" || key == "" {
		return key
	}
	if resolved, err := evalSerialSymlinks(key); err == nil && resolved != "" {
		key = normalizeSerialPort(goos, resolved)
	}
	return key
}
```

- `normalizeSerialPort` 自体は契約のまま（darwin: `/dev/tty.X → /dev/cu.X`、windows: `\\.\` を外して大文字、その他はそのまま）。本書はそれを包むだけ。
- シンボリックリンクが壊れている（デバイスが無い）ときは `EvalSymlinks` が失敗するので正規化だけのキーになる。open も失敗するので実害はない。
- frontend の `serialport.js`（別担当）は表示と一覧照合のための正規化で、予約の正はこの Go 側。§9.1 の正規化ベクタ（N1〜N10）は `serialport.js` のテストと同じ値にそろえる（Integrator が突き合わせる）。

### 4.2 拒否の層

| 層 | 何を防ぐか | 仕組み | 戻り |
|---|---|---|---|
| アプリ内（一次） | 同じデバイスを2タブで開く（cu/tty、別名、同じ接続先の2タブ目、別の接続先が同じデバイス） | `serialOwners[serialReservationKey]` を StartTerminal の termMu 内で予約（connecting の時点で予約済み） | `port_in_use` |
| プロセス間（二次） | screen などが先に開いている／後から開こうとする | ライブラリの `TIOCEXCL`（unix）、排他 `CreateFile`（windows）→ `PortBusy` | `port_busy`（E1） |

予約の解除は契約 §1.7 の close 手順（`terms` 削除 → 予約表の解除）と、StartTerminal の失敗時（§3.3 手順7）の2か所だけ。

---

## 5. 切断の判定（device_removed / io_error）

### 5.1 読み込みループ（terminal.go）

```go
func (a *App) serialReadLoop(sess *terminalSession) {
	h := sess.serial
	buf := make([]byte, 4096)
	for {
		n, err := h.port.Read(buf)
		if n > 0 {
			sess.out.write(buf[:n]) // 契約 §1.7 の outputBuffer
		}
		if err == nil {
			continue // windows の Read はタイムアウト無しなら n==0 で戻らない。unix も NoTimeout
		}
		if sess.closing.Load() {
			return // user / io_error(SetMode 失敗) などで close 済み。Close による PortClosed を誤判定しない
		}
		reason, message := serialEndReason(runtime.GOOS, sess.port, err)
		sess.close(reason, nil, message) // Once。先に user が入っていれば契約どおり user が勝つ
		return
	}
}
```

入力 goroutine（契約 §1.2 SendTerminalInput）の書き込み失敗も、シリアルでは `io_error` 固定ではなく `serialEndReason(runtime.GOOS, sess.port, err)` で判定する（抜いた直後は書き込み側が先に `ENXIO`/`EIO` を受けることがあるため）。

### 5.2 存在確認（`serial_port.go`）

```go
// defaultSerialPortPresent reports whether the device behind name still exists
// without opening it (opening toggles DTR; see D1).
func defaultSerialPortPresent(goos string, name string) (bool, error) {
	switch goos {
	case "windows":
		ports, err := serial.GetPortsList() // レジストリ SERIALCOMM。open しない
		if err != nil {
			return false, err
		}
		want := normalizeSerialPort(goos, name)
		for _, p := range ports {
			if normalizeSerialPort(goos, p) == want {
				return true, nil
			}
		}
		return false, nil
	default: // darwin, linux, *bsd
		_, err := os.Stat(normalizeSerialPort(goos, name)) // リンクは辿る
		if err == nil {
			return true, nil
		}
		if errors.Is(err, fs.ErrNotExist) {
			return false, nil
		}
		return false, err
	}
}
```

darwin は USB を抜くと `/dev/cu.X` と `/dev/tty.X` の両方が消える。linux は `/dev/ttyUSB0` / `/dev/ttyACM0` と `/dev/serial/by-id/*` が消える（udev が少し遅れることがあるので §5.3 で再試行する）。windows は SERIALCOMM から値が消える。

### 5.3 エラー分類と終了理由

```go
type serialIOClass int

const (
	serialIOGeneric serialIOClass = iota // 抜去の兆候なし
	serialIORemoved                      // 抜去でよく見る種類
)

// classifySerialIOError is pure (goos is a parameter) so every row of §9.3
// runs on any CI OS. Errno values are compared numerically.
func classifySerialIOError(goos string, err error) serialIOClass

// serialEndReason decides device_removed vs io_error after a read/write
// failure. It sleeps between probes and must be called without locks.
func serialEndReason(goos string, portName string, err error) (reason string, message string) {
	class := classifySerialIOError(goos, err)
	probes := serialRemovalProbes
	if class == serialIOGeneric {
		probes = probes[:1] // 兆候が無いときは即時に1回だけ確かめる
	}
	probeFailed := 0
	for _, d := range probes {
		if d > 0 {
			time.Sleep(d)
		}
		present, perr := serialPortPresent(goos, portName)
		if perr != nil {
			probeFailed++
			continue
		}
		if !present {
			return "device_removed", fmt.Sprintf("%s が外れました。ケーブルを挿し直してから再接続してください", portName)
		}
	}
	if class == serialIORemoved && probeFailed == len(probes) {
		// 列挙そのものができない（windows のレジストリ不可など）。兆候を信じる
		return "device_removed", fmt.Sprintf("%s が外れました。ケーブルを挿し直してから再接続してください", portName)
	}
	return "io_error", fmt.Sprintf("%s の通信でエラーが発生しました: %v", portName, err)
}
```

`classifySerialIOError` の表（上から順。`pe` / `errno` の取り方は §3.4 と同じ）:

| # | goos | エラー | 分類 | 典型的に出る場面 |
|---|---|---|---|---|
| C1 | 全部 | `*serial.PortError` で `Code()==serial.PortClosed` | Removed | linux: 抜去で「読めるが 0 バイト」（L10）。windows: `ERROR_OPERATION_ABORTED`（L11）。自分の Close は §5.1 の `closing` で先に除外済み |
| C2 | 全部 | `errors.Is(err, io.EOF)` | Removed | 念のため（ライブラリは返さないが、fake や将来の変更に備える） |
| C3 | darwin / *bsd | errno `ENXIO`(6) | Removed | macOS で USB シリアルを抜いた直後の read/write（"device not configured"） |
| C4 | darwin / *bsd | errno `EIO`(5)、`ENODEV`(19) | Removed | ドライバによっては EIO |
| C5 | linux | errno `EIO`(5)、`ENODEV`(19)、`ENXIO`(6) | Removed | 抜去後の write、cdc_acm の read |
| C6 | windows | errno `ERROR_ACCESS_DENIED`(5)、`ERROR_INVALID_HANDLE`(6)、`ERROR_BAD_COMMAND`(22)、`ERROR_GEN_FAILURE`(31)、`ERROR_DEVICE_NOT_CONNECTED`(1167)、`ERROR_DEVICE_REMOVED`(1617)、`ERROR_OPERATION_ABORTED`(995) | Removed | FTDI / CH340 / CP210x の VCP ドライバで抜去時に返る値 |
| C7 | 全部 | それ以外（`EBADF`、`EINTR` 以外の select 失敗、未知の errno、文字列だけのエラー） | Generic | |

判定結果の組み合わせ（`serialEndReason`）:

| 分類 | 存在確認の結果 | reason |
|---|---|---|
| Removed | いずれかの回で「無い」 | `device_removed` |
| Removed | 全回「ある」 | `io_error`（抜去ではなくドライバ側の失敗。再接続で直ることが多い） |
| Removed | 全回エラー | `device_removed` |
| Generic | 1回目で「無い」 | `device_removed` |
| Generic | 「ある」またはエラー | `io_error` |

`device_removed` のとき frontend は「デバイスが外れました［再接続］」を出す（§10）。`lastErr[connID]` には message を入れる（契約 §1.4 の「user 以外で終わり open タブが無ければ記録」に従う）。

---

## 6. setMode のその場適用

### 6.1 公開関数（terminal.go。B2 側の app.go から呼ぶ。Wails バインドはしない）

```go
// applySerialSettings pushes new line settings to the open serial tab of
// connID without reopening the port (no DTR toggle, so the board is not reset).
// It reports whether a tab was updated. Settings must already be validated.
func (a *App) applySerialSettings(connID string, s serialSettings) (applied bool, err error)
```

### 6.2 擬似コード

```
applySerialSettings(connID, s):
  termMu.Lock(); ensureMaps()
  var sess *terminalSession
  for _, t := range terms { if t.connID == connID && t.kind == "serial" { sess = t; break } }
     // シリアルはポート予約で1接続先1タブまでなので最初の1件で足りる
  if sess == nil { termMu.Unlock(); return false, nil }
  if sess.status == "connecting" { cp := s; sess.serial.pending = &cp; termMu.Unlock(); return true, nil }
  termMu.Unlock()
  return a.applyNow(sess, s)

applyNow(sess, s):
  h := sess.serial
  termMu.Lock(); prev := h.settings; termMu.Unlock()
  if prev == s { return true, nil }
  h.writeMu.Lock()
  drainWithTimeout(h.port, serialDrainTimeout)   // 送信待ちを出し切る。タイムアウトしても続行（エラーは無視）
  err := h.port.SetMode(s.mode())
  if err != nil && runtime.GOOS != "windows" {
      _ = h.port.SetMode(prev.mode())            // L5 の中間状態からのロールバック（失敗しても無視）
  }
  h.writeMu.Unlock()
  if err != nil {
      if runtime.GOOS == "windows" {             // L7: ポートは既に閉じられている
          sess.close("io_error", nil, fmt.Sprintf("%s の通信設定を変更できず、ポートが閉じられました: %v", sess.port, err))
      }
      return false, err
  }
  termMu.Lock(); h.settings = s; sess.baud = s.Baud; termMu.Unlock()
  a.sessionsChanged()
  return true, nil

drainWithTimeout(p, d):
  done := make(chan struct{})
  go func(){ _ = p.Drain(); close(done) }()
  select { case <-done: case <-time.After(d): }
```

- 受信側（読み込みループ）は止めない。SetMode と並行して `Read` が select で待っていても問題ない（別 ioctl / 別 API）。設定の切り替わりの瞬間に受けた数バイトが化けるのは仕様とする。
- `runtime.GOOS` はテストで windows 分岐を通せないので、`applyNow` の中では `serialGOOS`（`var serialGOOS = runtime.GOOS`）を使い、テストで差し替える。§3.3・§5.1 の `runtime.GOOS` も同じ変数にそろえる。

---

## 7. app.go / approval.go の変更

### 7.1 Connection

```go
type Connection struct {
	// …既存…
	SerialPort string `json:"serialPort"`
	BaudRate   int    `json:"baudRate"`
	DataBits   int    `json:"dataBits"` // 追加。0 は 8
	Parity     string `json:"parity"`   // 追加。"" は none
	StopBits   string `json:"stopBits"` // 追加。"" は "1"
	// …既存…
}
```

`sameTarget` は**変えない**（通信パラメータは「どの機器か」ではないので、パリティだけ変えても lastTest とパスフレーズのキャッシュは有効のまま）。

### 7.2 normalizeConnection

```
… 既存 …
connection.Parity = strings.ToLower(strings.TrimSpace(connection.Parity))
connection.StopBits = strings.TrimSpace(connection.StopBits)
if connection.Type == "Serial" {
	if connection.BaudRate <= 0 { connection.BaudRate = 115200 }   // 既存
	if connection.DataBits == 0 { connection.DataBits = 8 }
	if connection.Parity == "" { connection.Parity = "none" }
	if connection.StopBits == "" { connection.StopBits = "1" }
} else {
	// SSH に戻したら シリアルの値を残さない（幽霊値の防止。§4 の Medium バグと対になる）
	connection.SerialPort, connection.BaudRate = "", 0
	connection.DataBits, connection.Parity, connection.StopBits = 0, "", ""
}
```

「Serial に変えたら credential を消す」（`SaveConnection` の `connection.Host, connection.User, connection.Credential = "", "", ""`）と「Host 一致からシリアルを除外」（`resolveConnection` の `conn.connectionType() == "SSH"`）は `a6f2706` で実装済み。フェーズ4では回帰テストだけを足す（§9.4 A1〜A3）。

### 7.3 SaveConnection

`a.mu` を `defer` で解放しているので、その場適用（termMu を取る）を呼べるように本体を分ける。

```go
func (a *App) SaveConnection(connection Connection) ActionResult {
	result, post := a.saveConnection(connection) // 既存の本体。a.mu の取得〜解放はこの中で完結
	if post != nil {
		result = post(result) // a.mu を持たずに呼ぶ
	}
	return result
}

// saveConnection is the former SaveConnection body; post (may be nil) runs
// after a.mu is released.
func (a *App) saveConnection(connection Connection) (ActionResult, func(ActionResult) ActionResult)
```

`saveConnection` への追加:

```
connection = normalizeConnection(connection)
… 既存の必須チェック …
if connection.Type == "Serial" {
	if err := validateSerialSettings(serialGOOS, serialSettingsOf(connection)); err != nil {
		return ActionResult{OK:false, Code:"invalid_serial_mode", Message: err.Error(), ID: connection.ID}, nil
	}
}
… 既存の保存（DB → メモリ）…
（更新のとき、a.mu を持っている間に previous と connection を写しておき）
post = func(r ActionResult) ActionResult { return a.afterSerialSave(previous, connection, r) }
```

注: `serialModeError.Field` は Go 内部とテストでだけ使い、`ActionResult` にはフィールドを足さない（Wails の生成物を変えないため）。§2.3 の Message はすべて項目名を含むので、UI は Message をそのまま出せばよい。

```go
// afterSerialSave applies saved line settings to an open serial tab, or tells
// the UI that the tab still uses the old port/type.
func (a *App) afterSerialSave(prev Connection, next Connection, r ActionResult) ActionResult
```

| # | prev → next | 開いているシリアルタブ | 動作 | 戻り値の変化（`OK:true` は維持） |
|---|---|---|---|---|
| S1 | Serial → Serial、予約キー同じ、設定同じ | 問わない | 何もしない | なし |
| S2 | Serial → Serial、予約キー同じ、設定違う | あり（open） | `applySerialSettings` 成功 | Message に `（開いているターミナルに 9600 7E1 を適用しました）` を足す、`Code:"serial_mode_applied"` |
| S3 | 同上 | あり（connecting） | `pending` に積む（§6.2） | S2 と同じ |
| S4 | 同上 | あり | `applySerialSettings` 失敗（unix） | Message に `（開いているターミナルには適用できませんでした: %v。再接続すると反映されます）`、`Code:"serial_mode_not_applied"` |
| S5 | 同上 | あり | 失敗（windows。タブは `io_error` で閉じる） | S4 と同じ文言の末尾を `ターミナルは閉じられました` に。`Code:"serial_mode_not_applied"` |
| S6 | 同上 | なし | 何もしない | なし |
| S7 | Serial → Serial、予約キー違う | あり | 何もしない（タブは旧ポートのまま） | Message に `（開いているターミナルは旧ポート %s のままです。再接続すると反映されます）`、`Code:"reconnect_required"` |
| S8 | Serial → SSH、または SSH → Serial | あり（どの kind でも） | 何もしない（契約「SaveConnection はタブを閉じない」） | Message に `（開いているターミナルは変更前の設定のままです）`、`Code:"reconnect_required"` |

「開いているタブ」の判定は `a.terminalRuntime()[conn.ID]`（契約 §1.5）の `Open+Connecting > 0`。S7/S8 で `reconnect_required` を出すのは prev/next のどちらかがシリアルのときだけ（SSH → SSH の設定変更は既存どおり何も足さない）。

### 7.4 シリアルのライブ状態（契約 §1.3 の ConnState 表の「それ以外」行を置き換える）

| 条件 | シリアル |
|---|---|
| `Shells > 0` | `in_use` |
| `Connecting > 0` | `connecting` |
| `LastError != ""` | `error` |
| 他の接続先のタブが同じ予約キーを持つ | `in_use` |
| `serialPresence` で存在する | `available` |
| 存在しない | `missing` |
| 存在確認がエラー | `disconnected` |

```go
// serialPresence caches serialPortPresent per reservation key for
// serialPresenceTTL so sessionSummaries stays cheap. Guarded by its own
// presenceMu (末端ロック。持ったまま serialPortPresent を呼ばない：
// 期限切れのキーを写す → 解放 → 確認 → 取り直して書く).
var serialPresenceTTL = 2 * time.Second

func (a *App) serialPresence(goos string, ports []string) map[string]bool // key: serialReservationKey
```

- `sessionSummaries` は `a.mu` を解放した後（契約の順）にシリアルの接続先のポート名を集めて1回だけ呼ぶ。
- 抜き差しを画面に出すため、`startup` で `go a.watchSerialPresence(a.appCtx)` を起動する: 3秒ごとに `serialPresence` を取り、前回と集合が変わったときだけ `a.sessionsChanged()`。シリアルの接続先が0件なら何もしない。
- **抜いた後に挿し直したときの `lastErr`**: 上の表では `LastError` が `available`/`missing` より先に効き、`lastErr` は契約上 open 成功でしか消えない。そのままだと P10（`device_removed` で `lastErr` に記録）の後に挿し直しても `error` のままになり、ライブ判定が意味を持たない。そこで `watchSerialPresence` は、前回 missing（または前回の集合に無い）で今回 present になった予約キーについて、そのキーを持つシリアルの接続先の `lastErr[connID]` を消してから `a.sessionsChanged()` する（`a.mu` を取って消す。presenceMu は持たない）。present → missing の変化では消さない。SSH の接続先の `lastErr` は触らない。
- `testSerialConnection`（既存）は判定を次に替える: 使用中は `serialPortInUse` を「同じ予約キーを持つ任意のセッション」で判定（Message に相手の接続先名）、存在は `serialPortPresent`。`GetPortsList` は使わない（D1、D6）。戻りの Code は `in_use` / `available` / `missing`（OK はそれぞれ true / true / false）。

`serialPortInUse` の契約上のシグネチャ（`func (a *App) serialPortInUse(connID string) bool`）はそのまま残し、追加で次を定義する:

```go
// serialOwnerOf returns the session holding the device behind port, if any.
func (a *App) serialOwnerOf(goos string, port string) (sessionID string, connName string, ok bool)
```

### 7.5 承認・実行・読取のガード（approval.go）

| 入口 | 現状（a6f2706） | フェーズ4 |
|---|---|---|
| MCP `execute_command` / `request_command_execution` | `serial_not_supported` を返す（mcp_server.go:321） | 変更なし。回帰テスト M1 |
| `executeSSH` | `serial connections are interactive-only` を返す（approval.go:333） | 変更なし |
| GUI の `approve`（ApproveCommand 系） | ガード無し。`running` に遷移してから executeSSH で失敗する | `connectionByID` / `findConnection` の直後、`transitionRequest` の**前**に `if connOK && conn.connectionType()=="Serial" { return ActionResult{OK:false, Code:"serial_not_supported", Message:"シリアルの接続先では承認・実行できません。この要求は拒否してください"} }`。要求は pending のまま |
| MCP `get_request_status` / `get_command_history` | 履歴を返すだけ（端末出力は返さない） | 変更なし。シリアルのターミナル出力を返す MCP ツールは作らない（回帰テスト M2: どのツールの結果にも terminal:data の内容が含まれない） |

---

## 8. storage.go の変更

### 8.1 列の追加（`migrate` の `columns` に3行足す）

```go
{"connections", "data_bits", `ALTER TABLE connections ADD COLUMN data_bits INTEGER NOT NULL DEFAULT 8`},
{"connections", "parity", `ALTER TABLE connections ADD COLUMN parity TEXT NOT NULL DEFAULT 'none'`},
{"connections", "stop_bits", `ALTER TABLE connections ADD COLUMN stop_bits TEXT NOT NULL DEFAULT '1'`},
```

- **`schemaVersion` は 2 のまま**にする。理由: `addColumnIfMissing` は冪等で、既定値つきの列追加は旧バイナリ（列名を明示した SELECT/INSERT/UPDATE しか使わない）と両立する。3 に上げると、旧バイナリが `user_version > schemaVersion` で DB 全体を拒否し、ダウングレードできなくなる。列の追加だけで版を上げる必要はない。
- 既存の SSH 行も `8 / 'none' / '1'` を持つが、読み込み後の `normalizeConnection` は通らない（loadConnections は生の値を返す）ので、SSH 行は保存し直したときに 0 / '' になる。どちらでも害は無い（SSH では使わない）。

### 8.2 読み書き

```go
const connectionColumns = `id, name, type, host, port, user, auth_method, credential, serial_port, baud_rate,
	data_bits, parity, stop_bits,
	connect_timeout, command_timeout, tags_json, description, last_test_result, last_test_at, last_test_message, mcp_first_trust`
```

- `loadConnections`: Scan の `&conn.BaudRate` の後に `&conn.DataBits, &conn.Parity, &conn.StopBits`。
- `insertConnection`: 列リストの `baud_rate,` の後に `data_bits, parity, stop_bits,`、プレースホルダを3つ増やし、`conn.BaudRate` の後に `conn.DataBits, conn.Parity, conn.StopBits`。
- `updateConnection`: `baud_rate = ?,` の後に `data_bits = ?, parity = ?, stop_bits = ?,`、引数も同じ位置に3つ。
- SSH 行（DataBits=0, Parity="", StopBits=""）もそのまま書く。DB の DEFAULT は列追加時の既存行にだけ効く。

---

## 9. テストケース表

すべて `go test -race ./...`。fake は次の2つを `serial_port_test.go` に置く。

```go
// fakeSerialPort is a scriptable serialPort.
type fakeSerialPort struct {
	mu       sync.Mutex
	reads    chan fakeRead      // Read はここから1件ずつ受ける。close で PortClosed
	written  bytes.Buffer
	modes    []serial.Mode      // SetMode に渡された値の履歴
	setErr   error              // 次の SetMode が返すエラー（1回で消える）
	drainDur time.Duration      // Drain が戻るまでの時間
	closed   atomic.Bool
}
type fakeRead struct { data []byte; err error }

// withFakeSerial swaps openSerialPort / serialPortPresent / evalSerialSymlinks /
// serialRemovalProbes(→{0,0,0}) / serialGOOS for the test and restores them.
func withFakeSerial(t *testing.T, goos string, open func(name string, mode *serial.Mode) (serialPort, error), present func(goos, name string) (bool, error))
```

### 9.1 `serial_port_test.go`（純関数・表駆動）

`TestSerialSettingsValidate`（`validateSerialSettings(goos, s)`。`s` は `baud/data/parity/stop`）:

| # | goos | baud | data | parity | stop | 期待 |
|---|---|---|---|---|---|---|
| T1 | darwin | 115200 | 8 | none | 1 | OK |
| T2 | linux | 9600 | 7 | even | 1 | OK |
| T3 | windows | 9600 | 8 | odd | 2 | OK |
| T4 | darwin | 49 | 8 | none | 1 | baudRate |
| T5 | linux | 4000001 | 8 | none | 1 | baudRate |
| T6 | linux | 250000 | 8 | none | 1 | OK（特殊ボーレート） |
| T7 | windows | 9600 | 4 | none | 1 | dataBits |
| T8 | darwin | 9600 | 9 | none | 1 | dataBits |
| T9 | linux | 9600 | 8 | parity | 1 | parity |
| T10 | darwin | 9600 | 8 | mark | 1 | parity（V4） |
| T11 | openbsd | 9600 | 8 | space | 1 | parity（V4） |
| T12 | linux | 9600 | 8 | mark | 1 | OK |
| T13 | windows | 9600 | 8 | space | 1 | OK |
| T14 | linux | 9600 | 8 | none | 3 | stopBits（V5） |
| T15 | darwin | 9600 | 5 | none | 1.5 | stopBits（V6） |
| T16 | linux | 9600 | 5 | none | 1.5 | stopBits（V6） |
| T17 | windows | 9600 | 5 | none | 1.5 | OK |
| T18 | windows | 9600 | 8 | none | 1.5 | stopBits（V7） |
| T19 | windows | 9600 | 5 | none | 2 | stopBits（V8） |
| T20 | linux | 9600 | 5 | none | 2 | OK |

`TestSerialSettingsOfDefaults`: `Connection{Type:"Serial"}` → `{115200, 8, "none", "1"}`。`Parity:" EVEN "` → `"even"`。

`TestSerialSettingsModeAndLabel`:

| 入力 | `mode()` | `label()` |
|---|---|---|
| 115200/8/none/1 | `{115200, 8, NoParity, OneStopBit, nil}` | `8N1` |
| 9600/7/even/1 | `{9600, 7, EvenParity, OneStopBit, nil}` | `7E1` |
| 9600/8/odd/2 | `{9600, 8, OddParity, TwoStopBits, nil}` | `8O2` |
| 1200/5/mark/1.5 | `{1200, 5, MarkParity, OnePointFiveStopBits, nil}` | `5M1.5` |
| 4800/6/space/1 | `{4800, 6, SpaceParity, OneStopBit, nil}` | `6S1` |

`TestSerialReservationKey`（`evalSerialSymlinks` を map の fake に差し替え）。N1〜N10 は frontend の `serialport.js` と同じベクタにする:

| # | goos | 入力 | symlink fake | 期待キー |
|---|---|---|---|---|
| N1 | darwin | `/dev/tty.usbserial-10` | なし | `/dev/cu.usbserial-10` |
| N2 | darwin | `/dev/cu.usbserial-10` | なし | `/dev/cu.usbserial-10` |
| N3 | darwin | ` /dev/tty.usbmodem1101 ` | なし | `/dev/cu.usbmodem1101` |
| N4 | darwin | `/dev/ttys003` | なし | `/dev/ttys003`（pty。ドットが無いので変えない） |
| N5 | windows | `COM3` | — | `COM3` |
| N6 | windows | `\\.\com3` | — | `COM3` |
| N7 | windows | `com10` | — | `COM10` |
| N8 | linux | `/dev/ttyUSB0` | なし | `/dev/ttyUSB0` |
| N9 | linux | `/dev/ttyusb0` | なし | `/dev/ttyusb0`（大文字小文字は区別） |
| N10 | linux | `/dev/serial/by-id/usb-FTDI_FT232R-if00-port0` | → `/dev/ttyUSB0` | `/dev/ttyUSB0` |
| N11 | darwin | `/dev/my-board` | → `/dev/tty.usbserial-10` | `/dev/cu.usbserial-10` |
| N12 | linux | `/dev/serial/by-id/missing` | エラー | `/dev/serial/by-id/missing` |
| N13 | windows | `\\.\COM3` | （呼ばれないことを確認） | `COM3` |

`TestSerialOpenError`（`serialOpenErrorFrom` を表で回す。下注。「PortBusy」などは `*serial.PortError` の Code を表す）:

| # | goos | err | Code |
|---|---|---|---|
| O1 | darwin | PortBusy | `port_busy` |
| O2 | linux | PermissionDenied | `permission_denied`（Message に dialout） |
| O3 | darwin | `syscall.Errno(13)` | `permission_denied` |
| O4 | windows | PortNotFound | `port_not_found` |
| O5 | linux | `syscall.Errno(2)` | `port_not_found` |
| O6 | darwin | `fmt.Errorf("x: %w", fs.ErrNotExist)` | `port_not_found` |
| O7 | linux | InvalidParity | `invalid_serial_mode` |
| O8 | darwin | InvalidSerialPort | `open_failed` |
| O9 | windows | `syscall.Errno(5)` | `open_failed`（windows の 5 は E2 にしない） |
| O10 | linux | `errors.New("boom")` | `open_failed` |

注: `serial.PortError` の `code` は非公開フィールドで、パッケージ外から任意の Code の値を作れない。そこで判定を `serialPortErrorCode(err error) (serial.PortErrorCode, bool)`（`errors.As` で `*serial.PortError` を取り出して `Code()` を返す）と、Code と errno から結論を出す純関数 `serialOpenErrorFrom(goos string, code serial.PortErrorCode, hasCode bool, errno syscall.Errno, hasErrno bool, notExist bool) string` に分け、表は後者で回す。`serialOpenError` は両者を繋ぐだけ。O1/O2/O4/O7/O8 は `serialOpenErrorFrom` に Code を直接渡す。PortClosed（§9.3 C1）も同じ方法で `classifySerialIOErrorFrom(goos, code, hasCode, errno, hasErrno, isEOF)` に分ける。

### 9.2 存在確認

`TestDefaultSerialPortPresentUnix`（darwin/linux のみ実行。`t.TempDir()` にファイルを作る）: ファイルあり → `true,nil`。削除後 → `false,nil`。リンク先を消したシンボリックリンク → `false,nil`。権限の無いディレクトリ配下（root ではないときだけ）→ `false,err`。

### 9.3 `TestClassifySerialIOError`（`classifySerialIOErrorFrom` を表で回す）

| # | goos | 入力 | 分類 |
|---|---|---|---|
| C1a | linux | PortClosed | Removed |
| C1b | windows | PortClosed（ERROR_OPERATION_ABORTED 由来） | Removed |
| C2 | darwin | `io.EOF` | Removed |
| C3 | darwin | errno 6 (ENXIO) | Removed |
| C4a | darwin | errno 5 (EIO) | Removed |
| C4b | freebsd | errno 19 (ENODEV) | Removed |
| C5a | linux | errno 5 (EIO) | Removed |
| C5b | linux | errno 19 (ENODEV) | Removed |
| C6a | windows | errno 1167 | Removed |
| C6b | windows | errno 22 | Removed |
| C6c | windows | errno 31 | Removed |
| C6d | windows | errno 5 | Removed |
| C6e | windows | errno 1617 | Removed |
| C7a | darwin | errno 9 (EBADF) | Generic |
| C7b | linux | errno 11 (EAGAIN) | Generic |
| C7c | windows | errno 121（表に無い値） | Generic |
| C7d | darwin | errno 1167 | Generic（windows の値を unix で拾わない） |
| C7e | linux | `errors.New("x")` | Generic |

`TestSerialEndReason`（`serialPortPresent` を「呼ばれた順に結果を返す」fake にする。probes は `{0,0,0,0}`）:

| # | 分類になる err | present の返り（順） | 期待 reason | present の呼び出し回数 |
|---|---|---|---|---|
| R1 | linux EIO | false | `device_removed` | 1 |
| R2 | linux EIO | true, true, false | `device_removed` | 3（udev の遅れ） |
| R3 | linux EIO | true ×4 | `io_error` | 4 |
| R4 | windows 1167 | err ×4 | `device_removed` | 4 |
| R5 | darwin EBADF | false | `device_removed` | 1 |
| R6 | darwin EBADF | true | `io_error` | 1 |
| R7 | darwin EBADF | err | `io_error` | 1 |
| R8 | darwin ENXIO | err, false | `device_removed` | 2 |

### 9.4 `terminal_serial_test.go`（StartTerminal 〜 close。`withFakeSerial` と契約の `app.emit` フック）

| # | テスト | 手順 | 確認すること |
|---|---|---|---|
| P1 | `TestSerialOpenUsesSettings` | DataBits=7, Parity=even, Baud=9600 の接続先で StartTerminal | open に渡った mode が `{9600,7,EvenParity,OneStopBit}`。`ListTerminalSessions()[0]` の `baud:9600`、`serialMode:"7E1"`、`port` は設定値そのまま |
| P2 | `TestSerialInvalidModeRejectedBeforeOpen` | darwin で Parity=mark | `Code:"invalid_serial_mode"`、open は呼ばれない、予約表と terms は空、sessions:changed は送られない |
| P3 | `TestSerialCuTtyDoubleOpenRejected` | 接続先A `/dev/cu.usbserial-10` を open、接続先B `/dev/tty.usbserial-10` を StartTerminal（darwin） | B は `Code:"port_in_use"`、Message に A の名前と「同じデバイス」、open は1回だけ |
| P4 | `TestSerialSameConnSecondTabRejected` | 同じ接続先で2回 StartTerminal | 2回目 `port_in_use` |
| P5 | `TestSerialReservedWhileConnecting` | open の fake をチャネルで止めたまま、別の id で同じポートを StartTerminal | 2つ目は即座に `port_in_use`（open は呼ばれない）。1つ目を進めると成功 |
| P6 | `TestSerialReservationReleasedOnOpenFailure` | open が PortBusy を返す → もう一度 StartTerminal（今度は成功） | 1回目 `port_busy`、`lastErr` に記録。2回目は成功（予約が残っていない） |
| P7 | `TestSerialReservationReleasedOnClose` | A を開く → CloseTerminal → B（tty 名）を開く | B が成功 |
| P8 | `TestSerialWindowsAliases` | windows で `COM3` を開き、別の接続先 `\\.\com3` | `port_in_use` |
| P9 | `TestSerialLinuxByIDSymlink` | linux、by-id のリンク（fake で `/dev/ttyUSB0`）と `/dev/ttyUSB0` | `port_in_use` |
| P10 | `TestSerialUnplugDeviceRemoved` | 開いた後、fake の reads に `{nil, syscall.Errno(6)}`（darwin）、present=false | exit が1回だけ `{"reason":"device_removed"}`、message にポート名、タブが ListTerminalSessions から消える、予約解除、`lastErr` に記録、sessions:changed |
| P11 | `TestSerialLinuxZeroReadIsRemoval` | linux、reads を close（PortClosed）、present=true, true, false | `device_removed` |
| P12 | `TestSerialIOErrorWhenStillPresent` | reads に EIO、present 常に true | `io_error` |
| P13 | `TestSerialUserCloseNotMisclassified` | CloseTerminal（fake の Close で reads を close → PortClosed） | exit は `user` で1回。present は1回も呼ばれない |
| P14 | `TestSerialWriteErrorClassified` | fake の Write が ENXIO、present=false、SendTerminalInput | exit `device_removed` |
| P15 | `TestSerialOutputBeforeErrorDelivered` | reads に `{"abc", nil}` → `{nil, EIO}` | terminal:data に `abc` が exit より先に届く |

### 9.5 その場適用（`terminal_serial_test.go`）

| # | テスト | 手順 | 確認すること |
|---|---|---|---|
| M1s | `TestApplySerialSettingsInPlace` | 開いたタブに `applySerialSettings(conn, 9600/7/even/1)` | `applied=true`、fake.modes の末尾が新設定、Close は呼ばれない、TerminalInfo の `baud:9600`・`serialMode:"7E1"`、sessions:changed が1回 |
| M2s | `TestApplySerialSettingsSameIsNoop` | 現在と同じ設定で適用 | SetMode も sessions:changed も呼ばれない、`applied=true` |
| M3s | `TestApplySerialSettingsNoTab` | タブ無し | `applied=false, err=nil` |
| M4s | `TestApplySerialSettingsWaitsForWrites` | fake の Write を 50ms ブロックさせ、SendTerminalInput と applySerialSettings を同時に呼ぶ | fake の記録で Write → Drain → SetMode の順（重ならない） |
| M5s | `TestApplySerialSettingsDrainTimeout` | `drainDur=1s`、`serialDrainTimeout=20ms` | 約 20ms で SetMode まで進む |
| M6s | `TestApplySerialSettingsRollbackUnix` | `serialGOOS="linux"`、`setErr=InvalidSpeed 相当` | `err!=nil`、fake.modes が [新, 旧]、タブは開いたまま、TerminalInfo は旧設定のまま |
| M7s | `TestApplySerialSettingsWindowsFailureCloses` | `serialGOOS="windows"`、`setErr` あり | exit `io_error` が1回、ロールバックの SetMode は呼ばれない、読み込みループの PortClosed で二重 exit にならない |
| M8s | `TestApplySerialSettingsWhileConnecting` | open を止めたまま適用 → open を進める | open 時の mode は旧設定、open 完了直後に SetMode(新) が1回、TerminalInfo は新設定 |
| M9s | `TestApplySerialSettingsRace` | `-count=20`。適用と CloseTerminal と読み込みエラーを同時に起こす | `-race` で問題なし、exit は1回 |

### 9.6 `app_serial_test.go`（app.go / approval.go）

| # | テスト | 確認すること |
|---|---|---|
| A1 | `TestSwitchToSerialClearsCredential` | パスワード認証の SSH 接続先を Serial に変えて保存 → DB と `a.conns` の Host/User/Credential が空 |
| A2 | `TestHostMatchExcludesSerial` | シリアル行に（古い DB を模して）Host=`10.0.0.5` を直接 INSERT、SSH 行は無し → `resolveConnection("10.0.0.5")` は見つからない |
| A3 | `TestSwitchToSSHClearsSerialFields` | Serial を SSH に変えて保存 → SerialPort/BaudRate/DataBits/Parity/StopBits が空・0 |
| A4 | `TestNormalizeSerialDefaults` | `Connection{Type:"Serial", SerialPort:"/dev/x"}` → 115200/8/none/1。`Parity:"Odd"` → `odd` |
| A5 | `TestSaveConnectionRejectsInvalidSerialMode` | `serialGOOS="darwin"`、Parity=mark で保存 → `OK:false, Code:"invalid_serial_mode"`、DB は変わらない |
| A6 | `TestSaveConnectionAppliesToOpenTab` | タブを開いたまま Parity を even に変えて保存 → `OK:true, Code:"serial_mode_applied"`、fake.modes の末尾が EvenParity、タブは残る（表 S2） |
| A7 | `TestSaveConnectionApplyFailure` | fake の SetMode 失敗 → `OK:true, Code:"serial_mode_not_applied"`、DB には新設定が入っている（S4） |
| A8 | `TestSaveConnectionPortChangeNeedsReconnect` | ポートを変えて保存 → `Code:"reconnect_required"`、SetMode は呼ばれない、タブは旧ポートのまま（S7） |
| A9 | `TestSaveConnectionTypeChangeKeepsTab` | 開いたまま SSH に変えて保存 → `Code:"reconnect_required"`、タブは残る（S8） |
| A10 | `TestSaveConnectionNoTabNoCode` | タブ無しで設定変更 → Code 空（S6） |
| A11 | `TestSaveConnectionNoDeadlock` | `-race -count=20`。SaveConnection とタブの開閉・その場適用を並行に回してもデッドロックしない（`a.mu` を持ったまま termMu を取っていない） |
| A12 | `TestSerialConnStateLive` | タブ無し・present=true → `available`、present=false → `missing`、開くと `in_use`、別の接続先が同じデバイスを開いていると `in_use` |
| A13 | `TestSerialPresenceCached` | TTL 内に `sessionSummaries` を10回 → present の呼び出しはポートごとに1回 |
| A14 | `TestWatchSerialPresenceEmitsOnChange` | 間隔を短くして present を true→false に → sessions:changed が1回、変化が無い周期では送らない |
| A14b | `TestSerialReplugClearsLastError` | P10 と同じ手順で `device_removed` → `lastErr` に記録され connState は `error`。present を false→true にして watch を1周 → `lastErr` が消え、sessions:changed が1回、connState が `available`。present が false のままの周期では `lastErr` は残る |
| A15 | `TestTestSerialConnectionDoesNotOpen` | testSerialConnection は open を1回も呼ばない。Code は `available` / `in_use` / `missing` |
| A16 | `TestApproveRejectsSerial` | SSH 時代に作った pending 要求の接続先を Serial に変更 → ApproveCommand は `Code:"serial_not_supported"`、要求は pending のまま、executeSSH は呼ばれない |
| M1 | `TestMCPExecuteSerialNotSupported`（mcp_server_test.go に既存の serial ケースがあれば流用） | execute_command / request_command_execution が `serial_not_supported`、要求は作られない |
| M2 | `TestMCPNeverReturnsTerminalOutput` | シリアルのタブで `secret-xyz` を出力させた後、全 MCP ツールを呼んでも結果に `secret-xyz` が含まれない |

### 9.7 storage（`storage_test.go` に追記）

| # | テスト | 確認すること |
|---|---|---|
| ST1 | `TestSerialColumnsMigrateV2` | 3列が無い v2 DB（a6f2706 の schema）を開く → 列が追加され、既存のシリアル行は 8/none/1、`user_version` は 2 のまま、`.bak-v2` は作られない（版を上げていないので） |
| ST2 | `TestSerialSettingsRoundTrip` | 7/even/2 で insert → load → update（odd）→ load |
| ST3 | `TestSerialColumnsIdempotent` | 同じ DB を2回 openStore してもエラーにならない |

---

## 10. 統合時に main.js 側でやること（Integrator 向け）

1. **接続先の編集画面（契約 §2.5 F3 / design §6.5）**: シリアルのとき「データビット（5/6/7/8）」「パリティ（なし/奇数/偶数/マーク/スペース）」「ストップビット（1/1.5/2）」の select を足す。値は `dataBits`（数値）、`parity`（`none|odd|even|mark|space`）、`stopBits`（`"1"|"1.5"|"2"`）。macOS ではマーク/スペースと 1.5 を disabled にし、Windows ではデータビット 5 のときだけ 1.5 を有効、5 のとき 2 を無効にする（§2.3 と同じ規則。正は Go の検証）。「RTS/CTS フロー制御は非対応です / RTS/CTS flow control is not supported」を注記で出す。
2. **model.js の `editableSnapshot` / `connectionDirtyState`（契約 §2.6）**: 比較項目に `dataBits`, `parity`, `stopBits` を足す（Serial のときだけ比べる）。足さないと設定を変えても「保存済み」のままになり保存ボタンが押せない。
3. **SaveConnection の Code**: `serial_mode_applied` は成功トースト（Message をそのまま）、`serial_mode_not_applied` と `reconnect_required` は警告トーストにし、該当タブに「再接続」ボタンを出す。`invalid_serial_mode` はフォーム内アラート。
4. **StartTerminal の Code**: `port_in_use`（Message に相手の接続先名。そのタブへ移動するボタン）、`port_busy`、`permission_denied`、`port_not_found`、`invalid_serial_mode`、`open_failed` を I18N に足す（JA は Message、EN はキーで固定文言）。
5. **terminal:exit の `reason:"device_removed"`**: タブを読み取り専用にし、「デバイスが外れました［再接続］/ Device disconnected [Reconnect]」を出す（契約の他の非 user reason と同じ扱いで、文言だけ専用）。再接続は新しい `TERM-<uuid>` で StartTerminal（同じ id は使わない。旧 id は map から消えている）。
6. **TerminalInfo.serialMode**: タブのツールチップとサブタブのラベルに `"/dev/cu.usbserial-10 @9600 7E1"` の形で出す。sessions:changed を受けたら更新する（その場適用で変わる）。
7. **connState の `available` / `missing`**: §7.4 によりサイドバーと上部サーバータブにシリアルの `available`（○ 利用可能）/`missing`（▲ ではなく ○ に「見つかりません」）が来る。`model.js` の `connStateSymbol` は `available`/`missing` を `○` にする（契約の記号表に無いので追加）。
8. **serialport.js（別担当）との突き合わせ**: §9.1 の N1〜N10 を serialport.js のテストに入れ、Go と同じ結果になることを確認する。frontend の正規化はポート一覧の照合と表示にだけ使い、二重 open の判定は Go の戻り値（`port_in_use`）に任せる。
9. **`wails generate module`**: `Connection` に3フィールド、`TerminalInfo` に `serialMode` が増えるので再生成する。新しい Wails バインド関数は無い。
