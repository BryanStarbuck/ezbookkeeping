// NODE ONLY — the node sink and the process-level net (pm/error_err.mdx §3.1, §5.4, §8 N14).
//
// Used by the MCP server (`app: 'mcp'`). EVERY echo goes to stderr: stdout is the JSON-RPC
// channel, and there is no code path in this file that can write to it (R11).

import { homedir, tmpdir, userInfo } from 'node:os';

import { describeError, errorFileFor, flushErrorFile, readEnv, resolveErrorFilePath, setErrorSink } from './core.js';
import type { ErrorSink } from './core.js';
import { stackOf } from './describe.js';
import { formatRecord } from './format.js';
import { setStatementsRoot } from './redact.js';
import { RollingFileWriter } from './rolling-file-writer.js';

export { RollingFileWriter } from './rolling-file-writer.js';
export type { RollingFileWriterOptions } from './rolling-file-writer.js';

export type NodeErrorFileOptions = {
    /** The runtime tag (§3.3): 'mcp'. */
    app: string;
    /** Full path override. Default: EZBK_ERROR_FILE, else §3.1. */
    file?: string;
    /** The repo-relative path the process-level records report as. Default `mcp/src/index.ts`. */
    where?: string;
    /** Install uncaughtException / unhandledRejection / exit / signal handlers. Default true. */
    handleProcessErrors?: boolean;
    /** After an uncaught exception is written and flushed, exit(1). Default true. */
    exitOnUncaught?: boolean;
    /** Echo records to STDERR. Default on (§11.5); EZBK_ERROR_FILE_ECHO=0 turns it off. */
    echo?: boolean;
    /** The statements root to redact (§12.7). Default EZBK_STATEMENTS_DIR. */
    statementsRoot?: string;
};

export type NodeErrorFile = {
    /** Synchronous barrier: owed summaries and buffered lines are on disk when it returns. */
    flush: () => void;
    /** The file this process writes. */
    file: string;
};

function uid(): string {
    try {
        return String(userInfo().uid);
    } catch {
        return 'user';
    }
}

function safeHome(): string {
    try {
        return homedir();
    } catch {
        return '';
    }
}

function safeTmp(): string {
    try {
        return tmpdir();
    } catch {
        return '/tmp';
    }
}

/** §3.1 for this process: the shared resolver, fed node's uid, home and temp directory. */
export function defaultErrorFilePath(env: NodeJS.ProcessEnv = process.env): string {
    return resolveErrorFilePath(env, { uid: uid(), home: safeHome(), tmpdir: safeTmp() });
}

function defaultEcho(): boolean {
    return readEnv('EZBK_ERROR_FILE_ECHO') !== '0';
}

function toStderr(text: string): void {
    try {
        process.stderr.write(text.endsWith('\n') ? text : `${text}\n`);
    } catch {
        // nothing else we can safely do
    }
}

type Installed = {
    app: string;
    file: string;
    writer: RollingFileWriter;
};

let installed: Installed | null = null;
let handlersInstalled = false;

function installProcessHandlers(where: string, exitOnUncaught: boolean): void {
    if (handlersInstalled) {
        return;
    }

    handlersInstalled = true;
    const errors = errorFileFor(where);

    process.on('uncaughtException', (err: unknown) => {
        // FATAL, then a synchronous flush (fatal does both), then the process ends (R9).
        errors.fatal('an uncaught exception', err);

        if (exitOnUncaught) {
            // Our listener replaced Node's own report, so print what it would have printed.
            toStderr(`ezbookkeeping mcp: uncaught exception: ${describeError(err)}`);
            const stack = stackOf(err);

            if (stack) {
                toStderr(stack);
            }

            process.exit(1);
        }
    });

    process.on('unhandledRejection', (reason: unknown) => {
        errors.caught('an unhandled promise rejection', reason);
    });

    process.on('beforeExit', () => flushErrorFile());
    process.on('exit', () => flushErrorFile());

    for (const signal of ['SIGINT', 'SIGTERM'] as const) {
        const onSignal = (): void => {
            flushErrorFile();

            // Adding a listener removes Node's default "exit on signal". If ours is the only one,
            // re-raise so the process ends exactly as it would have without us; a host handler is
            // never replaced.
            if (process.listenerCount(signal) === 1) {
                process.removeListener(signal, onSignal);
                process.kill(process.pid, signal);
            }
        };
        process.on(signal, onSignal);
    }
}

/**
 * Install the node sink for this process. Idempotent: a second call returns the first install.
 * Every path is total — a broken environment degrades to stderr, never to a throw.
 */
export function installNodeErrorFile(opts: NodeErrorFileOptions): NodeErrorFile {
    if (installed) {
        return { flush: flushErrorFile, file: installed.file };
    }

    const file = opts.file ?? defaultErrorFilePath();
    const writer = new RollingFileWriter({ filePath: file });
    const echo = opts.echo ?? defaultEcho();

    try {
        setStatementsRoot(opts.statementsRoot ?? readEnv('EZBK_STATEMENTS_DIR') ?? '');
    } catch {
        // the root stays unset; the rest of the redaction still applies
    }

    const sink: ErrorSink = {
        app: opts.app,
        verbose: readEnv('EZBK_ERROR_FILE_VERBOSE') === '1',
        write: record => {
            const line = formatRecord(record);
            writer.write(line);

            if (echo) {
                toStderr(line);
            }
        },
        flush: () => writer.flush()
    };
    installed = { app: opts.app, file, writer };
    setErrorSink(sink);

    if (opts.handleProcessErrors ?? true) {
        try {
            installProcessHandlers(opts.where ?? 'mcp/src/index.ts', opts.exitOnUncaught ?? true);
        } catch {
            // no usable process object; the sink still works
        }
    }

    return { flush: flushErrorFile, file };
}

/** Test seam: forget the install (process listeners, once added, stay). */
export function resetNodeErrorFileForTests(): void {
    installed = null;
    setErrorSink(null);
}
