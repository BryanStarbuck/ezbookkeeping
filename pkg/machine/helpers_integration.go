package machine

import (
	"errors"
	"sort"
	"strconv"
	"strings"

	"github.com/mayswind/ezbookkeeping/pkg/errfile"
	"github.com/mayswind/ezbookkeeping/pkg/errs"
)

// Integration glue between families that were built in parallel.

func init() {
	// import-fallout (analytics) finds a run's fallback categories in the ingest run report,
	// so `ezbk analytics import-fallout --run R` works without naming the categories again
	AnalyticsRunFallbackLookup = integRunFallbackLookup
}

func integRunFallbackLookup(mc *Ctx, runId string) ([]int64, bool, error) {
	if runId == "" || !ingRunIdPattern(runId) {
		return nil, false, nil
	}

	// an unreadable or missing report is "unknown run": the route then asks for the ids
	rep, err := ingLoadRunReport(mc.Uid, runId)

	if err != nil {
		errfile.Expected("loading the run report for the fallback lookup", err)
		return nil, false, nil
	}

	seen := map[int64]bool{}
	ids := []int64{}

	add := func(s string) {
		s = strings.TrimSpace(s)

		if s == "" {
			return
		}

		id, err := strconv.ParseInt(s, 10, 64)

		if err != nil || id <= 0 || seen[id] {
			errfile.Expected("parsing a transaction id from the run report", err)
			return
		}

		seen[id] = true
		ids = append(ids, id)
	}

	// the categories the run actually put rows in as a fallback; the transfer category an
	// accepted transfer needs is not a fallback for an unmatched category, so it is left out
	for _, row := range rep.Fallback {
		if row["kind"] != "transfer" {
			add(row["category_id"])
		}
	}

	if len(ids) == 0 {
		for kind, v := range rep.FallbackCategoryIds {
			if kind != "transfer" {
				add(v)
			}
		}
	}

	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })

	return ids, len(ids) > 0, nil
}

// integNameRow adds display names to a transaction row whose upstream source carries ids only
// (upstream's reconciliation statement): sourceAccountName, destinationAccountName, categoryName
// (leaf name), categoryPath ("Parent > Child") and tagNames. Existing keys are never overwritten.
func integNameRow(row map[string]any, lk *txnLookup) {
	if row == nil || lk == nil {
		return
	}

	setIfAbsent := func(k, v string) {
		if _, ok := row[k]; !ok && v != "" {
			row[k] = v
		}
	}

	if id, ok := txnAsInt64(row["sourceAccountId"]); ok {
		setIfAbsent("sourceAccountName", lk.accountName(id))
	}

	if id, ok := txnAsInt64(row["destinationAccountId"]); ok && id != 0 {
		setIfAbsent("destinationAccountName", lk.accountName(id))
	}

	if id, ok := txnAsInt64(row["categoryId"]); ok && id != 0 {
		if c := lk.catMap[id]; c != nil {
			setIfAbsent("categoryName", c.Name)
		}

		setIfAbsent("categoryPath", lk.categoryPath(id))
	}

	if _, ok := row["tagNames"]; !ok {
		ids := []string{}

		if list, ok := row["tagIds"].([]any); ok {
			for _, v := range list {
				ids = append(ids, txnAsString(v))
			}
		} else if list, ok := row["tagIds"].([]string); ok {
			ids = list
		}

		row["tagNames"] = lk.tagNames(ids)
	}
}

// integFillTags fills tagIds on statement rows (upstream's reconciliation statement leaves them
// empty) from the rows' own tag index. Best effort: on any read error the rows are left as they are.
func integFillTags(mc *Ctx, rows []map[string]any) {
	if len(rows) == 0 || len(rows) > MaxLimit {
		return
	}

	ids := make([]int64, 0, len(rows))

	for _, r := range rows {
		if id, ok := txnAsInt64(r["id"]); ok {
			ids = append(ids, id)
		}
	}

	states, canon, _, err := txnReadStates(mc, ids)

	if err != nil {
		errfile.Caught("reading the transaction states to fill the tag ids", err)
		return
	}

	for _, r := range rows {
		id, ok := txnAsInt64(r["id"])

		if !ok {
			continue
		}

		key := id

		if c, ok := canon[id]; ok {
			key = c
		}

		if st := states[key]; st != nil && len(st.TagIds) > 0 {
			tagIds := make([]any, 0, len(st.TagIds))

			for _, t := range st.TagIds {
				tagIds = append(tagIds, t)
			}

			r["tagIds"] = tagIds
		}
	}
}

// integIsCategoryIdInvalid reports upstream's "transaction category id is invalid"
func integIsCategoryIdInvalid(err error) bool {
	var ue *errs.Error

	if errors.As(err, &ue) {
		return ue == errs.ErrTransactionCategoryIdInvalid
	}

	var f *Fail

	return errors.As(err, &f) && f.UpstreamCode == errs.ErrTransactionCategoryIdInvalid.Code()
}
