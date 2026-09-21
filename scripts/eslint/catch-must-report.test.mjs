// §13.2 — errfile/catch-must-report, tested with ESLint's RuleTester under vitest
// (`npx vitest run scripts/eslint`). At least 10 valid and 10 invalid cases, one per pattern of §7,
// including one .vue fixture parsed by vue-eslint-parser.
import tsParser from '@typescript-eslint/parser';
import { RuleTester } from 'eslint';
import { describe, it } from 'vitest';
import vueParser from 'vue-eslint-parser';

import rule, { expectedWhere } from './catch-must-report.mjs';

RuleTester.describe = describe;
RuleTester.it = it;
RuleTester.itOnly = it.only;

const tester = new RuleTester({
    languageOptions: {
        parser: tsParser,
        ecmaVersion: 2022,
        sourceType: 'module'
    }
});

const vueLanguageOptions = {
    parser: vueParser,
    ecmaVersion: 2022,
    sourceType: 'module',
    parserOptions: { parser: tsParser }
};

const filename = 'src/stores/account.ts';
const viewFile = 'src/views/desktop/accounts/ListPage.vue';

tester.run('catch-must-report', rule, {
    valid: [
        // T1 — a plain catch that reports and continues
        {
            code: `async function f() { try { await load(); } catch (e) { errors.caught('loading the account list', e); return null; } }`,
            filename
        },
        // T1 — the store idiom: logger.error is the N4 net, reject beside it is fine
        {
            code: `return new Promise((resolve, reject) => { services.getAllAccounts().then(r => resolve(r)).catch(error => { logger.error('failed to load account list', error); reject(error); }); });`,
            filename
        },
        // T2 — cleanup then rethrow through the library
        {
            code: `try { reader.read(); } catch (e) { reader.abort(); errors.rethrow('reading the chosen file', e); }`,
            filename: 'src/lib/file.ts'
        },
        // T2 — a bare rethrow is a hand-off to the frame that reports
        {
            code: `try { run(); } catch (e) { cleanup(); throw e; }`,
            filename
        },
        // T3 — the expected failure
        {
            code: `let supported; try { supported = !!window.PublicKeyCredential; } catch (e) { errors.expected('probing for WebAuthn support', e); supported = false; }`,
            filename: 'src/lib/webauthn.ts'
        },
        // T3 — split expected / caught
        {
            code: `try { x(); } catch (e) { if (isNotFound(e)) errors.expected('reading it', e); else errors.caught('reading it', e); }`,
            filename
        },
        // T4 — tryOr / tryOrAsync are not catch sites at all
        {
            code: `const parsed = tryOr(errors, 'parsing the saved filter', () => JSON.parse(raw), null);
const rates = await tryOrAsync(errors, 'loading exchange rates', () => fetchRates(), []);`,
            filename
        },
        // T5 — a .catch() that recovers and reports inside
        {
            code: `const rates = await exchangeRatesStore.load().catch(e => { errors.caught('loading exchange rates', e); return null; });`,
            filename
        },
        // T5 — .catch() that hands the rejection on
        {
            code: `p.catch(e => Promise.reject(e)); q.catch(e => { return Promise.reject(e); });`,
            filename
        },
        // T5 — reportRejection is a reporting helper
        {
            code: `reportRejection(errors, 'refreshing the user profile', userStore.refreshUserInfo());`,
            filename
        },
        // T6 — guard wraps a listener; the error listener itself reports
        {
            code: `reader.onload = guard(errors, 'importing the chosen file', async e => { await importFile(e); });
window.addEventListener('error', ev => { errors.caught('an uncaught error', ev.error); });
self.addEventListener('unhandledrejection', ev => { errors.caught('an unhandled promise rejection', ev.reason); });`,
            filename
        },
        // T6 — onerror that reports
        {
            code: `reader.onerror = e => { errors.caught('reading the chosen file', e); };`,
            filename
        },
        // T7 — the MCP dispatch boundary with a template-literal doing built only from allowed expressions
        {
            code: `try { await handler(req); } catch (err) { if (failure.code === 'internal') errors.caught(\`running \${def.name}\`, err, { tool: def.name }); else errors.expected(\`running \${def.name}\`, err); }
try { go(); } catch (err) { errors.caught(\`handling \${r.method} \${r.path}\`, err); }
try { go(); } catch (err) { errors.caught(\`running the \${name} verb\`, err); }
try { go(); } catch (err) { errors.caught(\`running \${tool}\`, err); }`,
            filename: 'mcp/src/server.ts'
        },
        // T7 — .then(_, fn) that rethrows through the library
        {
            code: `handler(args).then(ok, err => { if (!isAnswer(err)) errors.rethrow(\`running \${verb}\`, err); throw err; });`,
            filename
        },
        // T8 — the top-level main().catch ends in fatal
        {
            code: `Main.run().catch(err => { errors.fatal('starting the ezbookkeeping MCP server', err); process.stderr.write(describeError(err)); process.exit(1); });`,
            filename: 'mcp/src/index.ts'
        },
        // A named handler is opaque to the rule and passes
        {
            code: `p.catch(handleFailure); q.then(ok, onFailure);`,
            filename
        },
        // An ErrorFile under another name still counts
        {
            code: `try { go(); } catch (e) { workerErrors.warn('handling a worker message', e); }`,
            filename
        },
        // logger.warn is a net too
        {
            code: `try { go(); } catch (e) { logger.warn('cannot parse the cached value', e); }`,
            filename
        },
        // The where literal matches the repo-relative path
        {
            code: `const errors = errorFileFor('src/stores/account.ts');`,
            filename
        },
        {
            code: `const errors = errorFileFor('mcp/src/server.ts');`,
            filename: 'mcp/src/server.ts'
        },
        // try/finally has no catch and is not a site (§7.17)
        {
            code: `try { go(); } finally { done(); }`,
            filename
        },
        // A .vue view: <script setup> with the constant after the imports and a reporting .catch
        {
            code: `<template><div>{{ items.length }}</div></template>
<script setup lang="ts">
import { ref } from 'vue';
import { errorFileFor } from '@/lib/errfile';
const errors = errorFileFor('src/views/desktop/accounts/ListPage.vue');
const items = ref([]);
function reload() {
    store.loadAllAccounts().then(r => { items.value = r; }).catch(error => {
        errors.caught('loading the account list', error);
        if (!error.processed) { snackbar.value?.showError(error); }
    });
}
</script>`,
            filename: viewFile,
            languageOptions: vueLanguageOptions
        }
    ],
    invalid: [
        // T1 — console-only catch
        {
            code: `try { go(); } catch (e) { console.error(e); }`,
            filename,
            errors: [{ messageId: 'unreported' }]
        },
        // T1 — the view idiom that only shows a snackbar
        {
            code: `store.load().catch(error => { loading.value = false; if (!error.processed) { snackbar.value?.showError(error); } });`,
            filename,
            errors: [{ messageId: 'unreported' }]
        },
        // T1 — a bare reject(error) moves the fault without recording it
        {
            code: `services.getAll().catch(error => { reject(error); });`,
            filename,
            errors: [{ messageId: 'unreported' }]
        },
        // T2 — cleanup that swallows instead of rethrowing
        {
            code: `try { reader.read(); } catch (e) { reader.abort(); }`,
            filename,
            errors: [{ messageId: 'unreported' }]
        },
        // T3 — the empty catch (optional catch binding, and the braces-only form)
        {
            code: `let supported; try { supported = !!window.PublicKeyCredential; } catch { supported = false; }`,
            filename,
            errors: [{ messageId: 'unreported' }]
        },
        {
            code: `try { go(); } catch (e) {}`,
            filename,
            errors: [{ messageId: 'emptyCatch' }]
        },
        // T4 — the five-line fallback with a console report
        {
            code: `let parsed; try { parsed = JSON.parse(raw); } catch (e) { console.warn(e); parsed = null; }`,
            filename,
            errors: [{ messageId: 'unreported' }]
        },
        // T5 — .catch() that only logs, inline or by reference, or does nothing
        {
            code: `startWatcher().catch(err => console.log(err));`,
            filename,
            errors: [{ messageId: 'unreported' }]
        },
        {
            code: `startWatcher().catch(console.error);`,
            filename,
            errors: [{ messageId: 'unreported' }]
        },
        {
            code: `p.catch(() => {});`,
            filename,
            errors: [{ messageId: 'emptyCatch' }]
        },
        // T6 — an error listener or onerror that only logs
        {
            code: `window.addEventListener('error', ev => { console.log(ev); });`,
            filename,
            errors: [{ messageId: 'unreported' }]
        },
        {
            code: `self.addEventListener('unhandledrejection', ev => { console.log(ev.reason); });`,
            filename: 'src/sw.ts',
            errors: [{ messageId: 'unreported' }]
        },
        {
            code: `reader.onerror = () => { loading.value = false; };`,
            filename,
            errors: [{ messageId: 'unreported' }]
        },
        // T7 — .then(_, fn) that swallows
        {
            code: `handler(args).then(ok, err => { state.err = err; });`,
            filename,
            errors: [{ messageId: 'unreported' }]
        },
        // T7 — an unstable doing (a URL would fragment the fold key)
        {
            code: `try { go(); } catch (err) { errors.caught(\`handling \${req.url}\`, err); }`,
            filename: 'mcp/src/server.ts',
            errors: [{ messageId: 'dynamicDoing' }]
        },
        {
            code: `try { go(); } catch (err) { errors.caught(message, err); }`,
            filename,
            errors: [{ messageId: 'dynamicDoing' }]
        },
        {
            code: `const v = tryOr(errors, 'reading ' + key, () => read(key), null);`,
            filename,
            errors: [{ messageId: 'dynamicDoing' }]
        },
        // T8 — the process-level last net must not swallow
        {
            code: `process.on('unhandledRejection', reason => { try { report(reason); } catch (e) { /* ignore */ } });`,
            filename: 'mcp/src/index.ts',
            errors: [{ messageId: 'emptyCatch' }]
        },
        {
            code: `Main.run().catch(err => { process.stderr.write(String(err)); process.exit(1); });`,
            filename: 'mcp/src/index.ts',
            errors: [{ messageId: 'unreported' }]
        },
        // Companion check on where — fixable
        {
            code: `const errors = errorFileFor('src/stores/transaction.ts');`,
            filename,
            output: `const errors = errorFileFor('src/stores/account.ts');`,
            errors: [{ messageId: 'wrongWhere' }]
        },
        {
            code: `const errors = errorFileFor(__filename);`,
            filename,
            output: `const errors = errorFileFor('src/stores/account.ts');`,
            errors: [{ messageId: 'wrongWhere' }]
        },
        {
            code: `const errors = errorFileFor('mcp/src/index.ts');`,
            filename: 'mcp/src/server.ts',
            output: `const errors = errorFileFor('mcp/src/server.ts');`,
            errors: [{ messageId: 'wrongWhere' }]
        },
        // A .vue view whose .catch only shows the snackbar, and whose where names another file
        {
            code: `<template><div /></template>
<script setup lang="ts">
import { errorFileFor } from '@/lib/errfile';
const errors = errorFileFor('src/views/desktop/accounts/EditPage.vue');
function reload() {
    store.loadAllAccounts().catch(error => {
        if (!error.processed) { snackbar.value?.showError(error); }
    });
}
</script>`,
            output: `<template><div /></template>
<script setup lang="ts">
import { errorFileFor } from '@/lib/errfile';
const errors = errorFileFor('src/views/desktop/accounts/ListPage.vue');
function reload() {
    store.loadAllAccounts().catch(error => {
        if (!error.processed) { snackbar.value?.showError(error); }
    });
}
</script>`,
            filename: viewFile,
            languageOptions: vueLanguageOptions,
            errors: [{ messageId: 'wrongWhere' }, { messageId: 'unreported' }]
        }
    ]
});

describe('expectedWhere', () => {
    it('is the repo-relative path, for relative and absolute filenames', async () => {
        const { strict: assert } = await import('node:assert');
        const { fileURLToPath } = await import('node:url');
        const path = await import('node:path');
        const root = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..', '..');
        assert.equal(expectedWhere('src/stores/account.ts'), 'src/stores/account.ts');
        assert.equal(expectedWhere('./mcp/src/server.ts'), 'mcp/src/server.ts');
        assert.equal(expectedWhere(path.join(root, 'src', 'lib', 'logger.ts')), 'src/lib/logger.ts');
        assert.equal(expectedWhere('<input>'), null);
        assert.equal(expectedWhere(''), null);
    });
});
