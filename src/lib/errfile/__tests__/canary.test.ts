// §15.3 — the web and sw runtime canaries: a deliberate fault in each browser runtime reaches the
// POST body with the right `app` tag. scripts/error-file-coverage.mjs --canary runs this file.
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import { BATCH_DELAY_MS, installBrowserErrorFile, resetBrowserErrorFileForTests } from '../browser.js';
import type { BrowserReportBody, BrowserScope } from '../browser.js';
import { resetErrorFileForTests } from '../core.js';

type Posted = { url: string; body: BrowserReportBody };

function stubScope(pathname: string, worker: boolean): BrowserScope & { posts: Posted[]; emit(type: string, event: unknown): void } {
    const listeners = new Map<string, ((event: unknown) => void)[]>();
    const posts: Posted[] = [];
    const scope = {
        posts,
        location: { pathname },
        addEventListener(type: string, listener: (event: unknown) => void) {
            listeners.set(type, [...(listeners.get(type) ?? []), listener]);
        },
        emit(type: string, event: unknown) {
            for (const l of listeners.get(type) ?? []) {
                l(event);
            }
        },
        fetch: (url: string, init: RequestInit) => {
            posts.push({ url, body: JSON.parse(String(init.body)) as BrowserReportBody });
            return Promise.resolve({});
        }
    };
    return worker ? scope : Object.assign(scope, { document: { visibilityState: 'visible' } });
}

beforeEach(() => {
    resetBrowserErrorFileForTests();
    resetErrorFileForTests();
    vi.useFakeTimers();
});

afterEach(() => vi.useRealTimers());

describe('runtime canaries (§15.3)', () => {
    it('canary web: a throw in a click handler reaches POST /error-report tagged [web]', () => {
        const scope = stubScope('/desktop', false);
        installBrowserErrorFile({ app: 'web', edition: 'desktop', scope });
        const onClick = (): void => {
            throw new TypeError('canary web');
        };

        try {
            onClick();
        } catch (e) {
            scope.emit('error', { error: e, message: String(e) });
        }

        vi.advanceTimersByTime(BATCH_DELAY_MS);
        expect(scope.posts).toHaveLength(1);
        expect(scope.posts[0]?.url).toBe('/error-report');
        expect(scope.posts[0]?.body.app).toBe('web');
        expect(scope.posts[0]?.body.events[0]?.error).toBe('TypeError: canary web');
        expect(scope.posts[0]?.body.events[0]?.data).toEqual({ edition: 'desktop' });
    });

    it('canary sw: an unhandled rejection in the worker reaches POST /error-report tagged [sw]', () => {
        const scope = stubScope('/sw.js', true);
        installBrowserErrorFile({ app: 'sw', scope });
        scope.emit('unhandledrejection', { reason: new Error('canary sw') });
        vi.advanceTimersByTime(BATCH_DELAY_MS);
        expect(scope.posts).toHaveLength(1);
        expect(scope.posts[0]?.body.app).toBe('sw');
        expect(scope.posts[0]?.body.events[0]?.where).toBe('src/sw.ts');
        expect(scope.posts[0]?.body.events[0]?.error).toBe('Error: canary sw');
    });
});
