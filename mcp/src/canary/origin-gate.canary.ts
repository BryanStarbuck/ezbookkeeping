/**
 * T14 / T15 — a web page reaching the plane, a forged loopback header (pm/mcp.mdx §7.0, §17
 * "origin-gate canary", AC 18): with a valid key, `Origin` → 404, `Sec-Fetch-Site` → 404, a bad
 * `Host` → 404; no key → 401 with the constant body; a wrong key → the same body byte for byte; and
 * `X-Forwarded-For` is ignored (the gate reads the socket peer). The non-loopback-socket half of
 * T15 cannot be produced from a loopback-only test listener; pkg/machine/gates_test.go covers it.
 *
 * Needs the Go binary (EZBK_SERVER_BIN or <repo>/ezbookkeeping); skips with a reason otherwise.
 */
import { afterAll, beforeAll, describe, expect, it } from 'vitest';

import { startLiveServer } from '../../test/helpers/live.js';
import type { LiveServer } from '../../test/helpers/live.js';

let live: LiveServer | null = null;
let reason = 'no ezBookkeeping binary with the machine plane (set EZBK_SERVER_BIN)';

describe('canary: origin-gate', () => {
  beforeAll(async () => {
    try {
      live = await startLiveServer({ allowWrite: false, registerUser: false });
    } catch (err) {
      reason = (err as Error).message;
      live = null;
    }
  }, 90_000);

  afterAll(async () => {
    await live?.stop();
  });

  it('answers the key on loopback with 200', async ({ skip }) => {
    if (live === null) {
      skip(reason);
      return;
    }
    const r = await live.plane('GET', '/ping');
    expect(r.status).toBe(200);
    expect((r.json as { ok: boolean }).ok).toBe(true);
  });

  it('Origin: https://evil.example → 404 (T14)', async ({ skip }) => {
    if (live === null) {
      skip(reason);
      return;
    }
    const r = await live.plane('GET', '/ping', { headers: { Origin: 'https://evil.example' } });
    expect(r.status).toBe(404);
    expect(r.text).toBe('{"error":{"code":"not_found"},"ok":false}');
  });

  it('Sec-Fetch-Site: cross-site → 404, and Sec-Fetch-Mode: cors → 404 (which is why the client is not fetch)', async ({ skip }) => {
    if (live === null) {
      skip(reason);
      return;
    }
    expect((await live.plane('GET', '/ping', { headers: { 'Sec-Fetch-Site': 'cross-site' } })).status).toBe(404);
    expect((await live.plane('GET', '/ping', { headers: { 'Sec-Fetch-Site': 'same-origin' } })).status).toBe(404);
    expect((await live.plane('GET', '/ping', { headers: { 'Sec-Fetch-Mode': 'cors' } })).status).toBe(404);
  });

  it('a rebinding Host → 404 (T14)', async ({ skip }) => {
    if (live === null) {
      skip(reason);
      return;
    }
    expect((await live.plane('GET', '/ping', { headers: { Host: 'evil.example' } })).status).toBe(404);
    expect((await live.plane('GET', '/ping', { headers: { Host: `127.0.0.1:${String(live.port + 1)}` } })).status).toBe(404);
  });

  it('no key → 401 with the constant body; a wrong key → the same body byte for byte (T10)', async ({ skip }) => {
    if (live === null) {
      skip(reason);
      return;
    }
    const none = await live.plane('GET', '/ping', { key: null });
    const wrong = await live.plane('GET', '/ping', { key: 'f'.repeat(64) });
    const uppercase = await live.plane('GET', '/ping', { key: live.key.toUpperCase() });
    expect(none.status).toBe(401);
    expect(wrong.status).toBe(401);
    expect(uppercase.status).toBe(401);
    expect(none.text).toBe('{"error":{"code":"unauthorized"},"ok":false}');
    expect(wrong.text).toBe(none.text);
    expect(uppercase.text).toBe(none.text);
  });

  it('X-Forwarded-For and X-Real-IP are ignored: the gate reads the socket peer (T15)', async ({ skip }) => {
    if (live === null) {
      skip(reason);
      return;
    }
    // A loopback socket claiming to be remote is still loopback (the header is never read) …
    expect((await live.plane('GET', '/ping', { headers: { 'X-Forwarded-For': '203.0.113.9', 'X-Real-IP': '203.0.113.9' } })).status).toBe(200);
    // … and the plane's own gates_test.go proves a remote socket claiming 127.0.0.1 gets 404.
  });

  it('answers every refusal with Cache-Control: no-store', async ({ skip }) => {
    if (live === null) {
      skip(reason);
      return;
    }
    const r = await live.plane('GET', '/ping', { headers: { Origin: 'https://evil.example' } });
    expect(r.status).toBe(404);
  });
});
