// A minimal flat config for scripts/error-file-coverage.mjs (pm/error_err.mdx §13.3): only
// errfile/catch-must-report, no type information, so it runs fast over src/ and mcp/src/ alike.
// The root eslint.config.mjs and mcp/eslint.config.mjs stay the developer-facing configs.

import tsParser from '@typescript-eslint/parser';
import vueParser from 'vue-eslint-parser';

import errfilePlugin, { ERRFILE_LINT_IGNORES } from './errfile-plugin.mjs';

export default [
    {
        ignores: ['**/node_modules/**', '**/dist/**', ...ERRFILE_LINT_IGNORES]
    },
    {
        files: ['**/*.vue'],
        languageOptions: {
            parser: vueParser,
            ecmaVersion: 2022,
            sourceType: 'module',
            parserOptions: { parser: tsParser, extraFileExtensions: ['.vue'] }
        }
    },
    {
        files: ['**/*.{ts,tsx,mts}'],
        languageOptions: {
            parser: tsParser,
            ecmaVersion: 2022,
            sourceType: 'module'
        }
    },
    {
        files: ['**/*.{vue,ts,tsx,mts}'],
        plugins: { errfile: errfilePlugin },
        rules: { 'errfile/catch-must-report': 'error' }
    }
];
