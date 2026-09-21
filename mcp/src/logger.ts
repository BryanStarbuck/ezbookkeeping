/**
 * Logging — pm/mcp.mdx §8.1 and §16.
 *
 * THIS MODULE HAS NO STDOUT PATH, AND THAT IS THE POINT.
 *
 * stdout is the JSON-RPC wire. One `console.log` anywhere in this process — ours or dragged in from
 * a dependency — corrupts the stream, and the symptom is not an error: the client simply goes quiet.
 * The CLI has an `out()` function; it is the single thing from the CLI's shape that must not be
 * ported here. Everything below writes to stderr or to a file, and a canary greps the BUILT bundle
 * to prove it stayed that way.
 *
 * Files: ~/T/_ezbookkeeping/mcp.info (INFO and one CALL line per tool call; file only) and mcp.err
 * (WARN, ERROR and every denial; also stderr). Append-only, 0600, rotated at 8 MB with five
 * generations. Logging can never crash the server.
 */
import fs from 'node:fs';
import path from 'node:path';

import { errorFileFor } from './errfile/index.js';

const errors = errorFileFor('mcp/src/logger.ts');

export const ROTATE_BYTES = 8 * 1024 * 1024;
export const GENERATIONS = 5;

const LEVEL_ORDER = { error: 0, warn: 1, info: 2, debug: 3 } as const;
export type Level = keyof typeof LEVEL_ORDER;

export type LoggerOptions = {
  dir: string;
  level: Level;
};

function rotateIfNeeded(file: string): void {
  try {
    if (!fs.existsSync(file) || fs.statSync(file).size < ROTATE_BYTES) {
      return;
    }
    for (let i = GENERATIONS - 1; i >= 1; i--) {
      const from = `${file}.${i}`;
      if (fs.existsSync(from)) {
        fs.renameSync(from, `${file}.${i + 1}`);
      }
    }
    fs.renameSync(file, `${file}.1`);
  } catch (err) {
    // A rotation that fails is a bigger file, not a dead server.
    errors.warn('rotating the MCP log file', err);
  }
}

export class Logger {
  readonly #dir: string;
  readonly #level: Level;

  constructor({ dir, level }: LoggerOptions) {
    this.#dir = dir;
    this.#level = level;
  }

  /** Append to mcp.info or mcp.err. Every filesystem fault is swallowed and reported once. */
  #append(name: 'mcp.info' | 'mcp.err', line: string): void {
    try {
      fs.mkdirSync(this.#dir, { recursive: true, mode: 0o700 });
      const file = path.join(this.#dir, name);
      rotateIfNeeded(file);
      fs.appendFileSync(file, `${line}\n`, { mode: 0o600 });
    } catch (err) {
      errors.warn('appending to the MCP log file', err, { file: name });
    }
  }

  #enabled(level: Level): boolean {
    return LEVEL_ORDER[level] <= LEVEL_ORDER[this.#level];
  }

  /** File only. Never stderr — the operator did not ask for a running commentary. */
  info(message: string): void {
    if (this.#enabled('info')) {
      this.#append('mcp.info', `${new Date().toISOString()} INFO ${message}`);
    }
  }

  debug(message: string): void {
    if (this.#enabled('debug')) {
      this.#append('mcp.info', `${new Date().toISOString()} DEBUG ${message}`);
    }
  }

  /** File AND stderr — a warning the operator should see in the client's log. */
  warn(message: string): void {
    if (this.#enabled('warn')) {
      const line = `${new Date().toISOString()} WARN ${message}`;
      this.#append('mcp.err', line);
      process.stderr.write(`${line}\n`);
    }
  }

  error(message: string): void {
    const line = `${new Date().toISOString()} ERROR ${message}`;
    this.#append('mcp.err', line);
    process.stderr.write(`${line}\n`);
  }

  /** The one-line startup banner (§14). stderr, never stdout. */
  banner(message: string): void {
    process.stderr.write(`${message}\n`);
  }

  /** The audit line (§16.2). Allowed calls to mcp.info, denials to both. */
  audit(line: string, denied: boolean): void {
    this.#append('mcp.info', line);
    if (denied) {
      this.#append('mcp.err', line);
    }
  }
}
