/**
 * A real ezBookkeeping server on an ephemeral port with a temp work dir, temp SQLite, temp
 * credentials and one synthetic user — pm/mcp.mdx §17 "Integration".
 *
 * It needs the Go binary: EZBK_SERVER_BIN, else <repo>/ezbookkeeping. When neither exists, or the
 * binary predates the machine plane, `startLiveServer()` returns null and the caller skips with a
 * reason. Nothing here ever touches the repo's data/, the real ~/.credentials/, or any real books.
 */
import { spawn } from 'node:child_process';
import type { ChildProcess } from 'node:child_process';
import crypto from 'node:crypto';
import fs from 'node:fs';
import http from 'node:http';
import net from 'node:net';
import os from 'node:os';
import path from 'node:path';

import { MCP_ROOT } from './rpc.js';

export const REPO_ROOT = path.resolve(MCP_ROOT, '..');

export type LiveServer = {
  apiUrl: string;
  port: number;
  workDir: string;
  credentialsFile: string;
  stateDir: string;
  errorFile: string;
  key: string;
  /** The env a spawned MCP needs to talk to this server. */
  mcpEnv: (extra?: Record<string, string>) => NodeJS.ProcessEnv;
  /** One raw machine-plane request, for the gate canaries. */
  plane: (method: string, route: string, opts?: { headers?: Record<string, string>; body?: unknown; key?: string | null }) => Promise<{ status: number; text: string; json: unknown }>;
  stop: () => Promise<void>;
};

export function serverBinary(): string | null {
  const fromEnv = process.env.EZBK_SERVER_BIN?.trim();
  if (fromEnv !== undefined && fromEnv !== '' && fs.existsSync(fromEnv)) {
    return fromEnv;
  }
  const inRepo = path.join(REPO_ROOT, 'ezbookkeeping');
  return fs.existsSync(inRepo) ? inRepo : null;
}

function freePort(): Promise<number> {
  return new Promise((resolve, reject) => {
    const srv = net.createServer();
    srv.listen(0, '127.0.0.1', () => {
      const address = srv.address();
      const port = typeof address === 'object' && address !== null ? address.port : 0;
      srv.close(() => {
        resolve(port);
      });
    });
    srv.on('error', reject);
  });
}

function rawRequest(url: string, method: string, headers: Record<string, string>, body: string | undefined): Promise<{ status: number; text: string }> {
  return new Promise((resolve, reject) => {
    const req = http.request(url, { method, headers: { ...headers, ...(body === undefined ? {} : { 'Content-Length': String(Buffer.byteLength(body)) }) } }, res => {
      const chunks: Buffer[] = [];
      res.on('data', (c: Buffer) => chunks.push(c));
      res.on('end', () => {
        resolve({ status: res.statusCode ?? 0, text: Buffer.concat(chunks).toString('utf8') });
      });
    });
    req.on('error', reject);
    if (body !== undefined) {
      req.write(body);
    }
    req.end();
  });
}

async function waitFor(fn: () => Promise<boolean>, timeoutMs: number, child: ChildProcess): Promise<boolean> {
  const deadline = Date.now() + timeoutMs;
  while (Date.now() < deadline) {
    if (child.exitCode !== null) {
      return false;
    }
    try {
      if (await fn()) {
        return true;
      }
    } catch {
      // not up yet
    }
    await new Promise(r => setTimeout(r, 250));
  }
  return false;
}

/** The synthetic user every live test binds to. Invented names only (apis.mdx §20). */
export const SYNTHETIC_USER = { username: 'operator', email: 'operator@example.com', nickname: 'Operator', password: 'synthetic-pass-123' };

const SYNTHETIC_CATEGORIES = [
  { name: 'Salary', type: 1, subs: ['Paycheck'] },
  { name: 'Food', type: 2, subs: ['Groceries', 'Restaurants'] },
  { name: 'Home', type: 2, subs: ['Home Repair', 'Other'] },
  { name: 'Transfer', type: 3, subs: ['General Transfer'] },
];

export type LiveOptions = {
  allowWrite?: boolean;
  /** Extra EBK_* overrides for the server, e.g. the upstream doors. */
  serverEnv?: Record<string, string>;
  registerUser?: boolean;
};

export async function startLiveServer(opts: LiveOptions = {}): Promise<LiveServer | null> {
  const bin = serverBinary();
  if (bin === null) {
    return null;
  }

  const root = fs.mkdtempSync(path.join(os.tmpdir(), 'ezbkmcp-live-'));
  const workDir = path.join(root, 'srv');
  const stateDir = path.join(root, 'state');
  const staticDir = path.join(workDir, 'dist');
  for (const d of [workDir, path.join(workDir, 'data'), path.join(workDir, 'log'), path.join(workDir, 'storage'), staticDir, stateDir]) {
    fs.mkdirSync(d, { recursive: true, mode: 0o700 });
  }
  fs.writeFileSync(path.join(staticDir, 'index.html'), '<!doctype html><title>test</title>\n');
  const secretFile = path.join(workDir, 'secret_key');
  fs.writeFileSync(secretFile, crypto.randomBytes(24).toString('hex'), { mode: 0o600 });
  const credentialsFile = path.join(root, 'creds.json');
  const errorFile = path.join(stateDir, 'error.err');
  const port = await freePort();
  const apiUrl = `http://127.0.0.1:${String(port)}`;

  const env: NodeJS.ProcessEnv = {
    PATH: process.env.PATH ?? '',
    HOME: root,
    EBK_WORK_DIR: workDir,
    EBK_SERVER_HTTP_ADDR: '127.0.0.1',
    EBK_SERVER_HTTP_PORT: String(port),
    EBK_SERVER_DOMAIN: 'localhost',
    EBK_SERVER_STATIC_ROOT_PATH: staticDir,
    EBKCFP_SECURITY_SECRET_KEY: secretFile,
    EZBK_CREDENTIALS_FILE: credentialsFile,
    EZBK_STATE_DIR: stateDir,
    EZBK_ERROR_FILE: errorFile,
    EZBK_MACHINE_ALLOW_WRITE: opts.allowWrite === false ? '0' : '1',
    EZBK_MACHINE_ALLOW_ADMIN: '0',
    ...(opts.serverEnv ?? {}),
  };

  const out = fs.openSync(path.join(root, 'server.out'), 'a');
  const child = spawn(bin, ['--conf-path', 'conf/ezbookkeeping.ini', 'server', 'run'], { cwd: REPO_ROOT, env, stdio: ['ignore', out, out] });

  const stop = async (): Promise<void> => {
    if (child.exitCode === null) {
      child.kill('SIGTERM');
      await new Promise<void>(resolve => {
        const t = setTimeout(() => {
          child.kill('SIGKILL');
          resolve();
        }, 5000);
        child.on('exit', () => {
          clearTimeout(t);
          resolve();
        });
      });
    }
    fs.closeSync(out);
    fs.rmSync(root, { recursive: true, force: true });
  };

  const healthy = await waitFor(
    async () => {
      const r = await rawRequest(`${apiUrl}/healthz.json`, 'GET', {}, undefined);
      return r.status === 200 && r.text.includes('"ok"');
    },
    60_000,
    child,
  );
  if (!healthy) {
    await stop();
    throw new Error(`the live server did not become healthy; see ${path.join(root, 'server.out')}`);
  }

  // The plane mints the key into EZBK_CREDENTIALS_FILE at boot (apis.mdx §5.4).
  const armed = await waitFor(async () => fs.existsSync(credentialsFile), 10_000, child);
  if (!armed) {
    await stop();
    return null; // a binary without the machine plane
  }
  const doc = JSON.parse(fs.readFileSync(credentialsFile, 'utf8')) as { ezbookkeeping?: { machine?: { api_key?: string } } };
  const key = doc.ezbookkeeping?.machine?.api_key ?? '';

  const plane: LiveServer['plane'] = async (method, route, o = {}) => {
    const headers: Record<string, string> = { Accept: 'application/json', ...(o.headers ?? {}) };
    const k = o.key === undefined ? key : o.key;
    if (k !== null) {
      headers['X-Ezbk-Api-Key'] = k;
    }
    let body: string | undefined;
    if (o.body !== undefined) {
      body = JSON.stringify(o.body);
      headers['Content-Type'] = 'application/json';
    }
    const r = await rawRequest(`${apiUrl}/machine/v1${route}`, method, headers, body);
    let json: unknown = null;
    try {
      json = JSON.parse(r.text);
    } catch {
      json = null;
    }
    return { status: r.status, text: r.text, json };
  };

  const ping = await plane('GET', '/ping');
  if (ping.status !== 200) {
    await stop();
    return null;
  }

  if (opts.registerUser !== false) {
    const categories = SYNTHETIC_CATEGORIES.map(c => ({
      name: c.name,
      type: c.type,
      icon: '1',
      color: '000000',
      subCategories: c.subs.map(s => ({ name: s, type: c.type, icon: '1', color: '000000' })),
    }));
    const body = JSON.stringify({ ...SYNTHETIC_USER, language: 'en', defaultCurrency: 'USD', firstDayOfWeek: 0, categories });
    const r = await rawRequest(`${apiUrl}/api/register.json`, 'POST', { 'Content-Type': 'application/json', 'X-Timezone-Offset': '0' }, body);
    if (r.status !== 200 || !r.text.includes('"success":true')) {
      await stop();
      throw new Error(`registering the synthetic user failed: ${r.status} ${r.text.slice(0, 200)}`);
    }
  }

  const mcpEnv = (extra: Record<string, string> = {}): NodeJS.ProcessEnv => ({
    PATH: process.env.PATH ?? '',
    HOME: root,
    EZBKMCP_API_URL: apiUrl,
    EZBKMCP_CREDENTIALS_FILE: credentialsFile,
    EZBKMCP_LOG_DIR: stateDir,
    EZBK_ERROR_FILE: errorFile,
    EZBKMCP_TIMEZONE: 'UTC',
    ...extra,
  });

  return { apiUrl, port, workDir, credentialsFile, stateDir, errorFile, key, mcpEnv, plane, stop };
}
