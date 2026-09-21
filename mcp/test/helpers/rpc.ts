/**
 * A tiny JSON-RPC client over stdio for the built server — used by the scripted-session canary
 * (stdout must parse line by line as JSON-RPC) and by the live integration suite.
 */
import { spawn } from 'node:child_process';
import type { ChildProcessWithoutNullStreams } from 'node:child_process';
import path from 'node:path';
import { fileURLToPath } from 'node:url';

const here = path.dirname(fileURLToPath(import.meta.url));
export const MCP_ROOT = path.resolve(here, '..', '..');
export const DIST_ENTRY = path.join(MCP_ROOT, 'dist', 'index.js');

export type RpcResponse = { jsonrpc: '2.0'; id?: number; result?: unknown; error?: { code: number; message: string } };

export type ToolCallResult = { content: Array<{ type: string; text: string }>; isError?: boolean };

export class StdioRpcClient {
  readonly child: ChildProcessWithoutNullStreams;
  readonly stderr: string[] = [];
  readonly rawStdoutLines: string[] = [];
  #nextId = 1;
  #pending = new Map<number, { resolve: (r: RpcResponse) => void; reject: (e: Error) => void }>();
  #buffer = '';
  #exited: Promise<number | null>;

  constructor(env: NodeJS.ProcessEnv, args: string[] = ['serve']) {
    this.child = spawn(process.execPath, [DIST_ENTRY, ...args], { env: { ...env, PATH: process.env.PATH ?? '' }, stdio: ['pipe', 'pipe', 'pipe'] });
    this.child.stdout.setEncoding('utf8');
    this.child.stderr.setEncoding('utf8');
    this.child.stdout.on('data', (chunk: string) => {
      this.#buffer += chunk;
      let idx = this.#buffer.indexOf('\n');
      while (idx >= 0) {
        const line = this.#buffer.slice(0, idx);
        this.#buffer = this.#buffer.slice(idx + 1);
        if (line.trim() !== '') {
          this.rawStdoutLines.push(line);
          this.#onLine(line);
        }
        idx = this.#buffer.indexOf('\n');
      }
    });
    this.child.stderr.on('data', (chunk: string) => {
      this.stderr.push(chunk);
    });
    this.#exited = new Promise(resolve => {
      this.child.on('exit', code => {
        for (const p of this.#pending.values()) {
          p.reject(new Error(`server exited with ${String(code)} before answering; stderr: ${this.stderr.join('')}`));
        }
        this.#pending.clear();
        resolve(code);
      });
    });
  }

  #onLine(line: string): void {
    let msg: RpcResponse;
    try {
      msg = JSON.parse(line) as RpcResponse;
    } catch {
      for (const p of this.#pending.values()) {
        p.reject(new Error(`stdout carried a non-JSON line: ${line.slice(0, 200)}`));
      }
      this.#pending.clear();
      return;
    }
    if (msg.id !== undefined && this.#pending.has(msg.id)) {
      const p = this.#pending.get(msg.id);
      this.#pending.delete(msg.id);
      p?.resolve(msg);
    }
  }

  request(method: string, params: unknown = {}, timeoutMs = 60_000): Promise<RpcResponse> {
    const id = this.#nextId++;
    return new Promise((resolve, reject) => {
      const timer = setTimeout(() => {
        this.#pending.delete(id);
        reject(new Error(`no answer to ${method} within ${String(timeoutMs)} ms; stderr: ${this.stderr.join('')}`));
      }, timeoutMs);
      this.#pending.set(id, {
        resolve: r => {
          clearTimeout(timer);
          resolve(r);
        },
        reject: e => {
          clearTimeout(timer);
          reject(e);
        },
      });
      this.child.stdin.write(`${JSON.stringify({ jsonrpc: '2.0', id, method, params })}\n`);
    });
  }

  notify(method: string, params: unknown = {}): void {
    this.child.stdin.write(`${JSON.stringify({ jsonrpc: '2.0', method, params })}\n`);
  }

  async initialize(): Promise<RpcResponse> {
    const res = await this.request('initialize', { protocolVersion: '2025-06-18', capabilities: {}, clientInfo: { name: 'test', version: '1' } });
    this.notify('notifications/initialized');
    return res;
  }

  async listTools(): Promise<Array<{ name: string; description: string; inputSchema: Record<string, unknown> }>> {
    const res = await this.request('tools/list');
    return (res.result as { tools: Array<{ name: string; description: string; inputSchema: Record<string, unknown> }> }).tools;
  }

  /** Call a tool and parse the envelope out of the one text content block. */
  async call(name: string, args: unknown = {}, timeoutMs?: number): Promise<{ raw: ToolCallResult; envelope: Record<string, unknown> }> {
    const res = await this.request('tools/call', { name, arguments: args }, timeoutMs);
    if (res.error !== undefined) {
      throw new Error(`tools/call ${name} answered a JSON-RPC error: ${res.error.message}`);
    }
    const raw = res.result as ToolCallResult;
    const text = raw.content[0]?.text ?? '';
    return { raw, envelope: JSON.parse(text) as Record<string, unknown> };
  }

  async close(): Promise<number | null> {
    this.child.stdin.end();
    this.child.kill('SIGTERM');
    return this.#exited;
  }
}
