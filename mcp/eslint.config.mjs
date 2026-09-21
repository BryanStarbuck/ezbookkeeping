// ESLint for the ezbookkeeping MCP server — flat config, its own (the root config is upstream's Vue
// project). It registers the shared errfile rule (pm/error_err.mdx §13.2) at "warn" during Phase C.
import fs from 'node:fs';
import path from 'node:path';
import { fileURLToPath } from 'node:url';

import tseslint from 'typescript-eslint';

const here = path.dirname(fileURLToPath(import.meta.url));
const pluginPath = path.resolve(here, '..', 'scripts', 'eslint', 'errfile-plugin.mjs');

// The plugin is written by the error-file rollout (scripts/eslint/, owned by the root repo). If it
// is absent, fail loudly with the reason rather than linting without the rule and pretending.
if (!fs.existsSync(pluginPath)) {
  throw new Error(`mcp/eslint.config.mjs: the shared errfile ESLint plugin is missing at ${pluginPath} (pm/error_err.mdx §13.2). Run the root repo's error-file build (scripts/eslint/) first.`);
}
const { default: errfilePlugin } = await import(pluginPath);

export default tseslint.config(
  {
    // src/errfile/ is the vendored error-file library: drift-checked against src/lib/errfile and linted by the root config.
    ignores: ['dist/**', 'node_modules/**', 'src/instructions.ts', 'src/errfile/**', '*.mjs', '*.config.ts'],
  },
  ...tseslint.configs.recommendedTypeChecked,
  {
    languageOptions: {
      parserOptions: {
        project: ['./tsconfig.eslint.json'],
        tsconfigRootDir: here,
      },
    },
  },
  {
    files: ['**/*.ts'],
    plugins: { errfile: errfilePlugin },
    rules: {
      'errfile/catch-must-report': 'error',
      '@typescript-eslint/no-unused-vars': ['error', { argsIgnorePattern: '^_', varsIgnorePattern: '^_', destructuredArrayIgnorePattern: '^_' }],
      // stdout is the wire (pm/mcp.mdx §8.1): the canary greps dist/, and this catches it in the editor.
      'no-console': ['error', { allow: ['error', 'warn'] }],
      'no-restricted-properties': ['error', { object: 'process', property: 'stdout', message: 'stdout carries only JSON-RPC (pm/mcp.mdx §8.1)' }],
    },
  },
  {
    files: ['src/errfile/**', 'test/**', 'src/canary/**', 'scripts/**'],
    rules: {
      'errfile/catch-must-report': 'off',
      'no-console': 'off',
      'no-restricted-properties': 'off',
    },
  },
);
