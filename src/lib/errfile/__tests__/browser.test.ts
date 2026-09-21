import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import {
    BATCH_DELAY_MS,
    BATCH_SIZE,
    CLIENT_BUDGET_PER_MINUTE,
    basePathOf,
    componentNameOf,
    installBrowserErrorFile,
    isBenignBrowserError,
    resetBrowserErrorFileForTests,
    resolveEndpoint
} from '../browser.js';
import type { BrowserReportBody, BrowserScope, VueAppLike } from '../browser.js';
import { errorFileFor, flushErrorFile, resetErrorFileForTests } from '../core.js';

type FakeScope = BrowserScope & {
    listeners: Map<string, ((event: unknown) => void)[]>;
    posts: { url: string; body: BrowserReportBody }[];
    beacons: { url: string; data: string }[];
    emit(type: string, event: unknown): void;
};

function parseBody(text: string): BrowserReportBody {
    const parsed: unknown = JSON.parse(text);

    if (typeof parsed !== 'object' || parsed === null || !('events' in parsed)) {
        throw new Error('bad body');
    }

    return parsed as BrowserReportBody;
}

function fakeScope(opts: { fetchImpl?: () => Promise<unknown>; pathname?: string; worker?: boolean; beacon?: boolean } = {}): FakeScope {
    const listeners = new Map<string, ((event: unknown) => void)[]>();
    const posts: { url: string; body: BrowserReportBody }[] = [];
    const beacons: { url: string; data: string }[] = [];
    const scope: FakeScope = {
        listeners,
        posts,
        beacons,
        location: { pathname: opts.pathname ?? '/desktop' },
        addEventListener(type, listener) {
            listeners.set(type, [...(listeners.get(type) ?? []), listener]);
        },
        emit(type, event) {
            for (const l of listeners.get(type) ?? []) {
                l(event);
            }
        },
        fetch: (url, init) => {
            posts.push({ url, body: parseBody(String(init.body)) });
            return opts.fetchImpl ? opts.fetchImpl() : Promise.resolve({});
        },
        navigator: {
            sendBeacon: (url, data) => {
                beacons.push({ url, data: typeof data === 'string' ? data : 'blob' });
                return true;
            }
        }
    };

    if (!opts.worker) {
        scope.document = { visibilityState: 'visible' };
    }

    if (opts.beacon === false) {
        delete scope.navigator;
    }

    return scope;
}

const errors = errorFileFor('src/lib/errfile/__tests__/browser.test.ts');

beforeEach(() => {
    resetBrowserErrorFileForTests();
    resetErrorFileForTests();
    vi.useFakeTimers();
});

afterEach(() => vi.useRealTimers());

describe('browser sink (§5.3)', () => {
    it('batches at 20 records into one POST of { app, events }', () => {
        const scope = fakeScope();
        installBrowserErrorFile({ app: 'web', edition: 'desktop', scope });

        for (let i = 0; i < BATCH_SIZE; i++) {
            errorFileFor(`src/w${i}.ts`).caught('clicking', new Error('x'));
        }

        expect(scope.posts).toHaveLength(1);
        expect(scope.posts[0]?.url).toBe('/error-report');
        expect(scope.posts[0]?.body.app).toBe('web');
        expect(scope.posts[0]?.body.events).toHaveLength(BATCH_SIZE);
        expect(scope.posts[0]?.body.events[0]?.data).toEqual({ edition: 'desktop' });
    });

    it('batches after 2 s with one timer', () => {
        const scope = fakeScope({ worker: true, pathname: '/sw.js' });
        installBrowserErrorFile({ app: 'sw', scope });
        errors.caught('handling a fetch', new Error('x'));
        errors.caught('handling a message', new Error('y'));
        expect(scope.posts).toHaveLength(0);
        vi.advanceTimersByTime(BATCH_DELAY_MS);
        expect(scope.posts).toHaveLength(1);
        expect(scope.posts[0]?.body.events).toHaveLength(2);
        expect(scope.posts[0]?.body.app).toBe('sw');
        expect(scope.listeners.has('pagehide')).toBe(false);
    });

    it('resolves the endpoint against the base path so /desktop and /sub/mobile both reach /error-report', () => {
        expect(basePathOf('/desktop')).toBe('');
        expect(basePathOf('/desktop.html')).toBe('');
        expect(basePathOf('/sub/mobile')).toBe('/sub');
        expect(basePathOf('/sw.js')).toBe('');
        expect(basePathOf(undefined)).toBe('');
        expect(resolveEndpoint('/error-report', fakeScope({ pathname: '/ezbk/desktop' }))).toBe('/ezbk/error-report');
        expect(resolveEndpoint('http://127.0.0.1:8080/error-report', fakeScope())).toBe('http://127.0.0.1:8080/error-report');
        const scope = fakeScope({ pathname: '/ezbk/mobile' });
        installBrowserErrorFile({ app: 'web', edition: 'mobile', scope });
        errors.caught('saving', new Error('x'));
        vi.advanceTimersByTime(BATCH_DELAY_MS);
        expect(scope.posts[0]?.url).toBe('/ezbk/error-report');
    });

    it('uses sendBeacon on pagehide and when the page becomes hidden', () => {
        const scope = fakeScope();
        installBrowserErrorFile({ app: 'web', scope });
        errors.caught('closing the tab', new Error('x'));
        scope.emit('pagehide', {});
        expect(scope.beacons).toHaveLength(1);
        expect(scope.beacons[0]?.url).toBe('/error-report');
        expect(scope.posts).toHaveLength(0);
        errors.caught('hiding the tab', new Error('y'));
        scope.document = { visibilityState: 'hidden' };
        scope.emit('visibilitychange', {});
        expect(scope.beacons).toHaveLength(2);
    });

    it('enforces the client budget and sends one summary', () => {
        const scope = fakeScope({ beacon: false });
        installBrowserErrorFile({ app: 'web', scope });

        for (let i = 0; i < CLIENT_BUDGET_PER_MINUTE + 10; i++) {
            errorFileFor(`src/w${i}.ts`).caught('looping', new Error(`distinct ${i}`));
        }

        flushErrorFile();
        const events = scope.posts.flatMap(p => p.body.events);
        expect(events).toHaveLength(CLIENT_BUDGET_PER_MINUTE + 1);
        expect(events.at(-1)?.error).toBe(`10 records dropped over the client budget of ${CLIENT_BUDGET_PER_MINUTE}/min`);
        expect(events.at(-1)?.level).toBe('WARN');
    });

    it('never re-queues a delivery failure and warns once per session', async () => {
        const warn = vi.spyOn(console, 'warn').mockImplementation(() => undefined);
        const scope = fakeScope({ fetchImpl: () => Promise.reject(new Error('offline')) });
        installBrowserErrorFile({ app: 'web', scope });
        errors.caught('saving', new Error('x'));
        vi.advanceTimersByTime(BATCH_DELAY_MS);
        await vi.runAllTimersAsync();
        errors.caught('saving again', new Error('y'));
        vi.advanceTimersByTime(BATCH_DELAY_MS);
        await vi.runAllTimersAsync();
        expect(scope.posts).toHaveLength(2);
        expect(scope.posts[1]?.body.events).toHaveLength(1);
        expect(warn).toHaveBeenCalledTimes(1);
        warn.mockRestore();
    });

    it('reports window errors and rejections, ignoring ResizeObserver and HMR noise', () => {
        const scope = fakeScope();
        installBrowserErrorFile({ app: 'web', edition: 'desktop', scope });
        scope.emit('error', { error: null, message: 'ResizeObserver loop completed with undelivered notifications.' });
        scope.emit('error', { error: null, message: 'ResizeObserver loop limit exceeded' });
        scope.emit('error', { filename: 'http://localhost:8081/@vite/client', message: 'hmr' });
        scope.emit('error', { error: new TypeError('real'), message: 'real' });
        scope.emit('error', { error: null, message: 'Script error.' });
        scope.emit('unhandledrejection', { reason: new Error('rejected') });
        scope.emit('unhandledrejection', { reason: { canceled: true } });
        vi.advanceTimersByTime(BATCH_DELAY_MS);
        const events = scope.posts.flatMap(p => p.body.events);
        expect(events.map(e => e.doing)).toEqual(['an uncaught error', 'an uncaught error', 'an unhandled promise rejection']);
        expect(events.map(e => e.error)).toEqual(['TypeError: real', 'Thrown string: Script error.', 'Error: rejected']);
        expect(events[0]?.where).toBe('src/desktop-main.ts');
        expect(isBenignBrowserError({ error: new Error('ResizeObserver loop limit exceeded') })).toBe(false);
    });

    it('a chunk-load failure from the window net is a WARN', () => {
        const scope = fakeScope();
        installBrowserErrorFile({ app: 'web', scope });
        scope.emit('unhandledrejection', { reason: new TypeError('Failed to fetch dynamically imported module: http://localhost/js/x.js') });
        vi.advanceTimersByTime(BATCH_DELAY_MS);
        expect(scope.posts[0]?.body.events[0]?.level).toBe('WARN');
    });

    it('the Vue errorHandler reports with the component name and the hook', () => {
        const scope = fakeScope();
        const vueApp: VueAppLike = { config: {} };
        installBrowserErrorFile({ app: 'web', edition: 'mobile', vueApp, scope });
        expect(typeof vueApp.config.errorHandler).toBe('function');
        vueApp.config.errorHandler?.(new Error('render'), { $: { type: { __name: 'ListPage' } }, $options: {} }, 'mounted hook');
        vueApp.config.errorHandler?.(new Error('render2'), null, 'render function');
        vi.advanceTimersByTime(BATCH_DELAY_MS);
        const events = scope.posts.flatMap(p => p.body.events);
        expect(events[0]?.doing).toBe('rendering ListPage');
        expect(events[0]?.data).toEqual({ edition: 'mobile', hook: 'mounted hook' });
        expect(events[0]?.where).toBe('src/mobile-main.ts');
        expect(events[1]?.doing).toBe('rendering an anonymous component');
        expect(componentNameOf({ $options: { name: 'Named' } })).toBe('Named');
    });

    it('is idempotent and never echoes to the console', () => {
        const error = vi.spyOn(console, 'error').mockImplementation(() => undefined);
        const scope = fakeScope();
        installBrowserErrorFile({ app: 'web', scope });
        installBrowserErrorFile({ app: 'sw', scope });
        errors.caught('saving', new Error('x'));
        vi.advanceTimersByTime(BATCH_DELAY_MS);
        expect(scope.posts[0]?.body.app).toBe('web');
        expect(error).not.toHaveBeenCalled();
        error.mockRestore();
    });
});
