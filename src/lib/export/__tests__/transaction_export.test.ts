import { describe, expect, it } from 'vitest';

import { TransactionType } from '@/core/transaction.ts';
import type { TransactionInfoResponse } from '@/models/transaction.ts';

import {
    type TransactionExportLookups,
    type TransactionExportRow,
    formatHundredths,
    formatTransactionLocalTime,
    formatUtcOffset,
    renderTransactionExport,
    signedSourceHundredths,
    toCsv,
    toMarkdown,
    toTransactionExportRow,
    toYaml,
    transactionExportFileName
} from '@/lib/export/transaction_export.ts';

// Synthetic data only (CLAUDE.md: never real statements in this repo).
const ACCOUNTS: Record<string, { name: string, currency: string }> = {
    '101': { name: 'Northbank Checking ••4021', currency: 'USD' },
    '102': { name: 'Cash Wallet', currency: 'USD' },
    '103': { name: 'Meridian Savings', currency: 'EUR' }
};

const CATEGORIES: Record<string, { name: string, parentId: string }> = {
    '201': { name: 'Food & Dining', parentId: '0' },
    '202': { name: 'Groceries', parentId: '201' },
    '301': { name: 'Transfers', parentId: '0' },
    '302': { name: 'ATM Withdrawal', parentId: '301' },
    '401': { name: 'Salary', parentId: '0' }
};

const TAGS: Record<string, { name: string }> = {
    '501': { name: 'household' },
    '502': { name: 'acme_llc' }
};

const lookups: TransactionExportLookups = {
    account: id => ACCOUNTS[id],
    category: id => CATEGORIES[id],
    tag: id => TAGS[id]
};

function item(overrides: Partial<TransactionInfoResponse>): TransactionInfoResponse {
    return {
        id: '9007199254740993001',
        timeSequenceId: '1764800000000000',
        type: TransactionType.Expense,
        categoryId: '202',
        time: 1764800000,        // 2025-12-03T22:13:20Z
        utcOffset: -480,
        sourceAccountId: '101',
        destinationAccountId: '0',
        sourceAmount: 4217,
        destinationAmount: 0,
        hideAmount: false,
        tagIds: ['501'],
        comment: 'Corner Market',
        editable: true,
        ...overrides
    };
}

describe('formatHundredths', () => {
    it('formats integer hundredths without grouping or symbol', () => {
        expect(formatHundredths(0)).toBe('0.00');
        expect(formatHundredths(5)).toBe('0.05');
        expect(formatHundredths(4217)).toBe('42.17');
        expect(formatHundredths(-123456789)).toBe('-1234567.89');
        expect(formatHundredths(-5)).toBe('-0.05');
    });
});

describe('formatUtcOffset / formatTransactionLocalTime', () => {
    it('formats offsets', () => {
        expect(formatUtcOffset(-480)).toBe('-08:00');
        expect(formatUtcOffset(330)).toBe('+05:30');
        expect(formatUtcOffset(0)).toBe('+00:00');
    });

    it('shows wall-clock time in the transaction zone, crossing midnight', () => {
        // 2025-12-01T07:30:00Z is still Nov 30 in UTC-8
        expect(formatTransactionLocalTime(1764574200, -480)).toEqual({ date: '2025-11-30', time: '23:30:00' });
        expect(formatTransactionLocalTime(1764574200, 0)).toEqual({ date: '2025-12-01', time: '07:30:00' });
    });
});

describe('signedSourceHundredths', () => {
    it('makes money leaving the source account negative', () => {
        expect(signedSourceHundredths(TransactionType.Expense, 4217)).toBe(-4217);
        expect(signedSourceHundredths(TransactionType.Transfer, 20000)).toBe(-20000);
        expect(signedSourceHundredths(TransactionType.Income, 500000)).toBe(500000);
        expect(signedSourceHundredths(TransactionType.ModifyBalance, -1500)).toBe(-1500);
        expect(signedSourceHundredths(TransactionType.Expense, -999)).toBe(999); // a refund
    });
});

describe('toTransactionExportRow', () => {
    it('maps an expense with primary and secondary category', () => {
        const row = toTransactionExportRow(item({}), lookups);

        expect(row).toEqual({
            date: '2025-12-03',
            time: '14:13:20',
            timezone: '-08:00',
            type: 'Expense',
            category: 'Food & Dining',
            subcategory: 'Groceries',
            account: 'Northbank Checking ••4021',
            amount: '-42.17',
            currency: 'USD',
            toAccount: '',
            toAmount: '',
            toCurrency: '',
            tags: ['household'],
            description: 'Corner Market',
            id: '9007199254740993001'
        });
    });

    it('maps a withdrawal transfer with both sides', () => {
        const row = toTransactionExportRow(item({
            type: TransactionType.Transfer,
            categoryId: '302',
            sourceAccountId: '101',
            destinationAccountId: '102',
            sourceAmount: 20000,
            destinationAmount: 20000,
            tagIds: [],
            comment: 'ATM 5th & Main'
        }), lookups);

        expect(row.type).toBe('Transfer');
        expect(row.category).toBe('Transfers');
        expect(row.subcategory).toBe('ATM Withdrawal');
        expect(row.account).toBe('Northbank Checking ••4021');
        expect(row.amount).toBe('-200.00');
        expect(row.toAccount).toBe('Cash Wallet');
        expect(row.toAmount).toBe('200.00');
        expect(row.toCurrency).toBe('USD');
    });

    it('keeps each side of a cross-currency transfer in its own currency', () => {
        const row = toTransactionExportRow(item({
            type: TransactionType.Transfer,
            categoryId: '302',
            destinationAccountId: '103',
            sourceAmount: 10850,
            destinationAmount: 10000
        }), lookups);

        expect([row.amount, row.currency, row.toAmount, row.toCurrency]).toEqual(['-108.50', 'USD', '100.00', 'EUR']);
    });

    it('leaves categories empty for modify balance and uses a primary category alone', () => {
        const balance = toTransactionExportRow(item({ type: TransactionType.ModifyBalance, categoryId: '0', sourceAmount: 100000 }), lookups);
        expect([balance.type, balance.category, balance.subcategory, balance.amount]).toEqual(['Modify Balance', '', '', '1000.00']);

        const income = toTransactionExportRow(item({ type: TransactionType.Income, categoryId: '401', sourceAmount: 500000 }), lookups);
        expect([income.category, income.subcategory, income.amount]).toEqual(['Salary', '', '5000.00']);
    });

    it('survives unknown ids', () => {
        const row = toTransactionExportRow(item({ sourceAccountId: '999', categoryId: '999', tagIds: ['999'] }), lookups);
        expect([row.account, row.currency, row.category, row.tags]).toEqual(['', '', '', []]);
    });
});

const tricky: TransactionExportRow = {
    date: '2025-12-03', time: '14:13:20', timezone: '-08:00', type: 'Expense',
    category: 'Food & Dining', subcategory: 'Groceries', account: 'Northbank Checking',
    amount: '-42.17', currency: 'USD', toAccount: '', toAmount: '', toCurrency: '',
    tags: ['household', 'acme_llc'],
    description: 'Say "hi", pay | split\nline two',
    id: '9007199254740993001'
};

describe('toCsv', () => {
    it('writes a header and RFC 4180 quoting with CRLF', () => {
        const csv = toCsv([tricky]);
        const lines = csv.split('\r\n');

        expect(lines[0]).toBe('Date,Time,Timezone,Type,Category,Subcategory,Account,Amount,Currency,To Account,To Amount,To Currency,Tags,Description,ID');
        expect(csv).toContain(',"Say ""hi"", pay | split\nline two",9007199254740993001\r\n');
        expect(csv).toContain(',household; acme_llc,');
        expect(csv.endsWith('\r\n')).toBe(true);
    });

    it('quotes fields with leading or trailing spaces and writes only the header for no rows', () => {
        expect(toCsv([{ ...tricky, description: ' padded ' }])).toContain(',Groceries,Northbank Checking,-42.17,USD,,,,household; acme_llc," padded ",9007199254740993001\r\n');
        expect(toCsv([])).toBe('Date,Time,Timezone,Type,Category,Subcategory,Account,Amount,Currency,To Account,To Amount,To Currency,Tags,Description,ID\r\n');
    });
});

describe('toMarkdown', () => {
    it('writes a GFM table with escaped pipes, <br> newlines and right-aligned amounts', () => {
        const md = toMarkdown([tricky]).split('\n');

        expect(md[0]).toBe('| Date | Time | Timezone | Type | Category | Subcategory | Account | Amount | Currency | To Account | To Amount | To Currency | Tags | Description | ID |');
        expect(md[1]).toBe('| --- | --- | --- | --- | --- | --- | --- | ---: | --- | --- | ---: | --- | --- | --- | --- |');
        expect(md[2]).toContain('| Say "hi", pay \\| split<br>line two |');
        expect(md).toHaveLength(4); // header, rule, one row, trailing ''
    });
});

describe('toYaml', () => {
    it('quotes every string, keeps the id and amounts as strings, uses null for absent transfer sides', () => {
        const yaml = toYaml([tricky]);

        expect(yaml).toBe([
            'transactions:',
            '  - date: "2025-12-03"',
            '    time: "14:13:20"',
            '    timezone: "-08:00"',
            '    type: "Expense"',
            '    category: "Food & Dining"',
            '    subcategory: "Groceries"',
            '    account: "Northbank Checking"',
            '    amount: "-42.17"',
            '    currency: "USD"',
            '    to_account: null',
            '    to_amount: null',
            '    to_currency: null',
            '    tags: ["household", "acme_llc"]',
            '    description: "Say \\"hi\\", pay | split\\nline two"',
            '    id: "9007199254740993001"',
            ''
        ].join('\n'));
    });

    it('writes an empty list for no rows', () => {
        expect(toYaml([])).toBe('transactions: []\n');
    });
});

describe('renderTransactionExport / transactionExportFileName', () => {
    it('dispatches by format', () => {
        expect(renderTransactionExport('csv', [])).toBe(toCsv([]));
        expect(renderTransactionExport('yaml', [])).toBe(toYaml([]));
        expect(renderTransactionExport('markdown', [])).toBe(toMarkdown([]));
    });

    it('names files by date range', () => {
        expect(transactionExportFileName('csv', '2025-12-01', '2025-12-31')).toBe('transactions_2025-12-01_2025-12-31.csv');
        expect(transactionExportFileName('markdown', '', '')).toBe('transactions_all.md');
        expect(transactionExportFileName('yaml', '2025-12-01', '')).toBe('transactions_from_2025-12-01.yaml');
    });
});
