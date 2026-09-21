import { defineConfig } from 'vitest/config';

// Three kinds of test file, one runner (pm/mcp.mdx §17):
//   test/**/*.test.ts        catalogue, gates, credentials vectors, instructions, live integration
//   src/canary/*.canary.ts   one file per name in the §7.0 threat table's Canary column
//   src/**/*.test.ts         the vendored error-file library's own tests (mcp/src/errfile/)
export default defineConfig({
  test: {
    environment: 'node',
    include: ['test/**/*.test.ts', 'src/canary/*.canary.ts', 'src/**/*.test.ts'],
    // Canaries grep dist/ and a scripted session spawns the built server; never run them twice at once.
    fileParallelism: false,
  },
});
