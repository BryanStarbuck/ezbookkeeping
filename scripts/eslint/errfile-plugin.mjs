// The `errfile` ESLint plugin — pm/error_err.mdx §13.2. One rule: errfile/catch-must-report.
//
// Registered in the root eslint.config.mjs (Vue + TS) and in mcp/eslint.config.mjs:
//   import errfilePlugin from '../scripts/eslint/errfile-plugin.mjs';   // from mcp/
//   { plugins: { errfile: errfilePlugin }, rules: { 'errfile/catch-must-report': 'warn' } }

import catchMustReport from './catch-must-report.mjs';

/**
 * Files the rule is off for (§13.2): tests, fixtures, the libraries themselves, build output — and
 * src/lib/logger.ts, the N4 net, whose whole job is to pass the caller's message on as `doing`.
 */
export const ERRFILE_LINT_IGNORES = [
    'src/lib/logger.ts',
    'src/**/__tests__/**',
    '**/*.test.ts',
    '**/*.spec.ts',
    'src/lib/errfile/**',
    'mcp/src/errfile/**',
    'mcp/test/**',
    'dist/**',
    'public/**',
    '**/*.d.ts'
];

const plugin = {
    meta: {
        name: 'eslint-plugin-errfile',
        version: '1.0.0'
    },
    rules: {
        'catch-must-report': catchMustReport
    }
};

export default plugin;
