// Consistency checks for docs/spec/phase4-serial-patch.md.
//
// The spec is meant to be implemented verbatim, so its rule tables and its
// test-case tables must agree with each other. This file re-implements the
// rules (§2.3, §4.1, §5.3) as a tiny JS oracle and runs every test row of §9
// through it, and checks that the go.bug.st/serial facts in §1 still hold for
// the version pinned in go.mod (skipped when the module cache is absent).
//
// Run: node --test docs/spec/phase4-serial-patch.test.mjs

import { test } from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync, existsSync } from 'node:fs';
import { execFileSync } from 'node:child_process';
import { dirname, join } from 'node:path';
import { fileURLToPath } from 'node:url';

const here = dirname(fileURLToPath(import.meta.url));
const repoRoot = join(here, '..', '..');
const doc = readFileSync(join(here, 'phase4-serial-patch.md'), 'utf8');

// ---------------------------------------------------------------- helpers

function section(title) {
  const start = doc.indexOf(title);
  assert.ok(start >= 0, `section not found: ${title}`);
  const rest = doc.slice(start + title.length);
  // A heading title ends at the next heading of the same or a higher level;
  // a plain anchor (e.g. a test name) ends at the next ## or ### heading.
  const level = /^#+/.exec(title)?.[0].length ?? 3;
  const next = rest.search(new RegExp(`\\n#{2,${level}} `));
  return next < 0 ? rest : rest.slice(0, next);
}

// Rows of the first markdown table whose first cell matches idPattern.
function rows(text, idPattern) {
  return text
    .split('\n')
    .filter((line) => line.startsWith('|'))
    .map((line) => splitRow(line))
    .filter((cells) => idPattern.test(cells[0]));
}

function splitRow(line) {
  // Split on | that is not escaped and not inside backticks.
  const cells = [];
  let cur = '';
  let tick = false;
  for (let i = 1; i < line.length; i++) {
    const ch = line[i];
    if (ch === '`') tick = !tick;
    if (ch === '|' && !tick && line[i - 1] !== '\\') {
      cells.push(cur.trim());
      cur = '';
      continue;
    }
    cur += ch;
  }
  return cells;
}

const unquote = (s) => s.replace(/^`|`$/g, '');

// ---------------------------------------------------------------- oracles

const UNIX_NO_MARK_SPACE = new Set(['darwin', 'freebsd', 'openbsd', 'netbsd', 'dragonfly']);

// §2.3 validateSerialSettings → '' (OK) or Field name.
function validate(goos, baud, data, parity, stop) {
  if (baud < 50 || baud > 4_000_000) return 'baudRate';
  if (![5, 6, 7, 8].includes(data)) return 'dataBits';
  if (!['none', 'odd', 'even', 'mark', 'space'].includes(parity)) return 'parity';
  if ((parity === 'mark' || parity === 'space') && UNIX_NO_MARK_SPACE.has(goos)) return 'parity';
  if (!['1', '1.5', '2'].includes(stop)) return 'stopBits';
  if (stop === '1.5' && goos !== 'windows') return 'stopBits';
  if (goos === 'windows' && stop === '1.5' && data !== 5) return 'stopBits';
  if (goos === 'windows' && data === 5 && stop === '2') return 'stopBits';
  return '';
}

// Contract §1.5 normalizeSerialPort.
function normalizeSerialPort(goos, name) {
  if (goos === 'darwin') return name.replace(/^\/dev\/tty\./, '/dev/cu.');
  if (goos === 'windows') return name.replace(/^\\\\\.\\/, '').toUpperCase();
  return name;
}

// §4.1 serialReservationKey with a symlink fake (undefined = no link, null = error).
function reservationKey(goos, name, link, calls) {
  let key = normalizeSerialPort(goos, name.trim());
  if (goos === 'windows' || key === '') return key;
  calls.push(key);
  if (link) key = normalizeSerialPort(goos, link);
  return key;
}

function label(data, parity, stop) {
  return `${data}${parity[0].toUpperCase()}${stop}`;
}

// §5.3 classifySerialIOError. input: {portClosed, eof, errno, plain}
const REMOVED_ERRNO = {
  darwin: [6, 5, 19],
  freebsd: [6, 5, 19],
  openbsd: [6, 5, 19],
  linux: [5, 19, 6],
  windows: [5, 6, 22, 31, 1167, 1617, 995],
};
function classify(goos, input) {
  if (input.portClosed || input.eof) return 'Removed';
  if (input.errno !== undefined && (REMOVED_ERRNO[goos] || []).includes(input.errno)) return 'Removed';
  return 'Generic';
}

// §5.3 serialEndReason with 4 probes. results: array of true|false|'err'.
function endReason(cls, results) {
  const probes = cls === 'Generic' ? 1 : 4;
  let failed = 0;
  let calls = 0;
  for (let i = 0; i < probes; i++) {
    const r = results[Math.min(i, results.length - 1)];
    calls++;
    if (r === 'err') { failed++; continue; }
    if (r === false) return { reason: 'device_removed', calls };
  }
  if (cls === 'Removed' && failed === probes) return { reason: 'device_removed', calls };
  return { reason: 'io_error', calls };
}

const ERRNO_NAMES = { ENXIO: 6, EIO: 5, ENODEV: 19, EBADF: 9, EAGAIN: 11 };

function parseErrInput(cell) {
  if (/PortClosed/.test(cell)) return { portClosed: true };
  if (/io\.EOF/.test(cell)) return { eof: true };
  const m = cell.match(/errno\s+(\d+)/) || cell.match(/(\d{3,4})/);
  if (m) return { errno: Number(m[1]) };
  for (const [n, v] of Object.entries(ERRNO_NAMES)) if (cell.includes(n)) return { errno: v };
  return { plain: true };
}

// ---------------------------------------------------------------- tests

test('spec has every section the task asks for', () => {
  for (const h of [
    '## 0. まとめ',
    '## 1. go.bug.st/serial v1.7.1 で確認した事実',
    '## 2. 通信パラメータ',
    '## 3. シリアルの open',
    '## 4. cu / tty の二重 open 拒否',
    '## 5. 切断の判定',
    '## 6. setMode のその場適用',
    '## 7. app.go / approval.go の変更',
    '## 8. storage.go の変更',
    '## 9. テストケース表',
    '## 10. 統合時に main.js 側でやること',
  ]) {
    assert.ok(doc.includes(h), h);
  }
});

test('§9.1 validation rows agree with the §2.3 rules', () => {
  const table = rows(section('`TestSerialSettingsValidate`'), /^T\d+$/);
  assert.equal(table.length, 20);
  for (const [id, goos, baud, data, parity, stop, want] of table) {
    const got = validate(goos, Number(baud), Number(data), parity, stop);
    const expected = want.startsWith('OK') ? '' : want.split(/[（(]/)[0].trim();
    assert.equal(got, expected, `${id} ${goos} ${baud}/${data}/${parity}/${stop}`);
  }
});

test('§2.3 rule rows reference only fields the oracle knows', () => {
  const table = rows(section('### 2.3'), /^V\d+$/);
  assert.ok(table.length >= 8);
  for (const r of table) {
    const field = r[5];
    assert.ok(['baudRate', 'dataBits', 'parity', 'stopBits'].includes(field), `${r[0]} field ${field}`);
  }
});

test('§9.1 label rows match label()', () => {
  const t = section('`TestSerialSettingsModeAndLabel`');
  const table = rows(t, /^\d+\/\d\/[a-z]+\/[\d.]+$/);
  assert.equal(table.length, 5);
  for (const [input, , want] of table) {
    const [, data, parity, stop] = input.split('/');
    assert.equal(label(data, parity, stop), unquote(want), input);
  }
});

test('§9.1 reservation-key vectors follow normalizeSerialPort + symlink resolution', () => {
  const table = rows(section('`TestSerialReservationKey`'), /^N\d+$/);
  assert.equal(table.length, 13);
  for (const [id, goos, input, linkCell, wantCell] of table) {
    let link;
    const m = linkCell.match(/→ `([^`]+)`/);
    if (m) link = m[1];
    const calls = [];
    const got = reservationKey(goos, unquote(input), link, calls);
    const want = unquote(wantCell.split(/[（(]/)[0].trim());
    assert.equal(got, want, id);
    if (goos === 'windows') assert.equal(calls.length, 0, `${id}: windows must not resolve symlinks`);
  }
});

test('§9.3 classify rows agree with the §5.3 table', () => {
  const table = rows(section('### 9.3'), /^C\d+[a-e]?$/);
  assert.ok(table.length >= 18);
  for (const [id, goos, input, want] of table) {
    assert.equal(classify(goos, parseErrInput(input)), want.split(/[（(]/)[0].trim(), id);
  }
});

test('§9.3 end-reason rows agree with serialEndReason', () => {
  const t = section('### 9.3');
  const table = rows(t.slice(t.indexOf('`TestSerialEndReason`')), /^R\d+$/);
  assert.equal(table.length, 8);
  for (const [id, errCell, presentCell, wantReason, wantCalls] of table) {
    const [goos, ...rest] = errCell.split(' ');
    const cls = classify(goos, parseErrInput(rest.join(' ')));
    const results = [];
    for (const part of presentCell.split(',').map((s) => s.trim())) {
      const mm = part.match(/^(true|false|err)(?:\s*[×x]\s*(\d+))?/);
      assert.ok(mm, `${id}: cannot parse ${part}`);
      const v = mm[1] === 'err' ? 'err' : mm[1] === 'true';
      for (let i = 0; i < Number(mm[2] || 1); i++) results.push(v);
    }
    const got = endReason(cls, results);
    assert.equal(got.reason, unquote(wantReason), id);
    assert.equal(String(got.calls), wantCalls.split(/[（(]/)[0].trim(), `${id} calls`);
  }
});

test('every ActionResult/exit code the spec introduces is tested and handed to main.js', () => {
  const codes = [
    'invalid_serial_mode', 'port_in_use', 'port_busy', 'permission_denied', 'port_not_found',
    'open_failed', 'serial_mode_applied', 'serial_mode_not_applied', 'reconnect_required',
    'serial_not_supported', 'device_removed',
  ];
  const tests = section('## 9. テストケース表');
  const integ = section('## 10. 統合時に main.js 側でやること');
  for (const c of codes) {
    assert.ok(tests.includes(c), `no test mentions ${c}`);
    if (c !== 'serial_not_supported') assert.ok(integ.includes(c), `§10 does not mention ${c}`);
  }
});

// ------------------------------------------------- library facts (§1)

function serialModuleDir() {
  const gomod = readFileSync(join(repoRoot, 'go.mod'), 'utf8');
  const m = gomod.match(/go\.bug\.st\/serial (v[\d.]+)/);
  if (!m) return null;
  let cache;
  try {
    cache = execFileSync('go', ['env', 'GOMODCACHE'], { encoding: 'utf8' }).trim();
  } catch {
    return null;
  }
  const dir = join(cache, `go.bug.st/serial@${m[1]}`);
  return existsSync(dir) ? { dir, version: m[1] } : null;
}

const mod = serialModuleDir();

test('go.mod pins the serial version the spec was written against', () => {
  const gomod = readFileSync(join(repoRoot, 'go.mod'), 'utf8');
  assert.match(gomod, /go\.bug\.st\/serial v1\.7\.1/);
});

test('§1 library facts still hold in the module source', { skip: mod ? false : 'go module cache not available' }, () => {
  const src = (f) => readFileSync(join(mod.dir, f), 'utf8');
  const serialGo = src('serial.go');
  // L1/L2: API surface
  for (const sig of ['SetMode(mode *Mode) error', 'Drain() error', 'Read(p []byte) (n int, err error)']) {
    assert.ok(serialGo.includes(sig), sig);
  }
  assert.match(serialGo, /DataBits\s+int/);
  assert.match(serialGo, /OnePointFiveStopBits/);
  assert.doesNotMatch(serialGo, /SetFlowControl|RTSCTS/i, 'a flow-control API appeared; update §1 L1');
  // L3: PortError has no Unwrap
  assert.doesNotMatch(serialGo, /func \(e (\*)?PortError\) Unwrap/);
  const unix = src('serial_unix.go');
  // L4: exclusive access, L10: zero read => PortClosed
  assert.match(unix, /TIOCEXCL/);
  assert.match(unix, /n == 0 && err == nil[\s\S]{0,80}PortClosed/);
  assert.match(unix, /case unix\.EBUSY:\s*return nil, &PortError\{code: PortBusy\}/);
  // L6: 1.5 stop bits rejected on unix; darwin has no CMSPAR
  assert.match(unix, /case OnePointFiveStopBits:\s*return &PortError\{code: InvalidStopBits\}/);
  assert.match(src('serial_darwin_64.go'), /const tcCMSPAR uint64 = 0/);
  // L7: windows SetMode closes the port on failure
  const win = src('serial_windows.go');
  const setMode = win.slice(win.indexOf('func (port *windowsPort) SetMode'), win.indexOf('func (port *windowsPort) setModeParams'));
  assert.match(setMode, /port\.Close\(\)/);
  assert.match(win, /case windows\.ERROR_ACCESS_DENIED:\s*return nil, &PortError\{code: PortBusy\}/);
  // L11: aborted read => PortClosed
  assert.match(win, /ERROR_OPERATION_ABORTED:[\s\S]{0,80}PortClosed/);
  // L13: linux enumeration opens ttyS* (reason for D1)
  assert.match(unix, /HasPrefix\(f\.Name\(\), "ttyS"\)[\s\S]{0,200}nativeOpen\(portName/);
});
