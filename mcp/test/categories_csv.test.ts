/**
 * ezb_set_transaction_categories_csv — the id-carrying CSV back into assignments (pm/mcp.mdx §11.5c).
 * Synthetic data only.
 */
import { describe, expect, it } from 'vitest';

import { csvAssignments, parseDelimited } from '../src/tools/writes.js';

describe('parseDelimited', () => {
  it('keeps quoted separators, doubled quotes and CRLF', () => {
    expect(parseDelimited('a,"b, c","say ""hi"""\r\n1,2,3\r\n', ',')).toEqual([
      ['a', 'b, c', 'say "hi"'],
      ['1', '2', '3'],
    ]);
  });
});

describe('csvAssignments', () => {
  const head = 'ID,Date,Description,New Category,New Counter Account';

  it('groups rows by category and counter account, leaving blank rows alone', () => {
    const csv = [
      head,
      '101,2020-01-01,"EXAMPLE CAFE, CITY ST",Expense > Food & Drink > Coffee Shops,',
      '102,2020-01-02,EXAMPLE CAFE,Expense > Food & Drink > Coffee Shops,',
      '103,2020-01-03,Transfer to child,Transfer > General Transfer > Bank Transfer,Child Savings',
      '104,2020-01-04,something unclear,,',
    ].join('\n');
    const out = csvAssignments(csv, 'ID', 'New Category', 'New Counter Account');
    expect(out.rows).toBe(4);
    expect(out.blank).toBe(1);
    expect(out.assignments).toEqual([
      { ids: ['101', '102'], category: 'Expense > Food & Drink > Coffee Shops' },
      { ids: ['103'], category: 'Transfer > General Transfer > Bank Transfer', counter_account_name: 'Child Savings' },
    ]);
  });

  it('reads TSV and matches headers case-insensitively', () => {
    const out = csvAssignments('id\tnew category\n7\tExpense > Shopping > Amazon\n', 'ID', 'New Category', 'New Counter Account');
    expect(out.assignments).toEqual([{ ids: ['7'], category: 'Expense > Shopping > Amazon' }]);
  });

  it('refuses a mangled id (a spreadsheet turned it into 3.84E+18) and a repeated id', () => {
    expect(() => csvAssignments('ID,New Category\n3.84E+18,Expense > A > B\n', 'ID', 'New Category', 'x')).toThrow(/not an ezBookkeeping id/);
    expect(() => csvAssignments('ID,New Category\n5,Expense > A > B\n5,Expense > A > C\n', 'ID', 'New Category', 'x')).toThrow(/appears twice/);
  });

  it('refuses a file without the id or category column', () => {
    expect(() => csvAssignments('Date,New Category\n', 'ID', 'New Category', 'x')).toThrow(/no "ID" column/);
    expect(() => csvAssignments('ID,Category\n', 'ID', 'New Category', 'x')).toThrow(/no "New Category" column/);
  });
});
