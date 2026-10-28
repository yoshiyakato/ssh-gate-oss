// Pure helpers for serial port names (design §5.2, §7 phase 4). Nothing here
// touches the DOM or Wails; the caller passes goos (Wails
// Environment().platform: "darwin" | "windows" | "linux" ...).

// normalizeSerialPort mirrors normalizeSerialPort in terminal.go so the
// frontend compares ports with the same key the backend reserves:
// darwin folds /dev/tty.X onto /dev/cu.X, windows drops the \\.\ prefix and
// upper-cases, anything else is only trimmed.
export function normalizeSerialPort(goos, name) {
  const port = String(name ?? '').trim();
  switch (goos) {
    case 'darwin':
      return port.startsWith('/dev/tty.') ? `/dev/cu.${port.slice('/dev/tty.'.length)}` : port;
    case 'windows':
      return (port.startsWith('\\\\.\\') ? port.slice(4) : port).toUpperCase();
    default:
      return port;
  }
}

// sameSerialPort reports whether two names open the same device. Empty names
// never match, so an unset port is not "in use" by another unset port.
export function sameSerialPort(goos, a, b) {
  const left = normalizeSerialPort(goos, a);
  return left !== '' && left === normalizeSerialPort(goos, b);
}

// dedupeSerialPorts collapses names that normalize to the same device (on
// macOS ListSerialPorts returns both tty.X and cu.X). The first spelling of
// each device wins unless a cu.X / normalized spelling appears, which is
// preferred because it is what the backend reserves. Order follows first
// appearance; empty names are dropped.
export function dedupeSerialPorts(goos, ports) {
  const byKey = new Map();
  for (const raw of ports ?? []) {
    const name = String(raw ?? '').trim();
    const key = normalizeSerialPort(goos, name);
    if (!key) continue;
    if (!byKey.has(key) || name === key) byKey.set(key, name);
  }
  return [...byKey.values()];
}

// serialPortState returns the §5.4 serial connState for `port`:
// in_use when a live session holds the same device, available when the
// device is listed, missing otherwise. `inUsePorts` are the ports of open
// serial sessions (TerminalInfo.port), `availablePorts` is ListSerialPorts.
export function serialPortState(goos, port, { availablePorts = [], inUsePorts = [] } = {}) {
  const key = normalizeSerialPort(goos, port);
  if (!key) return 'missing';
  if ((inUsePorts ?? []).some((p) => normalizeSerialPort(goos, p) === key)) return 'in_use';
  if ((availablePorts ?? []).some((p) => normalizeSerialPort(goos, p) === key)) return 'available';
  return 'missing';
}
