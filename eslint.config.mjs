import pluginVue from 'eslint-plugin-vue';
import vueTsEslintConfig from '@vue/eslint-config-typescript';
import errfilePlugin, { ERRFILE_LINT_IGNORES } from './scripts/eslint/errfile-plugin.mjs';

export default [
    ...pluginVue.configs['flat/essential'],
    ...vueTsEslintConfig(),
    {
        languageOptions: {
            parserOptions: {
                projectService: true,
                tsconfigRootDir: import.meta.dirname,
            }
        },
    },
    {
        ignores: [
            'dist/**',
            'mcp/**',
            '**/*.{js,jsx,cjs,mjs}'
        ]
    },
    {
        files: [
            '**/*.{vue,ts,tsx,mts,js,jsx,cjs,mjs}'
        ],
        rules: {
            'vue/valid-v-slot': ['error', {
                allowModifiers: true
            }]
        }
    },
    {
        files: ['src/**/*.{vue,ts,tsx,mts}'],
        ignores: ERRFILE_LINT_IGNORES,
        plugins: { errfile: errfilePlugin },
        rules: { 'errfile/catch-must-report': 'error' } // pm/error_err.mdx §13.2
    },
];
