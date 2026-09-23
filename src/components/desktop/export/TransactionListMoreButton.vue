<template>
    <v-btn class="ms-3" color="default" variant="outlined"
           :disabled="disabled || busy" :loading="busy"
           v-if="isDataExportingEnabled()">
        More
        <v-icon :icon="mdiChevronDown" end />
        <v-menu activator="parent" location="bottom end">
            <v-list>
                <v-list-item :prepend-icon="mdiContentCopy" @click="run('csv', 'copy')">
                    <v-list-item-title>Copy as CSV</v-list-item-title>
                </v-list-item>
                <v-list-item :prepend-icon="mdiContentCopy" @click="run('yaml', 'copy')">
                    <v-list-item-title>Copy as YAML</v-list-item-title>
                </v-list-item>
                <v-list-item :prepend-icon="mdiContentCopy" @click="run('markdown', 'copy')">
                    <v-list-item-title>Copy as Markdown</v-list-item-title>
                </v-list-item>
                <v-divider class="my-1" />
                <v-list-item :prepend-icon="mdiDownload" @click="run('csv', 'download')">
                    <v-list-item-title>Download CSV</v-list-item-title>
                </v-list-item>
                <v-list-item :prepend-icon="mdiDownload" @click="run('markdown', 'download')">
                    <v-list-item-title>Download Markdown</v-list-item-title>
                </v-list-item>
                <v-list-item :prepend-icon="mdiDownload" @click="run('yaml', 'download')">
                    <v-list-item-title>Download YAML</v-list-item-title>
                </v-list-item>
            </v-list>
        </v-menu>
    </v-btn>

    <snack-bar ref="snackbar" />
</template>

<script setup lang="ts">
// The transaction list's "More ▾" button: copy / download every transaction matching the
// list's current filter as CSV, YAML or Markdown (pm/transaction_list.mdx §9). Fork code;
// upstream's ListPage.vue only gains the one tag that mounts this component.
import SnackBar from '@/components/desktop/SnackBar.vue';

import { ref, useTemplateRef } from 'vue';

import { useAccountsStore } from '@/stores/account.ts';
import { useTransactionCategoriesStore } from '@/stores/transactionCategory.ts';
import { useTransactionTagsStore } from '@/stores/transactionTag.ts';
import { useTransactionsStore } from '@/stores/transaction.ts';

import { parseDateTimeFromUnixTime } from '@/lib/datetime.ts';
import { isDataExportingEnabled } from '@/lib/server_settings.ts';
import { copyTextToClipboard, startDownloadFile } from '@/lib/ui/common.ts';
import {
    type TransactionExportFormat,
    type TransactionExportLookups,
    TRANSACTION_EXPORT_FILE,
    renderTransactionExport,
    toTransactionExportRow,
    transactionExportFileName
} from '@/lib/export/transaction_export.ts';
import { fetchAllTransactionsForExport } from '@/lib/export/transaction_export_source.ts';
import { errorFileFor } from '@/lib/errfile/index.ts';

import {
    mdiChevronDown,
    mdiContentCopy,
    mdiDownload
} from '@mdi/js';

const errors = errorFileFor('src/components/desktop/export/TransactionListMoreButton.vue');

type SnackBarType = InstanceType<typeof SnackBar>;

defineProps<{
    disabled?: boolean;
}>();

const accountsStore = useAccountsStore();
const transactionCategoriesStore = useTransactionCategoriesStore();
const transactionTagsStore = useTransactionTagsStore();
const transactionsStore = useTransactionsStore();

const snackbar = useTemplateRef<SnackBarType>('snackbar');

const busy = ref<boolean>(false);

const FORMAT_NAMES: Record<TransactionExportFormat, string> = {
    csv: 'CSV',
    yaml: 'YAML',
    markdown: 'Markdown'
};

const lookups: TransactionExportLookups = {
    account: id => accountsStore.allAccountsMap[id],
    category: id => transactionCategoriesStore.allTransactionCategoriesMap[id],
    tag: id => transactionTagsStore.allTransactionTagsMap[id]
};

async function buildText(format: TransactionExportFormat): Promise<{ text: string, count: number }> {
    const filter = transactionsStore.transactionsFilter;
    const items = await fetchAllTransactionsForExport({
        minTime: filter.minTime,
        maxTime: filter.maxTime,
        type: filter.type,
        categoryIds: filter.categoryIds,
        accountIds: filter.accountIds,
        tagFilter: filter.tagFilter,
        amountFilter: filter.amountFilter,
        keyword: filter.keyword,
        matchMode: filter.matchMode
    });
    const rows = items.map(item => toTransactionExportRow(item, lookups));

    return { text: renderTransactionExport(format, rows), count: rows.length };
}

function fileName(format: TransactionExportFormat): string {
    const filter = transactionsStore.transactionsFilter;
    const minDate = filter.minTime > 0 ? parseDateTimeFromUnixTime(filter.minTime).getGregorianCalendarYearDashMonthDashDay() : '';
    const maxDate = filter.maxTime > 0 ? parseDateTimeFromUnixTime(filter.maxTime).getGregorianCalendarYearDashMonthDashDay() : '';

    return transactionExportFileName(format, minDate, maxDate);
}

// Copy keeps the click's user activation (Safari drops it across an await) by handing the
// clipboard a promise; browsers without promise-valued ClipboardItem get writeText, then the
// app's own execCommand-based helper.
async function copy(format: TransactionExportFormat): Promise<number> {
    const pending = buildText(format);

    if (typeof ClipboardItem !== 'undefined' && navigator.clipboard && navigator.clipboard.write) {
        try {
            await navigator.clipboard.write([
                new ClipboardItem({ 'text/plain': pending.then(result => new Blob([result.text], { type: 'text/plain' })) })
            ]);
            return (await pending).count;
        } catch (e) {
            errors.expected('copying with a promise-valued ClipboardItem', e);
        }
    }

    const result = await pending;

    if (navigator.clipboard && navigator.clipboard.writeText) {
        try {
            await navigator.clipboard.writeText(result.text);
            return result.count;
        } catch (e) {
            errors.expected('copying with navigator.clipboard.writeText', e);
        }
    }

    copyTextToClipboard(result.text);
    return result.count;
}

async function download(format: TransactionExportFormat): Promise<number> {
    const result = await buildText(format);
    const file = TRANSACTION_EXPORT_FILE[format];
    // a UTF-8 BOM lets Excel open non-ASCII names correctly; copied text never carries one
    const parts = format === 'csv' ? ['\uFEFF', result.text] : [result.text];

    startDownloadFile(fileName(format), new Blob(parts, { type: file.mimeType }));
    return result.count;
}

function run(format: TransactionExportFormat, action: 'copy' | 'download'): void {
    if (busy.value) {
        return;
    }

    busy.value = true;

    const work = action === 'copy' ? copy(format) : download(format);

    work.then(count => {
        busy.value = false;
        const noun = count === 1 ? 'transaction' : 'transactions';

        if (action === 'copy') {
            snackbar.value?.showMessage(`Copied ${count} ${noun} as ${FORMAT_NAMES[format]}`);
        } else {
            snackbar.value?.showMessage(`Downloaded ${count} ${noun} as ${FORMAT_NAMES[format]}`);
        }
    }).catch(error => {
        errors.caught('exporting the transaction list from the More menu', error, { format: format, action: action });
        busy.value = false;

        if (!error || !error.processed) {
            snackbar.value?.showError(error instanceof Error ? error.message : (error?.message ?? 'Unable to retrieve transaction list'));
        }
    });
}
</script>
