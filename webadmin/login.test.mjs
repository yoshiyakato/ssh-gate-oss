// node --test webadmin/login.test.mjs
import { test } from 'node:test';
import assert from 'node:assert/strict';
import { createRequire } from 'node:module';

const require = createRequire(import.meta.url);
const login = require('./login.js');

test('safeNext keeps same-origin paths only', () => {
  assert.equal(login.safeNext('?next=%2Fconnections%3Fa%3D1'), '/connections?a=1');
  assert.equal(login.safeNext(''), '/');
  assert.equal(login.safeNext('?next=https%3A%2F%2Fevil.example'), '/');
  assert.equal(login.safeNext('?next=%2F%2Fevil.example'), '/');
  assert.equal(login.safeNext('?next=%2F%5Cevil.example'), '/');
  assert.equal(login.safeNext('?next=javascript%3Aalert(1)'), '/');
  assert.equal(login.safeNext('?next=%2Fa%0Ab'), '/');
  assert.equal(login.safeNext('?next=%2Flogin'), '/');
});

test('login posts JSON with the CSRF header', async () => {
  let seen;
  const status = await login.login(async (url, init) => {
    seen = { url, init };
    return { status: 204 };
  }, 'sgadm_x');
  assert.equal(status, 204);
  assert.equal(seen.url, '/api/login');
  assert.equal(seen.init.method, 'POST');
  assert.equal(seen.init.headers['X-SSH-Gete-CSRF'], '1');
  assert.equal(seen.init.headers['Content-Type'], 'application/json');
  assert.deepEqual(JSON.parse(seen.init.body), { token: 'sgadm_x' });
});

test('login sends a token as token and anything else as password', async () => {
  assert.deepEqual(login.loginBody('  sgadm_abc \n'), { token: 'sgadm_abc' });
  assert.deepEqual(login.loginBody('correct horse battery'), { password: 'correct horse battery' });
  assert.deepEqual(login.loginBody(' spaced pw '), { password: ' spaced pw ' });
  let body;
  await login.login(async (url, init) => {
    body = JSON.parse(init.body);
    return { status: 204 };
  }, 'my long admin password');
  assert.deepEqual(body, { password: 'my long admin password' });
  assert.equal('token' in body, false);
});

test('language and messages', () => {
  assert.equal(login.pickLang(['en-US']), 'en');
  assert.equal(login.pickLang(['ja-JP', 'en']), 'ja');
  assert.equal(login.pickLang([]), 'ja');
  assert.equal(login.messageFor(401, login.TEXT.en), login.TEXT.en.invalid);
  assert.equal(login.messageFor(429, login.TEXT.ja), login.TEXT.ja.locked);
  assert.match(login.messageFor(500, login.TEXT.en), /HTTP 500/);
});

// Review finding 5: the session key from the login response is handed to
// onKey and saved where only this origin can read it.
test('login hands the session key to onKey and saveSessionKey stores it', async () => {
  let key = null;
  const status = await login.login(async () => ({ status: 204, headers: { get: (n) => (n === 'X-SSH-Gete-Session-Key' ? 'k-1' : null) } }), 'sgadm_x', (k) => {
    key = k;
  });
  assert.equal(status, 204);
  assert.equal(key, 'k-1');
  let failedKey = 'unset';
  await login.login(async () => ({ status: 401, headers: { get: () => 'nope' } }), 'x', (k) => {
    failedKey = k;
  });
  assert.equal(failedKey, 'unset');
  const store = {};
  login.saveSessionKey({ localStorage: { setItem: (k, v) => (store[k] = v) } }, 'k-1');
  assert.equal(store[login.SESSION_KEY_ITEM], 'k-1');
  // localStorage unavailable: falls back to sessionStorage.
  const ss = {};
  login.saveSessionKey({ localStorage: { setItem: () => { throw new Error('denied'); } }, sessionStorage: { setItem: (k, v) => (ss[k] = v) } }, 'k-2');
  assert.equal(ss[login.SESSION_KEY_ITEM], 'k-2');
});
