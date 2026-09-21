// BROWSER ONLY — the browser/worker sink (pm/error_err.mdx §5.3, §9, R8).
//
// A browser tab and a service worker have no filesystem. Records are batched and POSTed to the
// loopback-only ingest route (`/error-report`) that the Go server mounts and the Vite dev server
// proxies (§8 N15). This file is imported once per entry point (§8 N1–N3) and never by an ordinary
// source file.

import { errorFileFor, flushErrorFile, setErrorSink } from './core.js';
import type { ErrorData, ErrorRecord, ErrorSink } from './core.js';
import { prop } from './describe.js';
import { unrefTimer } from './fold.js';

/** The slice of a Vue `App` this file uses; structural, so Vue is never imported here. */
export type VueAppLike = {
    config: {
        errorHandler?(err: unknown, instance: unknown, info: string): void;
    };
};

export type BrowserErrorFileOptions = {
    /** 'web' or 'sw' (§3.3). */
    app: string;
    /** 'desktop' or 'mobile' — stamped on every record as `data.edition` (§3.3). */
    edition?: string;
    /** The Vue app: its `config.errorHandler` becomes a report (§5.3). */
    vueApp?: VueAppLike;
    /** Default '/error-report', resolved against the page's base path. */
    endpoint?: string;
    /** Install `error` / `unhandledrejection` listeners on window / self. Default true. */
    handleGlobalErrors?: boolean;
    /** The repo-relative path global-handler records report as. Default: the entry point (§8). */
    where?: string;
    /** Test seam: the global scope (window or self). */
    scope?: BrowserScope;
};

export type BrowserReportBody = { app: string; events: ErrorRecord[] };

/** The slice of window / WorkerGlobalScope this file uses. */
export type BrowserScope = {
    addEventListener(type: string, listener: (event: unknown) => void): void;
    fetch?: (url: string, init: RequestInit) => Promise<unknown>;
    navigator?: { sendBeacon?: (url: string, data: Blob | string) => boolean };
    document?: { visibilityState?: string };
    location?: { pathname?: string };
};

/** Flush when this many records are queued … */
export const BATCH_SIZE = 20;
/** … or this long after the first one. */
export const BATCH_DELAY_MS = 2000;
/** Client budget: records per minute. The rest are counted and sent as one summary. */
export const CLIENT_BUDGET_PER_MINUTE = 30;

/** §11.3: never written, when no Error object was thrown. */
export const BENIGN_BROWSER_MESSAGES: readonly string[] = [
    'ResizeObserver loop completed with undelivered notifications',
    'ResizeObserver loop limit exceeded'
];

/** §11.3: benign browser conditions that are never written. */
export function isBenignBrowserError(event: unknown): boolean {
    const error = prop(event, 'error');
    const message = prop(event, 'message');

    if ((error === null || error === undefined) && typeof message === 'string') {
        for (const benign of BENIGN_BROWSER_MESSAGES) {
            if (message.includes(benign)) {
                return true;
            }
        }
    }

    const filename = prop(event, 'filename');

    if (typeof filename === 'string' && filename.includes('/@vite/client')) {
        return true;
    }

    const stack = prop(error, 'stack');

    if (typeof stack === 'string' && stack.includes('/@vite/client')) {
        return true;
    }

    return false;
}

/**
 * `getBasePath()` semantics from src/lib/web.ts, re-implemented here because this file may import
 * nothing from src/ (§5.1): the pathname up to its last slash, so `/desktop`, `/desktop.html`,
 * `/sw.js` all give '' and `/sub/mobile` gives `/sub`.
 */
export function basePathOf(pathname: string | undefined): string {
    if (!pathname) {
        return '';
    }

    const lastSlashIndex = pathname.lastIndexOf('/');

    if (lastSlashIndex < 0) {
        return pathname;
    }

    return pathname.substring(0, lastSlashIndex);
}

/** An absolute endpoint stays; a root-relative one is resolved against the page's base path. */
export function resolveEndpoint(endpoint: string, scope: BrowserScope | null): string {
    if (/^[a-z][a-z0-9+.-]*:\/\//i.test(endpoint) || !endpoint.startsWith('/')) {
        return endpoint;
    }

    return basePathOf(scope?.location?.pathname) + endpoint;
}

function isScope(value: unknown): value is BrowserScope {
    return typeof prop(value, 'addEventListener') === 'function';
}

function defaultScope(): BrowserScope | null {
    const scope: unknown = globalThis;
    return isScope(scope) ? scope : null;
}

/** The name of the Vue component an error was raised in, for `rendering <component name>`. */
export function componentNameOf(instance: unknown): string {
    const options = prop(instance, '$options');
    const fromOptions = prop(options, 'name');

    if (typeof fromOptions === 'string' && fromOptions) {
        return fromOptions;
    }

    const type = prop(prop(instance, '$'), 'type');

    for (const key of ['__name', 'name']) {
        const value = prop(type, key);

        if (typeof value === 'string' && value) {
            return value;
        }
    }

    return 'an anonymous component';
}

let installedApp: string | null = null;

/** Install the browser sink. Idempotent: a second call is ignored. Never throws. */
export function installBrowserErrorFile(opts: BrowserErrorFileOptions): void {
    if (installedApp !== null) {
        return;
    }

    const scope = opts.scope ?? defaultScope();
    const endpoint = resolveEndpoint(opts.endpoint ?? '/error-report', scope);
    const app = opts.app;
    const edition = opts.edition;
    installedApp = app;

    let queue: ErrorRecord[] = [];
    let timer: ReturnType<typeof setTimeout> | null = null;
    let budgetWindowStart = 0;
    let budgetUsed = 0;
    let overBudget = 0;
    let deliveryWarned = false;

    function warnOnce(err: unknown): void {
        if (deliveryWarned) {
            return;
        }

        deliveryWarned = true;

        try {
            console.warn('[errfile] could not deliver error reports', err);
        } catch {
            // silence
        }
    }

    function send(useBeacon: boolean): void {
        if (timer !== null) {
            clearTimeout(timer);
            timer = null;
        }

        if (overBudget > 0) {
            queue.push({
                ts: new Date().toISOString(),
                level: 'WARN',
                app,
                where: 'src/lib/errfile/browser.ts',
                doing: 'reporting browser errors',
                error: `${overBudget} records dropped over the client budget of ${CLIENT_BUDGET_PER_MINUTE}/min`,
                cause: '',
                stack: '',
                data: edition ? { edition } : null
            });
            overBudget = 0;
        }

        if (!queue.length) {
            return;
        }

        const body: BrowserReportBody = { app, events: queue };
        queue = [];

        // R11: a delivery failure is silent (one console.warn per session) and never re-queued.
        try {
            const json = JSON.stringify(body);
            const beacon = scope?.navigator?.sendBeacon;

            if (useBeacon && typeof beacon === 'function') {
                const payload = typeof Blob === 'function' ? new Blob([json], { type: 'text/plain' }) : json;

                if (beacon.call(scope?.navigator, endpoint, payload)) {
                    return;
                }
            }

            const doFetch = scope?.fetch;

            if (typeof doFetch !== 'function') {
                return;
            }

            doFetch.call(scope, endpoint, {
                method: 'POST',
                keepalive: true,
                headers: { 'content-type': 'application/json' },
                body: json
            }).then(undefined, warnOnce);
        } catch (e) {
            warnOnce(e);
        }
    }

    function withinBudget(): boolean {
        const now = Date.now();

        if (now - budgetWindowStart >= 60_000) {
            budgetWindowStart = now;
            budgetUsed = 0;
        }

        if (budgetUsed >= CLIENT_BUDGET_PER_MINUTE) {
            overBudget += 1;
            return false;
        }

        budgetUsed += 1;
        return true;
    }

    const sink: ErrorSink = {
        app,
        echo: false, // §5.3: upstream's logger.ts already prints to the console
        verbose: false,
        write(record) {
            if (!withinBudget()) {
                return;
            }

            if (edition && (!record.data || record.data['edition'] === undefined)) {
                record.data = { edition, ...(record.data ?? {}) };
            }

            queue.push(record);

            if (queue.length >= BATCH_SIZE) {
                send(false);
                return;
            }

            if (timer === null) {
                timer = setTimeout(() => send(false), BATCH_DELAY_MS);
                unrefTimer(timer);
            }
        },
        flush() {
            send(true);
        }
    };
    setErrorSink(sink);

    const where = opts.where ?? (app === 'sw' ? 'src/sw.ts' : edition ? `src/${edition}-main.ts` : `${app} (global)`);
    const errors = errorFileFor(where);

    if (opts.vueApp) {
        try {
            const previous = opts.vueApp.config.errorHandler;
            opts.vueApp.config.errorHandler = (err: unknown, instance: unknown, info: string): void => {
                const data: ErrorData = { hook: info };
                errors.caught(`rendering ${componentNameOf(instance)}`, err, data);

                if (typeof previous === 'function') {
                    previous.call(opts.vueApp?.config, err, instance, info);
                }
            };
        } catch {
            // no usable Vue app; the sink still works
        }
    }

    if (!scope) {
        return;
    }

    try {
        const onHide = (): void => flushErrorFile();

        if (scope.document) {
            scope.addEventListener('pagehide', onHide);
            scope.addEventListener('visibilitychange', () => {
                if (scope.document?.visibilityState === 'hidden') {
                    onHide();
                }
            });
        }

        if (opts.handleGlobalErrors ?? true) {
            scope.addEventListener('error', event => {
                if (isBenignBrowserError(event)) {
                    return;
                }

                const error = prop(event, 'error');
                errors.caught('an uncaught error', error === null || error === undefined ? prop(event, 'message') : error);
            });
            scope.addEventListener('unhandledrejection', event => {
                errors.caught('an unhandled promise rejection', prop(event, 'reason'));
            });
        }
    } catch {
        // no listeners — the sink still works
    }
}

/** Test seam: allow a second install. */
export function resetBrowserErrorFileForTests(): void {
    installedApp = null;
    setErrorSink(null);
}
