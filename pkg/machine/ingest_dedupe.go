package machine

import (
	"errors"
	"sort"
	"strings"
	"time"

	"github.com/mayswind/ezbookkeeping/pkg/errfile"
)

// ingest_dedupe.go — the two de-duplication layers (apis.mdx §14.5).
//
// Layer one, the statement level: the same month scanned or downloaded twice is one statement.
// Statements of one account are compared by what they CONTAIN (dates, amounts, currency,
// descriptions read from inside the document), never by their directory or file name:
//
//   - identical bytes                          → duplicate_identical
//   - in their overlapping date window, one's rows are a subset of the other's
//                                              → the fuller one wins; the loser is superseded,
//                                                ranked by a fixed rule chain (more rows; wider
//                                                coverage; better source; newer mtime;
//                                                lexicographically first path)
//   - overlapping windows that share no row    → two different statements (a cycle-dated card):
//                                                both contribute
//   - each has rows the other lacks            → conflict: that account-month is blocked and both
//                                                files are named, until `prefer` picks one
//
// Layer two, the transaction level, is the import_id (ingest_identity.go); a bank id that appears
// in two primary statements collapses to one row here.

// ingStatement is one parsed statement (a file) of one account
type ingStatement struct {
	File     string
	FileType string
	Sha      string
	ModTime  time.Time
	Rows     []*ingRow
	First    string
	Last     string
	// PeriodSource says where First/Last came from ("rows" or "document")
	PeriodSource string
	Preferred    bool
}

// ingDupeVerdict is layer one's verdict on one statement
type ingDupeVerdict struct {
	AccountKey string `json:"account_key"`
	File       string `json:"file"`
	Verdict    string `json:"verdict"` // primary | duplicate_identical | superseded | conflict | empty
	Rule       string `json:"rule,omitempty"`
	Winner     string `json:"winner,omitempty"`
	First      string `json:"first,omitempty"`
	Last       string `json:"last,omitempty"`
	Rows       int    `json:"rows"`
	RowsUsed   int    `json:"rows_used"`
}

// ingConflict is anything that blocks rows from importing, named with its files
type ingConflict struct {
	AccountKey string   `json:"account_key"`
	Kind       string   `json:"kind"`
	Files      []string `json:"files,omitempty"`
	Months     []string `json:"months,omitempty"`
	Message    string   `json:"message"`
	Hint       string   `json:"hint,omitempty"`
	Rows       int      `json:"rows,omitempty"`
}

// ingCollapse is one row layer two collapsed
type ingCollapse struct {
	AccountKey      string `json:"account_key"`
	ImportId        string `json:"import_id"`
	Rule            string `json:"rule"`
	KeptSource      string `json:"kept_source"`
	CollapsedSource string `json:"collapsed_source"`
	Date            string `json:"date"`
	Amount          int64  `json:"amount"`
	Currency        string `json:"currency"`
}

// ingLayerOneResult is layer one's output for one account
type ingLayerOneResult struct {
	Rows          []*ingRow
	Verdicts      []*ingDupeVerdict
	Conflicts     []*ingConflict
	BlockedMonths map[string]bool
	BlockedRows   int
	Collapsed     int
}

func (s *ingStatement) bounds() {
	if s.PeriodSource == "document" && s.First != "" && s.Last != "" {
		return
	}

	s.First, s.Last = "", ""

	for _, r := range s.Rows {
		if s.First == "" || r.Date < s.First {
			s.First = r.Date
		}

		if s.Last == "" || r.Date > s.Last {
			s.Last = r.Date
		}
	}

	s.PeriodSource = "rows"
}

func ingDays(first, last string) int {
	a, err1 := time.Parse("2006-01-02", first)
	b, err2 := time.Parse("2006-01-02", last)

	if err1 != nil || err2 != nil {
		errfile.Expected("parsing the first and last row dates of a statement", errors.Join(err1, err2))
		return 0
	}

	return int(b.Sub(a).Hours()/24) + 1
}

// ingDecisiveRule names the first rule of the chain that ranks a above b ("" when they tie
// completely, which cannot happen for two distinct paths)
func ingDecisiveRule(a, b *ingStatement) string {
	switch {
	case a.Preferred != b.Preferred:
		return "preferred"
	case len(a.Rows) != len(b.Rows):
		return "more_rows"
	case ingDays(a.First, a.Last) != ingDays(b.First, b.Last):
		return "wider_coverage"
	case ingSourceRank(a.FileType) != ingSourceRank(b.FileType):
		return "better_source"
	case !a.ModTime.Equal(b.ModTime):
		return "newer_mtime"
	case a.File != b.File:
		return "first_path"
	}

	return ""
}

// ingRankLess reports whether a ranks above b in the fixed chain
func ingRankLess(a, b *ingStatement) bool {
	switch ingDecisiveRule(a, b) {
	case "preferred":
		return a.Preferred
	case "more_rows":
		return len(a.Rows) > len(b.Rows)
	case "wider_coverage":
		return ingDays(a.First, a.Last) > ingDays(b.First, b.Last)
	case "better_source":
		return ingSourceRank(a.FileType) < ingSourceRank(b.FileType)
	case "newer_mtime":
		return a.ModTime.After(b.ModTime)
	case "first_path":
		return a.File < b.File
	}

	return false
}

// ingMonthsBetween lists the calendar months touched by [first, last]
func ingMonthsBetween(first, last string) []string {
	if len(first) < 7 || len(last) < 7 {
		return nil
	}

	a, err1 := time.Parse("2006-01", first[:7])
	b, err2 := time.Parse("2006-01", last[:7])

	if err1 != nil || err2 != nil || b.Before(a) {
		errfile.Expected("parsing the first and last months of a statement", errors.Join(err1, err2))
		return nil
	}

	var out []string

	for m := a; !m.After(b); m = m.AddDate(0, 1, 0) {
		out = append(out, m.Format("2006-01"))

		if len(out) > 1200 {
			break
		}
	}

	return out
}

// ingLayerOne runs statement-level de-duplication over one account's statements
func ingLayerOne(accountKey string, stmts []*ingStatement) *ingLayerOneResult {
	res := &ingLayerOneResult{BlockedMonths: map[string]bool{}}

	for _, s := range stmts {
		s.bounds()
	}

	// empty statements are reported, never silently dropped
	var live []*ingStatement

	for _, s := range stmts {
		if len(s.Rows) == 0 {
			res.Verdicts = append(res.Verdicts, &ingDupeVerdict{AccountKey: accountKey, File: s.File, Verdict: "empty", Rule: "no_rows"})
			continue
		}

		live = append(live, s)
	}

	// identical bytes → one statement
	sort.SliceStable(live, func(i, j int) bool { return live[i].File < live[j].File })
	firstBySha := map[string]*ingStatement{}
	var distinct []*ingStatement

	for _, s := range live {
		if s.Sha != "" {
			if w, ok := firstBySha[s.Sha]; ok {
				res.Verdicts = append(res.Verdicts, &ingDupeVerdict{AccountKey: accountKey, File: s.File, Verdict: "duplicate_identical", Rule: "identical_bytes", Winner: w.File, First: s.First, Last: s.Last, Rows: len(s.Rows)})
				continue
			}

			firstBySha[s.Sha] = s
		}

		distinct = append(distinct, s)
	}

	sort.SliceStable(distinct, func(i, j int) bool { return ingRankLess(distinct[i], distinct[j]) })

	var acc []*ingStatement
	var out []*ingRow

	for _, s := range distinct {
		claimed := make([]bool, len(s.Rows))
		var conflictWith *ingStatement
		var conflictFirst, conflictLast string
		rule, winner := "", ""

		for _, p := range acc {
			if s.Last < p.First || p.Last < s.First {
				continue
			}

			wFirst, wLast := s.First, s.Last

			if p.First > wFirst {
				wFirst = p.First
			}

			if p.Last < wLast {
				wLast = p.Last
			}

			// the window's rows of p, as a multiset
			pm := map[string]int{}
			pTotal := 0

			for _, r := range p.Rows {
				if r.Date >= wFirst && r.Date <= wLast {
					pm[r.Key()]++
					pTotal++
				}
			}

			// the window's unclaimed rows of s
			var sIdx []int
			sm := map[string]int{}

			for i, r := range s.Rows {
				if !claimed[i] && r.Date >= wFirst && r.Date <= wLast {
					sIdx = append(sIdx, i)
					sm[r.Key()]++
				}
			}

			if len(sIdx) == 0 {
				continue
			}

			shared, sOnly, pOnly := 0, 0, 0

			for k, n := range sm {
				m := pm[k]

				if n <= m {
					shared += n
				} else {
					shared += m
					sOnly += n - m
				}
			}

			for k, m := range pm {
				if n := sm[k]; m > n {
					pOnly += m - n
				}
			}

			switch {
			case shared == 0 && pTotal > 0:
				// two different statements whose periods touch: both contribute
				continue
			case sOnly == 0 || pOnly == 0 || (p.Preferred && !s.Preferred):
				// subset either way (the fuller content wins inside the window), or p was chosen
				take := map[string]int{}

				for k, n := range sm {
					m := pm[k]

					if p.Preferred && !s.Preferred {
						take[k] = n
					} else if n <= m {
						take[k] = n
					} else {
						take[k] = m
					}
				}

				for _, i := range sIdx {
					k := s.Rows[i].Key()

					if take[k] > 0 {
						take[k]--
						claimed[i] = true
					}
				}

				if winner == "" {
					winner = p.File
					rule = ingDecisiveRule(p, s)

					if p.Preferred && !s.Preferred {
						rule = "preferred"
					}
				}
			default:
				conflictWith = p
				conflictFirst, conflictLast = wFirst, wLast
			}

			if conflictWith != nil {
				break
			}
		}

		if conflictWith != nil {
			months := ingMonthsBetween(conflictFirst, conflictLast)

			for _, m := range months {
				res.BlockedMonths[m] = true
			}

			res.Verdicts = append(res.Verdicts, &ingDupeVerdict{AccountKey: accountKey, File: s.File, Verdict: "conflict", Rule: "rows_disagree", Winner: conflictWith.File, First: s.First, Last: s.Last, Rows: len(s.Rows)})
			res.Conflicts = append(res.Conflicts, &ingConflict{
				AccountKey: accountKey,
				Kind:       "statement_disagreement",
				Files:      []string{conflictWith.File, s.File},
				Months:     months,
				Message:    "two statements cover the same dates and disagree about which transactions exist",
				Hint:       "open both files, then re-run with prefer naming the correct one (ezbk statements dupes --prefer FILE)",
			})

			continue
		}

		used := 0

		for i, r := range s.Rows {
			if !claimed[i] {
				out = append(out, r)
				used++
			}
		}

		v := &ingDupeVerdict{AccountKey: accountKey, File: s.File, First: s.First, Last: s.Last, Rows: len(s.Rows), RowsUsed: used}

		if used == 0 {
			v.Verdict, v.Rule, v.Winner = "superseded", rule, winner
		} else {
			v.Verdict = "primary"

			if winner != "" {
				v.Rule, v.Winner = "adds_rows_missing_from_winner", winner
			}
		}

		res.Verdicts = append(res.Verdicts, v)
		acc = append(acc, s)
	}

	if len(res.BlockedMonths) > 0 {
		kept := out[:0]

		for _, r := range out {
			if res.BlockedMonths[r.Month] {
				res.BlockedRows++
				continue
			}

			kept = append(kept, r)
		}

		out = kept
	}

	res.Rows = out

	sort.SliceStable(res.Verdicts, func(i, j int) bool { return res.Verdicts[i].File < res.Verdicts[j].File })

	return res
}

// ingLayerTwo collapses rows that share an import_id (only a bank id can repeat across primary
// statements; minted ids are unique by construction)
func ingLayerTwo(rows []*ingRow) ([]*ingRow, []*ingCollapse) {
	seen := map[string]*ingRow{}
	var out []*ingRow
	var collapsed []*ingCollapse

	ordered := make([]*ingRow, len(rows))
	copy(ordered, rows)
	sort.SliceStable(ordered, func(i, j int) bool {
		if ordered[i].SourceFile != ordered[j].SourceFile {
			return ordered[i].SourceFile < ordered[j].SourceFile
		}

		return ordered[i].SourceIndex < ordered[j].SourceIndex
	})

	for _, r := range ordered {
		if k, ok := seen[r.ImportId]; ok {
			collapsed = append(collapsed, &ingCollapse{AccountKey: r.AccountKey, ImportId: r.ImportId, Rule: "same_bank_id", KeptSource: k.SourceFile, CollapsedSource: r.SourceFile, Date: r.Date, Amount: r.Amount, Currency: r.Currency})
			continue
		}

		seen[r.ImportId] = r
		out = append(out, r)
	}

	ingSortRows(out)

	return out, collapsed
}

// ingPreferSet normalises the prefer argument (paths relative to the root)
func ingPreferSet(prefer []string) map[string]bool {
	out := map[string]bool{}

	for _, p := range prefer {
		p = strings.Trim(strings.TrimSpace(p), "/")

		if p != "" {
			out[p] = true
		}
	}

	return out
}
