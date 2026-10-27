import { test } from 'node:test';
import assert from 'node:assert/strict';
import {
  normalizeSerialPort,
  sameSerialPort,
  dedupeSerialPorts,
  serialPortState,
} from './serialport.js';

test('normalizeSerialPort', async (t) => {
  const cases = [
    // [goos, input, want]
    ['darwin', '/dev/tty.usbserial-10', '/dev/cu.usbserial-10'],
    ['darwin', '/dev/cu.usbserial-10', '/dev/cu.usbserial-10'],
    ['darwin', '  /dev/tty.usbmodem1 ', '/dev/cu.usbmodem1'],
    ['darwin', '/dev/ttys001', '/dev/ttys001'],
    ['darwin', '/dev/tty.', '/dev/cu.'],
    ['darwin', 'tty.usbserial', 'tty.usbserial'],
    ['darwin', '/dev/TTY.usbserial', '/dev/TTY.usbserial'],
    ['windows', '\\\\.\\COM10', 'COM10'],
    ['windows', 'com3', 'COM3'],
    ['windows', ' \\\\.\\com4 ', 'COM4'],
    ['windows', 'COM1', 'COM1'],
    ['windows', '\\\\?\\COM5', '\\\\?\\COM5'],
    ['linux', '/dev/ttyUSB0', '/dev/ttyUSB0'],
    ['linux', '/dev/tty.usbserial', '/dev/tty.usbserial'],
    ['linux', 'com3', 'com3'],
    ['', ' /dev/ttyS0 ', '/dev/ttyS0'],
    ['darwin', '', ''],
    ['windows', null, ''],
    ['linux', undefined, ''],
  ];
  for (const [goos, input, want] of cases) {
    await t.test(`${goos || '(none)'} ${JSON.stringify(input)}`, () => {
      assert.equal(normalizeSerialPort(goos, input), want);
    });
  }
});

test('sameSerialPort', async (t) => {
  const cases = [
    ['darwin', '/dev/tty.usbserial-10', '/dev/cu.usbserial-10', true],
    ['darwin', '/dev/cu.a', '/dev/cu.b', false],
    ['windows', '\\\\.\\COM3', 'com3', true],
    ['windows', 'COM3', 'COM30', false],
    ['linux', '/dev/ttyUSB0', '/dev/ttyUSB0', true],
    ['linux', '/dev/tty.x', '/dev/cu.x', false],
    ['darwin', '', '', false],
    ['windows', ' ', null, false],
  ];
  for (const [goos, a, b, want] of cases) {
    await t.test(`${goos} ${JSON.stringify(a)} vs ${JSON.stringify(b)}`, () => {
      assert.equal(sameSerialPort(goos, a, b), want);
    });
  }
});

test('dedupeSerialPorts', async (t) => {
  const cases = [
    ['darwin keeps cu over tty', 'darwin',
      ['/dev/tty.usbserial-10', '/dev/cu.usbserial-10', '/dev/tty.Bluetooth'],
      ['/dev/cu.usbserial-10', '/dev/tty.Bluetooth']],
    ['darwin cu first', 'darwin',
      ['/dev/cu.a', '/dev/tty.a'], ['/dev/cu.a']],
    ['windows case and prefix', 'windows',
      ['com3', '\\\\.\\COM3', 'COM4'], ['com3', 'COM4']],
    ['windows prefers normalized', 'windows',
      ['com3', 'COM3'], ['COM3']],
    ['linux leaves distinct names', 'linux',
      ['/dev/ttyUSB0', '/dev/ttyUSB1', '/dev/ttyUSB0'], ['/dev/ttyUSB0', '/dev/ttyUSB1']],
    ['drops empty', 'linux', ['', '  ', null], []],
    ['null input', 'darwin', null, []],
  ];
  for (const [name, goos, input, want] of cases) {
    await t.test(name, () => {
      assert.deepEqual(dedupeSerialPorts(goos, input), want);
    });
  }
});

test('dedupeSerialPorts on windows keeps the first spelling when the exact normalized name is absent', () => {
  // '\\.\COM3' normalizes to 'COM3' but is not equal to it, so the first
  // spelling stays unless the exact normalized name is listed.
  assert.deepEqual(dedupeSerialPorts('windows', ['\\\\.\\COM3', 'com3']), ['\\\\.\\COM3']);
});

test('serialPortState', async (t) => {
  const cases = [
    ['in use via tty/cu fold', 'darwin', '/dev/tty.usbserial-10',
      { availablePorts: ['/dev/cu.usbserial-10'], inUsePorts: ['/dev/cu.usbserial-10'] }, 'in_use'],
    ['available', 'darwin', '/dev/cu.usbserial-10',
      { availablePorts: ['/dev/tty.usbserial-10'], inUsePorts: [] }, 'available'],
    ['missing', 'darwin', '/dev/cu.gone', { availablePorts: ['/dev/cu.other'] }, 'missing'],
    ['in use wins even if unplugged', 'windows', 'com3', { inUsePorts: ['\\\\.\\COM3'] }, 'in_use'],
    ['windows available', 'windows', '\\\\.\\com7', { availablePorts: ['COM7'] }, 'available'],
    ['empty port', 'linux', '', { availablePorts: [''] }, 'missing'],
    ['no options', 'linux', '/dev/ttyUSB0', undefined, 'missing'],
    ['null lists', 'linux', '/dev/ttyUSB0', { availablePorts: null, inUsePorts: null }, 'missing'],
  ];
  for (const [name, goos, port, opts, want] of cases) {
    await t.test(name, () => {
      assert.equal(serialPortState(goos, port, opts), want);
    });
  }
});
