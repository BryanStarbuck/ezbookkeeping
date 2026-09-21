// Test helper (not a test): an in-memory sink.
import type { ErrorRecord, ErrorSink } from '../core.js';
import { formatRecord } from '../format.js';

export type MemorySink = ErrorSink & {
    records: ErrorRecord[];
    lines: () => string[];
};

export function memorySink(app = 'test', verbose = false): MemorySink {
    const records: ErrorRecord[] = [];
    return {
        app,
        verbose,
        records,
        write(record) {
            records.push(record);
        },
        flush() {
            // nothing buffered: the test sink is synchronous
        },
        lines() {
            return records.map(r => formatRecord(r));
        }
    };
}
