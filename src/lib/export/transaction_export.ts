// Copy / download of the transaction list (pm/transaction_list.mdx §9).
//
// Pure functions only: raw list-API items in, text out. No Vue, no stores, no network,
// so every rule about rows, columns, signs and escaping is unit-tested in isolation.

import { TransactionType } from '@/core/transaction.ts';

import type { TransactionInfoResponse } from '@/models/transaction.ts';

export type TransactionExportFormat = 'csv' | 'yaml' | 'markdown';

export interface TransactionExportLookups {
    // each returns undefined when the id is unknown (deleted or not loaded)
    account(id: string): { name: string, currency: string } | undefined;
    category(id: string): { name: string, parentId: string } | undefined;
    tag(id: string): { name: string } | undefined;
}

// One exported row. Every format carries exactly these fields, in this order.
export interface TransactionExportRow {
    date: string;          // YYYY-MM-DD in the transaction's own time zone
    time: string;          // HH:mm:ss in the transaction's own time zone
    timezone: string;      // UTC offset of that zone, e.g. -08:00
    type: string;          // Income | Expense | Transfer | Modify Balance
    category: string;      // primary category ('' for Modify Balance)
    subcategory: string;   // the transaction's own category when it is a secondary one
    account: string;       // source account ("from" for a transfer)
    amount: string;        // signed decimal, source account's view: money out is negative
    currency: string;      // source account currency
    toAccount: string;     // transfers only
    toAmount: string;      // transfers only, positive, in the destination currency
    toCurrency: string;    // transfers only
    tags: string[];
    description: string;
    id: string;            // int64 as a string: never a JS number (precision)
}

export const TRANSACTION_EXPORT_COLUMNS: ReadonlyArray<{ key: keyof TransactionExportRow, header: string, yamlKey: string, numeric?: boolean }> = [
    { key: 'date', header: 'Date', yamlKey: 'date' },
    { key: 'time', header: 'Time', yamlKey: 'time' },
    { key: 'timezone', header: 'Timezone', yamlKey: 'timezone' },
    { key: 'type', header: 'Type', yamlKey: 'type' },
    { key: 'category', header: 'Category', yamlKey: 'category' },
    { key: 'subcategory', header: 'Subcategory', yamlKey: 'subcategory' },
    { key: 'account', header: 'Account', yamlKey: 'account' },
    { key: 'amount', header: 'Amount', yamlKey: 'amount', numeric: true },
    { key: 'currency', header: 'Currency', yamlKey: 'currency' },
    { key: 'toAccount', header: 'To Account', yamlKey: 'to_account' },
    { key: 'toAmount', header: 'To Amount', yamlKey: 'to_amount', numeric: true },
    { key: 'toCurrency', header: 'To Currency', yamlKey: 'to_currency' },
    { key: 'tags', header: 'Tags', yamlKey: 'tags' },
    { key: 'description', header: 'Description', yamlKey: 'description' },
    { key: 'id', header: 'ID', yamlKey: 'id' }
];

export const TAG_JOINER = '; ';

const TYPE_NAMES: Record<number, string> = {
    [TransactionType.ModifyBalance]: 'Modify Balance',
    [TransactionType.Income]: 'Income',
    [TransactionType.Expense]: 'Expense',
    [TransactionType.Transfer]: 'Transfer'
};

function pad2(n: number): string {
    return n < 10 ? `0${n}` : `${n}`;
}

// Integer hundredths -> "-1234.56". No grouping, '.' decimal, no currency symbol.
// ezBookkeeping keeps two decimal places for every currency.
export function formatHundredths(hundredths: number): string {
    if (!Number.isFinite(hundredths)) {
        return '';
    }

    const value = Math.trunc(hundredths);
    const abs = Math.abs(value);
    const units = Math.floor(abs / 100);
    const cents = abs % 100;

    return `${value < 0 ? '-' : ''}${units}.${pad2(cents)}`;
}

// minutes east of UTC -> "+05:30" / "-08:00"
export function formatUtcOffset(offsetMinutes: number): string {
    const sign = offsetMinutes < 0 ? '-' : '+';
    const abs = Math.abs(Math.trunc(offsetMinutes));

    return `${sign}${pad2(Math.floor(abs / 60))}:${pad2(abs % 60)}`;
}

// unix seconds + the transaction's own UTC offset -> wall-clock date and time there
export function formatTransactionLocalTime(unixSeconds: number, offsetMinutes: number): { date: string, time: string } {
    const shifted = new Date((unixSeconds + offsetMinutes * 60) * 1000);

    return {
        date: `${shifted.getUTCFullYear()}-${pad2(shifted.getUTCMonth() + 1)}-${pad2(shifted.getUTCDate())}`,
        time: `${pad2(shifted.getUTCHours())}:${pad2(shifted.getUTCMinutes())}:${pad2(shifted.getUTCSeconds())}`
    };
}

// Source-account view: income adds, expense and transfer-out subtract, modify balance is
// the stored adjustment (already signed). A negative expense (a refund) comes out positive.
export function signedSourceHundredths(type: number, sourceAmount: number): number {
    if (type === TransactionType.Expense || type === TransactionType.Transfer) {
        return -sourceAmount;
    }

    return sourceAmount;
}

export function toTransactionExportRow(item: TransactionInfoResponse, lookups: TransactionExportLookups): TransactionExportRow {
    const { date, time } = formatTransactionLocalTime(item.time, item.utcOffset);
    const sourceAccount = lookups.account(item.sourceAccountId);

    let category = '';
    let subcategory = '';

    if (item.type !== TransactionType.ModifyBalance && item.categoryId && item.categoryId !== '0') {
        const own = lookups.category(item.categoryId);

        if (own) {
            const parent = own.parentId && own.parentId !== '0' ? lookups.category(own.parentId) : undefined;

            if (parent) {
                category = parent.name;
                subcategory = own.name;
            } else {
                category = own.name;
            }
        }
    }

    let toAccount = '';
    let toAmount = '';
    let toCurrency = '';

    if (item.type === TransactionType.Transfer) {
        const destinationAccount = lookups.account(item.destinationAccountId);
        toAccount = destinationAccount?.name ?? '';
        toCurrency = destinationAccount?.currency ?? '';
        toAmount = formatHundredths(item.destinationAmount);
    }

    const tags: string[] = [];

    for (const tagId of item.tagIds || []) {
        const tag = lookups.tag(tagId);

        if (tag) {
            tags.push(tag.name);
        }
    }

    return {
        date,
        time,
        timezone: formatUtcOffset(item.utcOffset),
        type: TYPE_NAMES[item.type] ?? String(item.type),
        category,
        subcategory,
        account: sourceAccount?.name ?? '',
        amount: formatHundredths(signedSourceHundredths(item.type, item.sourceAmount)),
        currency: sourceAccount?.currency ?? '',
        toAccount,
        toAmount,
        toCurrency,
        tags,
        description: item.comment ?? '',
        id: String(item.id)
    };
}

function cellText(row: TransactionExportRow, key: keyof TransactionExportRow): string {
    const value = row[key];
    return Array.isArray(value) ? value.join(TAG_JOINER) : value;
}

// ---------- CSV (RFC 4180) ----------

function csvField(text: string): string {
    if (/[",\r\n]/.test(text) || text !== text.trim()) {
        return `"${text.replace(/"/g, '""')}"`;
    }

    return text;
}

export function toCsv(rows: TransactionExportRow[]): string {
    const lines: string[] = [TRANSACTION_EXPORT_COLUMNS.map(c => csvField(c.header)).join(',')];

    for (const row of rows) {
        lines.push(TRANSACTION_EXPORT_COLUMNS.map(c => csvField(cellText(row, c.key))).join(','));
    }

    return lines.join('\r\n') + '\r\n';
}

// ---------- Markdown (GitHub-flavored table) ----------

function markdownCell(text: string): string {
    return text
        .replace(/\\/g, '\\\\')
        .replace(/\|/g, '\\|')
        .replace(/\r\n|\r|\n/g, '<br>');
}

export function toMarkdown(rows: TransactionExportRow[]): string {
    const header = '| ' + TRANSACTION_EXPORT_COLUMNS.map(c => c.header).join(' | ') + ' |';
    const rule = '|' + TRANSACTION_EXPORT_COLUMNS.map(c => (c.numeric ? ' ---: ' : ' --- ')).join('|') + '|';
    const lines = [header, rule];

    for (const row of rows) {
        lines.push('| ' + TRANSACTION_EXPORT_COLUMNS.map(c => markdownCell(cellText(row, c.key))).join(' | ') + ' |');
    }

    return lines.join('\n') + '\n';
}

// ---------- YAML ----------

// JSON string syntax is a valid YAML double-quoted scalar, so every string is emitted that
// way: no bare scalar can be misread as a number, a boolean, null or a date.
function yamlString(text: string): string {
    return JSON.stringify(text);
}

export function toYaml(rows: TransactionExportRow[]): string {
    if (!rows.length) {
        return 'transactions: []\n';
    }

    const lines: string[] = ['transactions:'];

    for (const row of rows) {
        let first = true;

        for (const column of TRANSACTION_EXPORT_COLUMNS) {
            const prefix = first ? '  - ' : '    ';
            first = false;

            const value = row[column.key];
            let rendered: string;

            if (Array.isArray(value)) {
                rendered = value.length ? `[${value.map(yamlString).join(', ')}]` : '[]';
            } else if (value === '' && (column.key === 'toAccount' || column.key === 'toAmount' || column.key === 'toCurrency')) {
                rendered = 'null';
            } else {
                rendered = yamlString(value);
            }

            lines.push(`${prefix}${column.yamlKey}: ${rendered}`);
        }
    }

    return lines.join('\n') + '\n';
}

export function renderTransactionExport(format: TransactionExportFormat, rows: TransactionExportRow[]): string {
    if (format === 'csv') {
        return toCsv(rows);
    } else if (format === 'yaml') {
        return toYaml(rows);
    } else {
        return toMarkdown(rows);
    }
}

export const TRANSACTION_EXPORT_FILE: Record<TransactionExportFormat, { extension: string, mimeType: string }> = {
    csv: { extension: 'csv', mimeType: 'text/csv;charset=utf-8' },
    yaml: { extension: 'yaml', mimeType: 'application/yaml;charset=utf-8' },
    markdown: { extension: 'md', mimeType: 'text/markdown;charset=utf-8' }
};

// transactions_2025-12-01_2025-12-31.csv, or transactions_all.csv with no date range.
// minDate / maxDate are YYYY-MM-DD strings already resolved by the caller (or '').
export function transactionExportFileName(format: TransactionExportFormat, minDate: string, maxDate: string): string {
    const extension = TRANSACTION_EXPORT_FILE[format].extension;

    if (minDate && maxDate) {
        return `transactions_${minDate}_${maxDate}.${extension}`;
    } else if (minDate) {
        return `transactions_from_${minDate}.${extension}`;
    } else if (maxDate) {
        return `transactions_until_${maxDate}.${extension}`;
    }

    return `transactions_all.${extension}`;
}
