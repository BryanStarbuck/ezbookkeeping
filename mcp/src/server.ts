/**
 * The JSON-RPC surface — pm/mcp.mdx §8, §9.3, §12.
 *
 * The SDK's low-level `Server` with explicit request handlers, not `McpServer.registerTool`: the
 * JSON Schema the model reads is hand-authored and served verbatim (§8.2); Zod is only the runtime
 * parse inside gate 5. Capabilities are `{ tools: {} }` and nothing else, so every prompts/* and
 * resources/* request answers -32601 by not being declared.
 */
import { Server } from '@modelcontextprotocol/sdk/server/index.js';
import { CallToolRequestSchema, ListToolsRequestSchema } from '@modelcontextprotocol/sdk/types.js';

import { auditLine } from './audit.js';
import type { MachinePlaneClient } from './client.js';
import type { Config } from './config.js';
import { ERROR_FILE_HINT, ToolError } from './envelope.js';
import type { Envelope, Meta } from './envelope.js';
import { errorFileFor } from './errfile/index.js';
import { checkConfirm, checkInput, checkMode, checkNotForeign } from './gates.js';
import type { Logger } from './logger.js';
import { findTool, TOOLS } from './tools/registry.js';
import type { ToolDef } from './tools/tool.js';

const errors = errorFileFor('mcp/src/server.ts');

export const SERVER_NAME = 'ezbookkeeping';
export const SERVER_VERSION = '0.1.0';

export type HostOptions = {
  config: Config;
  client: MachinePlaneClient;
  logger: Logger;
  keyFingerprint: string;
  instructions: string;
};

export type ToolContent = { content: Array<{ type: 'text'; text: string }>; isError?: boolean };

/**
 * When the write tier is off, a write tool is still LISTED — with its description saying so and
 * naming both switches. A tool that vanishes teaches the model nothing (§7.1 gate 4).
 */
export function describeForListing(tool: ToolDef, config: Config): string {
  if (tool.tier !== 'write') {
    return tool.description;
  }
  if (config.target === 'remote') {
    return `${tool.description} CURRENTLY DISABLED: this server is pointed at a remote install, which is read-only.`;
  }
  if (!config.allowWrite) {
    return `${tool.description} CURRENTLY DISABLED: the write tier is off. To enable it the operator must start the app with \`ezbk up --allow-write\` (EZBK_MACHINE_ALLOW_WRITE=1) and set EZBKMCP_ALLOW_WRITE=1 for this server, then restart Claude Code.`;
  }
  return tool.description;
}

/** The plane's meta fields worth passing through, plus ours (§12.1). */
function buildMeta(planeMeta: Record<string, unknown> | undefined, config: Config, tookMs: number, extra: { truncated?: boolean | undefined; limitApplied?: number | undefined; untrusted?: string[] | undefined }): Meta {
  const pm = planeMeta ?? {};
  const meta: Meta = {
    app: 'ezbookkeeping',
    target: config.target,
    asOf: typeof pm.asOf === 'string' ? pm.asOf : new Date().toISOString(),
    tookMs,
    truncated: extra.truncated === true || pm.truncated === true,
  };
  if (typeof pm.user === 'string') {
    meta.user = pm.user;
  }
  if (typeof pm.defaultCurrency === 'string') {
    meta.defaultCurrency = pm.defaultCurrency;
  }
  if (typeof pm.serverVersion === 'string') {
    meta.serverVersion = pm.serverVersion;
  }
  if (typeof pm.timezone === 'string') {
    meta.timezone = pm.timezone;
  }
  const limitApplied = extra.limitApplied ?? (typeof pm.limitApplied === 'number' ? pm.limitApplied : undefined);
  if (limitApplied !== undefined) {
    meta.limitApplied = limitApplied;
  }
  const untrusted = Array.isArray(pm.untrusted) ? (pm.untrusted as string[]) : extra.untrusted;
  if (untrusted !== undefined && untrusted.length > 0) {
    meta.untrusted = untrusted;
  }
  for (const key of ['partial', 'composed', 'dryRun', 'replayed', 'tier', 'commit'] as const) {
    if (pm[key] !== undefined) {
      meta[key] = pm[key];
    }
  }
  return meta;
}

export class McpServerHost {
  readonly #opts: HostOptions;
  readonly server: Server;
  /** Concurrency 1 for writes (§15): a second write waits for the first. */
  #writeChain: Promise<unknown> = Promise.resolve();

  constructor(opts: HostOptions) {
    this.#opts = opts;

    this.server = new Server(
      { name: SERVER_NAME, version: SERVER_VERSION },
      {
        capabilities: { tools: {} },
        instructions: opts.instructions,
      },
    );

    this.server.setRequestHandler(ListToolsRequestSchema, async () => this.handleListTools());
    this.server.setRequestHandler(CallToolRequestSchema, async request => this.handleCallTool(request.params.name, request.params.arguments ?? {}));
  }

  handleListTools(): { tools: Array<{ name: string; description: string; inputSchema: Record<string, unknown> }> } {
    return {
      tools: TOOLS.map(tool => ({
        name: tool.name,
        description: describeForListing(tool, this.#opts.config),
        inputSchema: tool.inputSchema,
      })),
    };
  }

  async handleCallTool(name: string, args: unknown): Promise<ToolContent> {
    const { config, client, logger, keyFingerprint } = this.#opts;
    const startedAt = Date.now();
    const tool = findTool(name);

    if (tool === undefined) {
      // A tool-level failure, never a JSON-RPC error: losing one call must not kill a loop (§12.2).
      logger.audit(auditLine({ tool: name, tier: 'read', target: config.target, args, ok: false, tookMs: 0, keyFingerprint, gate: 'dispatch', errorCode: 'not_found' }), true);
      return respond({ ok: false, tool: name, error: { code: 'not_found', message: `No tool named "${name}" on this server.`, hint: 'every tool here begins with `ezb_`; list the tools to see them' } }, true);
    }

    let gate: string | undefined;
    try {
      gate = 'routing';
      checkNotForeign(args);

      gate = 'mode';
      checkMode(tool, config);

      gate = 'input';
      const parsed = checkInput<unknown>(tool, args);

      gate = 'confirm';
      checkConfirm(tool, parsed);

      gate = undefined;
      const result = tool.tier === 'write' ? await this.#serialised(() => tool.run(parsed, { client, config })) : await tool.run(parsed, { client, config });

      const meta = buildMeta(result.meta, config, Date.now() - startedAt, { truncated: result.truncated, limitApplied: result.limitApplied, untrusted: result.untrusted });
      const rows = countRows(result.data);
      logger.audit(
        auditLine({ tool: name, tier: tool.tier, target: config.target, user: meta.user, args, ok: true, tookMs: meta.tookMs, keyFingerprint, rows }),
        false,
      );

      const envelope: Envelope = { ok: true, tool: name, data: result.data, meta };
      return respond(envelope, false);
    } catch (err) {
      // pm/error_err.mdx §7 T7: the one catch around tool dispatch.
      const failure = toToolFailure(err);
      if (failure.code === 'internal') {
        errors.caught(`running ${tool.name}`, err, { tool: tool.name });
      } else {
        errors.expected(`running ${tool.name}`, err); // a gate refusal or a plane answer (R7)
      }

      logger.audit(
        auditLine({ tool: name, tier: tool.tier, target: config.target, args, ok: false, tookMs: Date.now() - startedAt, keyFingerprint, gate, errorCode: failure.code }),
        true,
      );

      return respond(
        {
          ok: false,
          tool: name,
          error: {
            code: failure.code,
            message: failure.message,
            ...(failure.hint === undefined ? {} : { hint: failure.hint }),
            ...(failure.details === undefined ? {} : { details: failure.details }),
          },
          meta: { app: 'ezbookkeeping', target: config.target, asOf: new Date().toISOString(), tookMs: Date.now() - startedAt },
        },
        true,
      );
    }
  }

  /** Run a write after every earlier write has settled, whatever the earlier one's outcome. */
  #serialised<T>(fn: () => Promise<T>): Promise<T> {
    const next = this.#writeChain.then(fn, fn);
    this.#writeChain = next.then(
      () => undefined,
      () => undefined,
    );
    return next;
  }
}

/** Anything that is not a ToolError is an internal fault: the model gets a code and a hint, the detail goes to error.err (§7.4). */
export function toToolFailure(err: unknown): ToolError {
  if (err instanceof ToolError) {
    return err;
  }
  return new ToolError('internal', 'The tool failed unexpectedly.', ERROR_FILE_HINT);
}

/** One text content block holding pretty-printed JSON (§12.1). */
function respond(envelope: Envelope, isError: boolean): ToolContent {
  return {
    content: [{ type: 'text', text: JSON.stringify(envelope, null, 2) }],
    ...(isError ? { isError: true } : {}),
  };
}

/** A row count for the audit line, when the data has an obvious list. A count is safe; the rows are not. */
function countRows(data: unknown): number | undefined {
  if (Array.isArray(data)) {
    return data.length;
  }
  if (data !== null && typeof data === 'object') {
    for (const value of Object.values(data as Record<string, unknown>)) {
      if (Array.isArray(value)) {
        return value.length;
      }
    }
  }
  return undefined;
}
