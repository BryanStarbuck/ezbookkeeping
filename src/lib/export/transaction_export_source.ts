// Fetches EVERY transaction matching the transaction list's current filter, across all
// pages, for copy / download (pm/transaction_list.mdx §9.3). It reads the same
// /api/v1/transactions/list.json the list itself reads, so the exported rows are the rows
// the list would show page after page, in the same order (newest first), with each transfer
// once. It never touches the transactions store, so the list on screen is left as it is.

import services from '@/lib/services.ts';

import type { TransactionInfoResponse } from '@/models/transaction.ts';

export interface TransactionExportFilter {
    readonly minTime: number;      // unix seconds, 0 = no lower bound
    readonly maxTime: number;      // unix seconds, 0 = no upper bound
    readonly type: number;
    readonly categoryIds: string;
    readonly accountIds: string;
    readonly tagFilter: string;
    readonly amountFilter: string;
    readonly keyword: string;
    readonly matchMode: number;
}

export const TRANSACTION_EXPORT_PAGE_SIZE = 50;          // the server's maximum (count max=50)
export const TRANSACTION_EXPORT_MAX_ROWS = 100000;       // runaway guard: 2,000 requests

export async function fetchAllTransactionsForExport(filter: TransactionExportFilter): Promise<TransactionInfoResponse[]> {
    const all: TransactionInfoResponse[] = [];

    // same cursor rules as the store's loadTransactions: max_time is a time-sequence id
    // (unix ms * 1000 + sequence); the first page starts at the end of maxTime's second.
    let cursor = filter.maxTime > 0 ? filter.maxTime * 1000 + 999 : 0;
    const minTime = filter.minTime > 0 ? filter.minTime * 1000 : 0;

    for (;;) {
        const response = await services.getTransactions({
            maxTime: cursor,
            minTime: minTime,
            count: TRANSACTION_EXPORT_PAGE_SIZE,
            page: 1,
            withCount: false,
            withPictures: false,
            mustHavePictures: false,
            type: filter.type,
            categoryIds: filter.categoryIds,
            accountIds: filter.accountIds,
            tagFilter: filter.tagFilter,
            amountFilter: filter.amountFilter,
            keyword: filter.keyword,
            matchMode: filter.matchMode
        });

        const data = response.data;

        if (!data || !data.success || !data.result) {
            throw new Error('Unable to retrieve transaction list');
        }

        all.push(...(data.result.items || []));

        const next = data.result.nextTimeSequenceId;

        if (!next) {
            return all;
        }

        if (next >= cursor && cursor !== 0) {
            // the cursor must move strictly backwards in time, or we would loop forever
            throw new Error('transaction list cursor did not advance');
        }

        if (all.length >= TRANSACTION_EXPORT_MAX_ROWS) {
            throw new Error(`more than ${TRANSACTION_EXPORT_MAX_ROWS} transactions match; narrow the date range`);
        }

        cursor = next;
    }
}
