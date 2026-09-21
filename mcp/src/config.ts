/**
 * Configuration — pm/mcp.mdx §14.
 *
 * Every variable is read ONCE at startup into a frozen object and printed to stderr in the banner
 * (the key only as a fingerprint). A setting that can change mid-session explains a bug badly.
 */
import os from 'node:os';
import path from 'node:path';

export type Target = 'local' | 'remote';
export type LogLevel = 'error' | 'warn' | 'info' | 'debug';

export type Config = Readonly<{
  apiUrl: string;
  target: Target;
  allowWrite: boolean;
  /** EZBKMCP_ALLOW_ADMIN=1 on top of the write switch: the one admin-tier tool, ezb_delete_transactions (§9.5b). */
  allowAdmin: boolean;
  allowRemote: boolean;
  maxChanges: number;
  maxRows: number;
  maxBytes: number;
  timeoutMs: number;
  timezone: string | undefined;
  promptFile: string | undefined;
  logLevel: LogLevel;
  logDir: string;
  credentialsFile: string;
  statementsDir: string | undefined;
}>;

export class ConfigError extends Error {
  readonly fix: string;

  constructor(message: string, fix: string) {
    super(message);
    this.name = 'ConfigError';
    this.fix = fix;
  }
}

export const DEFAULT_API_URL = 'http://127.0.0.1:8080';

/** The loopback classifier (§7.3): 127.0.0.0/8, ::1 and localhost, nothing else. */
export function isLoopbackHost(hostname: string): boolean {
  const h = hostname.toLowerCase();
  if (h === 'localhost' || h === '::1' || h === '[::1]') {
    return true;
  }
  return /^127\.\d{1,3}\.\d{1,3}\.\d{1,3}$/.test(h);
}

function positiveInt(raw: string | undefined, fallback: number, name: string): number {
  if (raw === undefined || raw.trim() === '') {
    return fallback;
  }
  if (!/^\d+$/.test(raw.trim()) || Number(raw) <= 0) {
    throw new ConfigError(`${name} must be a positive integer, got "${raw}".`, `unset ${name} or set it to a number`);
  }
  return Number(raw);
}

function logLevel(raw: string | undefined): LogLevel {
  if (raw === undefined || raw === '') {
    return 'info';
  }
  if (raw === 'error' || raw === 'warn' || raw === 'info' || raw === 'debug') {
    return raw;
  }
  throw new ConfigError(`EZBKMCP_LOG_LEVEL must be error, warn, info or debug, got "${raw}".`, 'unset EZBKMCP_LOG_LEVEL');
}

function blank(v: string | undefined): string | undefined {
  const t = v?.trim();
  return t === undefined || t === '' ? undefined : t;
}

/**
 * Resolve config, or throw. Called before the transport is attached, so a misconfiguration is a
 * clean refusal on stderr rather than a server that connects and then 500s on every call (§15).
 */
export function loadConfig(env: NodeJS.ProcessEnv = process.env): Config {
  const apiUrl = blank(env.EZBKMCP_API_URL) ?? DEFAULT_API_URL;

  let url: URL;
  try {
    url = new URL(apiUrl);
  } catch {
    throw new ConfigError(`EZBKMCP_API_URL is not a URL: "${apiUrl}".`, `unset it to use the default ${DEFAULT_API_URL}`);
  }
  if (url.protocol !== 'http:' && url.protocol !== 'https:') {
    throw new ConfigError(`EZBKMCP_API_URL must be http: or https:, got "${apiUrl}".`, `unset it to use the default ${DEFAULT_API_URL}`);
  }

  const loopback = isLoopbackHost(url.hostname);
  const allowRemote = env.EZBKMCP_ALLOW_REMOTE === '1';

  // §7.3 — the remote tripwire. A non-loopback URL is somebody's server, and reaching it by
  // accident is how a model answers about the wrong ledger with total confidence.
  if (!loopback) {
    if (!allowRemote) {
      throw new ConfigError(
        `EZBKMCP_API_URL points at ${url.host}, which is not this machine.`,
        'set EZBKMCP_ALLOW_REMOTE=1 if that is deliberate — it also requires https: and disables writes',
      );
    }
    if (url.protocol !== 'https:') {
      throw new ConfigError(`Refusing to send the API secret key to ${apiUrl} in cleartext.`, 'use https: for a remote target');
    }
  }

  const target: Target = loopback ? 'local' : 'remote';

  // A remote install is READ-ONLY from here, full stop — even with both write switches on.
  const allowWrite = env.EZBKMCP_ALLOW_WRITE === '1' && target === 'local';
  const allowAdmin = allowWrite && env.EZBKMCP_ALLOW_ADMIN === '1';

  const home = os.homedir();

  return Object.freeze({
    apiUrl: url.origin,
    target,
    allowWrite,
    allowAdmin,
    allowRemote,
    maxChanges: positiveInt(env.EZBKMCP_MAX_CHANGES, 200, 'EZBKMCP_MAX_CHANGES'),
    maxRows: positiveInt(env.EZBKMCP_MAX_ROWS, 1000, 'EZBKMCP_MAX_ROWS'),
    maxBytes: positiveInt(env.EZBKMCP_MAX_BYTES, 1048576, 'EZBKMCP_MAX_BYTES'),
    timeoutMs: positiveInt(env.EZBKMCP_TIMEOUT_MS, 30000, 'EZBKMCP_TIMEOUT_MS'),
    timezone: blank(env.EZBKMCP_TIMEZONE),
    promptFile: blank(env.EZBKMCP_PROMPT_FILE),
    logLevel: logLevel(blank(env.EZBKMCP_LOG_LEVEL)),
    logDir: blank(env.EZBKMCP_LOG_DIR) ?? path.join(home, 'T', '_ezbookkeeping'),
    credentialsFile:
      blank(env.EZBKMCP_CREDENTIALS_FILE) ?? blank(env.EZBK_CREDENTIALS_FILE) ?? path.join(home, '.credentials', 'ezbookkeeping.json'),
    statementsDir: blank(env.EZBK_STATEMENTS_DIR),
  });
}
