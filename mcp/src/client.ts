/**
 * The ONE http client — pm/mcp.mdx §7.0 T9, §15.
 *
 * This is the only module in the process that opens a socket, and the only host it may open one to
 * is the configured machine plane (loopback unless the §7.3 tripwire was deliberately armed). A
 * canary greps the built bundle for host literals and for a second socket opener to keep that true.
 *
 * One HTTP call per tool call. No fan-out, no cache: the plane holds the books, and a copy here is
 * a second copy that can be stale (§9.4).
 */
import http from 'node:http';
import https from 'node:https';

import { fail, isErrorCode } from './envelope.js';

export const CLIENT_NAME = 'ezbookkeeping-mcp';
export const CLIENT_VERSION = '0.1.0';

/** The plane's envelope (apis.mdx §7.1), or a raw file answer (CSV/TSV export). */
export type PlaneResponse = {
  ok: boolean;
  data?: unknown;
  error?: { code?: string; message?: string; hint?: string; details?: unknown; upstreamCode?: number };
  meta?: Record<string, unknown>;
  /** Set when the route answered a file rather than JSON (GET /transactions/export, /data/export). */
  raw?: { contentType: string; fileName: string | undefined; text: string };
};

export type QueryValue = string | number | boolean | string[] | undefined;

export type RequestOptions = {
  method?: 'GET' | 'POST' | 'PATCH' | 'PUT' | 'DELETE';
  query?: Record<string, QueryValue>;
  body?: unknown;
  /**
   * Exempt from the per-call timeout (§14): extraction and the import applies are long by nature,
   * and a client that gives up at 30 s reports a failure about a job that is running perfectly.
   */
  noTimeout?: boolean;
};

export type ClientOptions = {
  apiUrl: string;
  key: string;
  timeoutMs: number;
  maxBytes: number;
  timezone: string | undefined;
};


type RawResponse = { status: number; headers: Record<string, string>; text: string };

/**
 * One HTTP request over node:http / node:https. Not `fetch`: Node's fetch (undici) stamps
 * `Sec-Fetch-Mode: cors` on every request and will not let a caller remove it, and the plane's
 * origin gate (apis.mdx §5.7) treats any Sec-Fetch-* header as a browser and answers 404. Our
 * clients send no browser headers at all; presence is the signal.
 */
function httpRequest(url: URL, method: string, headers: Record<string, string>, body: string | undefined, signal: AbortSignal | undefined): Promise<RawResponse> {
  return new Promise((resolve, reject) => {
    const lib = url.protocol === 'https:' ? https : http;
    const req = lib.request(
      url,
      {
        method,
        headers: { ...headers, ...(body === undefined ? {} : { 'Content-Length': String(Buffer.byteLength(body)) }) },
        ...(signal === undefined ? {} : { signal }),
      },
      res => {
        const chunks: Buffer[] = [];
        res.on('data', (c: Buffer) => {
          chunks.push(c);
        });
        res.on('end', () => {
          const flat: Record<string, string> = {};
          for (const [k, v] of Object.entries(res.headers)) {
            if (typeof v === 'string') {
              flat[k] = v;
            } else if (Array.isArray(v)) {
              flat[k] = v.join(', ');
            }
          }
          resolve({ status: res.statusCode ?? 0, headers: flat, text: Buffer.concat(chunks).toString('utf8') });
        });
        res.on('error', reject);
      },
    );
    req.on('error', reject);
    if (body !== undefined) {
      req.write(body);
    }
    req.end();
  });
}

export class MachinePlaneClient {
  readonly #opts: ClientOptions;

  constructor(opts: ClientOptions) {
    this.#opts = opts;
  }

  /** The advisory identity every call carries (apis.mdx §5.6, §8.1). */
  static clientHeader(): string {
    return `${CLIENT_NAME}/${CLIENT_VERSION}`;
  }

  async request(route: string, opts: RequestOptions = {}): Promise<PlaneResponse> {
    const url = new URL(`${this.#opts.apiUrl}/machine/v1${route}`);
    for (const [name, value] of Object.entries(opts.query ?? {})) {
      if (value === undefined) {
        continue;
      }
      if (Array.isArray(value)) {
        if (value.length > 0) {
          url.searchParams.set(name, value.join(','));
        }
        continue;
      }
      url.searchParams.set(name, String(value));
    }

    const method = opts.method ?? 'GET';
    const signal = opts.noTimeout ? undefined : AbortSignal.timeout(this.#opts.timeoutMs);
    const headers: Record<string, string> = {
      // A header, never a query string: query strings land in logs and shell history (apis.mdx §5.6).
      'X-Ezbk-Api-Key': this.#opts.key,
      'X-Ezbk-Client': MachinePlaneClient.clientHeader(),
      Accept: 'application/json',
    };
    if (this.#opts.timezone !== undefined) {
      headers['X-Timezone-Name'] = this.#opts.timezone;
    }
    if (opts.body !== undefined) {
      headers['Content-Type'] = 'application/json';
    }

    let response: RawResponse;
    try {
      response = await httpRequest(url, method, headers, opts.body === undefined ? undefined : JSON.stringify(opts.body), signal);
    } catch (err) {
      if ((err as Error).name === 'TimeoutError' || (err as Error).name === 'AbortError') {
        throw fail(
          'upstream_error',
          `The app did not answer ${method} ${route} within ${this.#opts.timeoutMs} ms.`,
          'the request may still be running; check with the operator before retrying',
        );
      }
      // The model must be told to stop, not to retry: the app being down does not resolve on its
      // own, and this server never starts it (§9.8).
      throw fail(
        'not_ready',
        `ezBookkeeping is not reachable at ${this.#opts.apiUrl}.`,
        'ask the operator to run `ezbk up`; this server never starts the app',
      );
    }

    const text = response.text;
    if (text.length > this.#opts.maxBytes) {
      throw fail(
        'upstream_error',
        `The app returned ${text.length} bytes, over this server's ${this.#opts.maxBytes} byte cap (EZBKMCP_MAX_BYTES).`,
        'narrow the date range, pass a smaller limit, or page with cursor',
      );
    }

    const contentType = response.headers['content-type'] ?? '';
    if (!contentType.includes('json')) {
      if (response.status >= 400) {
        throw fail(
          'upstream_error',
          `The app answered ${response.status} with a non-JSON body for ${method} ${route}.`,
          'check that ezBookkeeping, and not something else, holds that port',
        );
      }
      const disposition = response.headers['content-disposition'] ?? '';
      const match = /filename=([^;]+)/.exec(disposition);
      return { ok: true, raw: { contentType, fileName: match?.[1]?.trim(), text } };
    }

    let parsed: PlaneResponse;
    try {
      parsed = JSON.parse(text) as PlaneResponse;
    } catch {
      throw fail(
        'upstream_error',
        `The app answered ${response.status} with malformed JSON for ${method} ${route}.`,
        'check that ezBookkeeping, and not something else, holds that port',
      );
    }

    if (typeof parsed !== 'object' || parsed === null || !('ok' in parsed)) {
      // Upstream's own {success, errorCode} shape: the binary predates the machine plane (§13).
      throw fail(
        'not_ready',
        'The app answered without the machine-plane envelope; this build of ezBookkeeping predates the machine plane.',
        'rebuild and restart the app: `just build` then `ezbk stop && ezbk up`',
      );
    }

    if (parsed.ok) {
      return parsed;
    }

    const code = parsed.error?.code ?? '';
    const message = parsed.error?.message;
    const details = parsed.error?.details;

    if (code === 'unauthorized') {
      // Distinct from not_ready on purpose: the app IS running, it is holding a different key.
      throw fail(
        'unauthorized',
        "The app rejected this server's API secret key.",
        'the app is holding an older key than the credentials file — ask the operator to restart it: `ezbk stop && ezbk up`',
      );
    }

    if (code === 'not_found' && (message === undefined || message === '')) {
      // The gate ladder's bare 404: the plane is not armed, or refused this socket (apis.mdx §5.7).
      throw fail(
        'not_ready',
        'The machine plane refused the call before reading it (not armed, or the request did not arrive on loopback).',
        'ask the operator to restart the app (`ezbk stop && ezbk up`) and read ~/T/_ezbookkeeping/server.log',
      );
    }

    // The ceiling (apis.mdx §9.1) arrives as conflict with the real count; the MCP names it (§12.3).
    if (code === 'conflict' && details !== null && typeof details === 'object' && 'would_change' in (details as object)) {
      const d = details as { would_change?: number; max_changes?: number };
      throw fail(
        'too_many_changes',
        `${String(d.would_change)} changes would be made; the ceiling is ${String(d.max_changes)}.`,
        'raise max_changes deliberately, or narrow the selection',
        details,
      );
    }

    if (code === 'write_disabled') {
      throw fail(
        'write_disabled',
        message ?? 'The write tier is off on the server.',
        'restart the app with `ezbk stop && ezbk up --allow-write` (EZBK_MACHINE_ALLOW_WRITE=1) and set EZBKMCP_ALLOW_WRITE=1 for this server, then retry',
        details,
      );
    }

    if (code === 'internal') {
      throw fail('upstream_error', message ?? `The app failed on ${method} ${route}.`, parsed.error?.hint ?? 'read ~/T/_ezbookkeeping/server.log', details);
    }

    throw fail(isErrorCode(code) ? code : 'upstream_error', message ?? `The app refused ${method} ${route}.`, parsed.error?.hint, details);
  }
}
