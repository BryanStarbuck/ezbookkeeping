// src/lib/errfile — the UNIVERSAL entry (pm/error_err.mdx §5.1, §6.2).
//
// This is the only entry ordinary source files import: `import { errorFileFor } from '@/lib/errfile'`
// (in mcp/, `./errfile/index.js`). It never touches `fs`, the DOM, Vue or axios, so it is safe in a
// browser tab, the service worker and node alike. Hosts import `./browser.js` or `./node.js`
// exactly once, at boot (§8).

export {
    describeError,
    errorFileFor,
    flushErrorFile,
    guard,
    hasErrorSink,
    isAnswer,
    isReported,
    reportRejection,
    resolveErrorFilePath,
    setErrorSink,
    tryOr,
    tryOrAsync
} from './core.js';
export type {
    ErrorData,
    ErrorDataValue,
    ErrorFile,
    ErrorFilePathOptions,
    ErrorLevel,
    ErrorRecord,
    ErrorSink,
    RedactedData
} from './core.js';
export { setStatementsRoot } from './redact.js';
