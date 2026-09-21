/**
 * Entry point — pm/mcp.mdx §6.3, §8, §14, §15.
 *
 * Key, config, target and instructions are resolved BEFORE the transport is attached, so a
 * misconfiguration is a clean refusal on stderr rather than a server that connects and then fails
 * every call. Nothing here writes to stdout: stdout is the JSON-RPC wire (§8.1).
 */
import { StdioServerTransport } from '@modelcontextprotocol/sdk/server/stdio.js';

import { MachinePlaneClient } from './client.js';
import { ConfigError, loadConfig } from './config.js';
import type { Config } from './config.js';
import { CredentialsError, checkCredentialsFile, fingerprint, readProduct, resolveKey } from './credentials.js';
import { describeError, errorFileFor } from './errfile/index.js';
import { installNodeErrorFile } from './errfile/node.js';
import { resolveInstructions } from './instructions.js';
import { Logger } from './logger.js';
import { McpServerHost, SERVER_NAME, SERVER_VERSION } from './server.js';

// pm/error_err.mdx §8 N14: the process-level net, first thing. The library echoes to stderr only.
installNodeErrorFile({ app: 'mcp' });
const errors = errorFileFor('mcp/src/index.ts');

function refuse(message: string, fix: string): never {
  // stderr, always: a refusal printed to stdout would be the first thing to corrupt the wire.
  process.stderr.write(`${SERVER_NAME}: ${message}\n  fix: ${fix}\n`);
  process.exit(2);
}

/** The timezone sent as X-Timezone-Name: EZBKMCP_TIMEZONE → the credentials file → the system (§14). */
function resolveTimezone(config: Config): string | undefined {
  if (config.timezone !== undefined) {
    return config.timezone;
  }
  try {
    const fromFile = readProduct(config.credentialsFile).timezone;
    if (fromFile !== undefined && fromFile !== '') {
      return fromFile;
    }
  } catch (err) {
    errors.expected('reading the timezone from the credentials file', err);
  }
  try {
    return Intl.DateTimeFormat().resolvedOptions().timeZone;
  } catch (err) {
    errors.expected('resolving the system timezone', err);
    return undefined;
  }
}

export class Main {
  static async run(argv: string[]): Promise<void> {
    if (argv[0] !== 'serve') {
      process.stderr.write(
        `usage: ${SERVER_NAME} serve\n\n` +
          '  Registered with:\n' +
          '    claude mcp add --scope user ezbookkeeping -- "$HOME/BGit/Bryan_git/ezbookkeeping/mcp/dist/index.js" serve\n' +
          '  The bare -- is load-bearing: everything after it is the command and its argv.\n',
      );
      process.exit(2);
    }

    let config: Config;
    try {
      config = loadConfig();
    } catch (err) {
      if (err instanceof ConfigError) {
        errors.expected('loading the MCP config', err); // a misconfiguration is an answer with a fix (R7)
        refuse(err.message, err.fix);
      }
      throw err;
    }

    const logger = new Logger({ dir: config.logDir, level: config.logLevel });

    // Gate 2, and fail closed harder than the CLI (§6.3): a credentials file looser than 0600 or a
    // key that cannot be resolved or minted refuses the start with one stderr line.
    let key: string;
    try {
      checkCredentialsFile(config.credentialsFile);
      const resolved = resolveKey({ file: config.credentialsFile, mint: true, mintedBy: 'mcp' });
      if (resolved === null) {
        refuse('no API secret key could be resolved or minted.', 'start the app once (`ezbk up`) so it mints one, or run `ezbk key init`');
      }
      key = resolved.key;
      logger.info(`key source=${resolved.source} file=${resolved.file} fingerprint=${fingerprint(key)}`);
    } catch (err) {
      if (err instanceof CredentialsError) {
        errors.expected('resolving the API secret key', err);
        refuse(err.message, err.fix);
      }
      throw err;
    }

    const keyFingerprint = fingerprint(key);
    const timezone = resolveTimezone(config);
    const client = new MachinePlaneClient({ apiUrl: config.apiUrl, key, timeoutMs: config.timeoutMs, maxBytes: config.maxBytes, timezone });

    const instructions = resolveInstructions({ apiUrl: config.apiUrl, credentialsFile: config.credentialsFile }, config.promptFile, line => {
      logger.warn(line);
    });

    const host = new McpServerHost({ config, client, logger, keyFingerprint, instructions });

    // One line, stderr, naming the target — so a transcript shows which install answered (§7.3).
    logger.banner(
      `${SERVER_NAME} ${SERVER_VERSION} -> ${config.apiUrl} (${config.target}) key ${keyFingerprint} writes ${config.allowWrite ? 'ENABLED' : 'off'} tz ${timezone ?? 'server'} log ${config.logDir}`,
    );
    logger.info(`start target=${config.target} url=${config.apiUrl} writes=${String(config.allowWrite)} key=${keyFingerprint} maxRows=${String(config.maxRows)} maxChanges=${String(config.maxChanges)} timeoutMs=${String(config.timeoutMs)}`);

    const transport = new StdioServerTransport();
    await host.server.connect(transport);

    const shutdown = (signal: string): void => {
      logger.info(`shutdown signal=${signal}`);
      host.server.close().then(
        () => {
          process.exit(0);
        },
        err => {
          errors.warn('closing the MCP server', err);
          process.exit(0);
        },
      );
    };
    process.on('SIGINT', () => {
      shutdown('SIGINT');
    });
    process.on('SIGTERM', () => {
      shutdown('SIGTERM');
    });
  }
}

// pm/error_err.mdx §7 T8: the top-level catch.
Main.run(process.argv.slice(2)).catch((err: unknown) => {
  errors.fatal('starting the ezbookkeeping MCP server', err);
  process.stderr.write(`${SERVER_NAME}: ${describeError(err)}\n`);
  process.exit(1);
});
