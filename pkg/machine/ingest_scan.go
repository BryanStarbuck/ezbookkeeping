package machine

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/mayswind/ezbookkeeping/pkg/errfile"
)

// ingest_scan.go — discovery (apis.mdx §14.4 /ingest/scan and /ingest/coverage; cli.mdx §10.3).
//
// Walk the root, classify every file, and derive a statement identity — entity, bank, account,
// last4 (from the directory AND the file name; a disagreement is reported, never guessed), period,
// sha256 and which sidecars exist. Scanning changes nothing.

// ingScanStatement is one statement: the files that share a directory and a stem
type ingScanStatement struct {
	Stem         string   `json:"stem"`
	Dir          string   `json:"dir"`
	Entity       string   `json:"entity"`
	Institution  string   `json:"institution"`
	Account      string   `json:"account"`
	Last4        string   `json:"last4,omitempty"`
	AccountKey   string   `json:"account_key"`
	Period       string   `json:"period,omitempty"`
	PeriodDate   string   `json:"period_date,omitempty"`
	PeriodSource string   `json:"period_source"`
	Files        []string `json:"files"`
	Primary      string   `json:"primary"`
	Sha256       string   `json:"sha256,omitempty"`
	Sidecars     []string `json:"sidecars"`
	Importable   []string `json:"importable"`
	HasPDF       bool     `json:"has_pdf"`
	Usable       bool     `json:"usable"`
	Warnings     []string `json:"warnings"`

	sidecarPaths map[string]string
	realDir      string
	year         string
}

// ingScanAccount groups statements of one account
type ingScanAccount struct {
	AccountKey     string           `json:"account_key"`
	Entity         string           `json:"entity"`
	Institution    string           `json:"institution"`
	Account        string           `json:"account"`
	Last4          string           `json:"last4,omitempty"`
	Path           string           `json:"path,omitempty"`
	Statements     int              `json:"statements"`
	First          string           `json:"first,omitempty"`
	Last           string           `json:"last,omitempty"`
	Years          []*ingScanYear   `json:"years"`
	MissingMonths  []string         `json:"missing_months"`
	DuplicateScans []map[string]any `json:"duplicate_scans"`
	Unusable       []string         `json:"unusable"`
	Undated        []string         `json:"undated"`
	Warnings       []string         `json:"warnings"`
	stmts          []*ingScanStatement
	manifest       *ingManifestRow
}

type ingScanYear struct {
	Year       string         `json:"year"`
	Statements int            `json:"statements"`
	Months     []ingScanMonth `json:"months"`
}

type ingScanMonth struct {
	Month      string `json:"month"`
	Statements int    `json:"statements"`
}

// ingScanResult is the whole walk
type ingScanResult struct {
	Root         string            `json:"root"`
	Mode         string            `json:"mode"`
	Layout       string            `json:"layout"`
	ManifestPath string            `json:"manifest_path,omitempty"`
	Accounts     []*ingScanAccount `json:"accounts"`
	Unclassified []string          `json:"unclassified"`
	Unattributed []string          `json:"unattributed"`
	Totals       map[string]int    `json:"totals"`
	Filters      map[string]string `json:"filters,omitempty"`
	Truncated    bool              `json:"truncated,omitempty"`
}

var (
	ingYearDir    = regexp.MustCompile(`^(19|20)\d{2}$`)
	ingMonthDir   = regexp.MustCompile(`^(0[1-9]|1[0-2])$`)
	ingDateInName = regexp.MustCompile(`(?:^|[^0-9])((?:19|20)\d{2})[-_]?(0[1-9]|1[0-2])[-_]?(0[1-9]|[12]\d|3[01])(?:[^0-9]|$)`)
	ingMonthName  = regexp.MustCompile(`(?:^|[^0-9])((?:19|20)\d{2})[-_](0[1-9]|1[0-2])(?:[^0-9]|$)`)
	ingDigitRun   = regexp.MustCompile(`\d+`)
	ingLast4Dir   = regexp.MustCompile(`(?:^|[^0-9])(\d{4})$`)
)

const ingScanMaxFiles = 200000

// ingSidecarKind returns claude/brew/ocr for a sidecar file name, "" otherwise
func ingSidecarKind(name string) string {
	lower := strings.ToLower(name)

	switch {
	case strings.HasSuffix(lower, "_claude.txt"):
		return "claude"
	case strings.HasSuffix(lower, "_brew.txt"):
		return "brew"
	case strings.HasSuffix(lower, "_ocr.txt"):
		return "ocr"
	}

	return ""
}

// ingStem strips sidecar suffixes and the extension
func ingStem(name string) string {
	lower := strings.ToLower(name)

	for _, suf := range []string{"_claude.txt", "_brew.txt", "_ocr.txt"} {
		if strings.HasSuffix(lower, suf) {
			return name[:len(name)-len(suf)]
		}
	}

	return strings.TrimSuffix(name, filepath.Ext(name))
}

// ingDateFromName finds a YYYYMMDD (or YYYY-MM) date in a file name
func ingDateFromName(name string) (date, month string) {
	if m := ingDateInName.FindStringSubmatch(name); m != nil {
		d := m[1] + "-" + m[2] + "-" + m[3]

		if _, err := time.Parse("2006-01-02", d); err == nil {
			return d, m[1] + "-" + m[2]
		}
	}

	if m := ingMonthName.FindStringSubmatch(name); m != nil {
		return "", m[1] + "-" + m[2]
	}

	return "", ""
}

// ingLast4FromName finds exactly one 4-digit run in a file name that is not part of its date
func ingLast4FromName(name string) string {
	stem := ingStem(name)
	var found []string

	for _, loc := range ingDigitRun.FindAllStringIndex(stem, -1) {
		run := stem[loc[0]:loc[1]]

		if len(run) != 4 {
			continue
		}

		// a year that begins a YYYY-MM or YYYY_MM date is not a last4
		if ingYearDir.MatchString(run) && loc[1] < len(stem) && (stem[loc[1]] == '-' || stem[loc[1]] == '_') {
			if rest := stem[loc[1]+1:]; len(rest) >= 2 && ingMonthDir.MatchString(rest[:2]) {
				continue
			}
		}

		found = append(found, run)
	}

	if len(found) == 1 {
		return found[0]
	}

	return ""
}

// ingScan walks the root. The manifest (when present) attributes files to accounts by path;
// otherwise the layout {ENTITY}/{BANK}/{ACCOUNT}/{YYYY}/{MM}/ is read from the directories.
func ingScan(root *ingRoot, manifest *ingManifest, mode string, filters map[string]string, qifOrder string) *ingScanResult {
	res := &ingScanResult{Root: root.Display, Mode: mode, Layout: "directories", Accounts: []*ingScanAccount{}, Unclassified: []string{}, Unattributed: []string{}, Totals: map[string]int{}, Filters: filters}

	if manifest != nil {
		res.Layout = "manifest"
		res.ManifestPath = manifest.Path
	}

	// manifest account prefixes, longest first
	type prefix struct {
		path string
		row  *ingManifestRow
	}

	var prefixes []prefix

	if manifest != nil {
		for _, row := range manifest.Rows {
			if row.Path != "" {
				prefixes = append(prefixes, prefix{path: row.Path, row: row})
			}
		}

		sort.Slice(prefixes, func(i, j int) bool { return len(prefixes[i].path) > len(prefixes[j].path) })
	}

	accounts := map[string]*ingScanAccount{}
	stmts := map[string]*ingScanStatement{}
	var manifestReal string

	if manifest != nil {
		manifestReal, _ = root.Resolve(manifest.Path)
	}

	count := 0

	_ = filepath.WalkDir(root.Real, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			errfile.Warn("walking the statements root", err)
			return nil
		}

		if d.IsDir() {
			if p != root.Real && ingSkipDir(d.Name()) {
				return filepath.SkipDir
			}

			return nil
		}

		if strings.HasPrefix(d.Name(), ".") {
			return nil
		}

		count++

		if count > ingScanMaxFiles {
			res.Truncated = true
			return filepath.SkipAll
		}

		real, rerr := filepath.EvalSymlinks(p)

		if rerr != nil || !ingWithin(root.Real, real) {
			errfile.Expected("resolving a scanned file's real path", rerr)
			return nil
		}

		rel := root.Rel(real)
		res.Totals["files"]++

		if real == manifestReal {
			return nil
		}

		name := d.Name()
		lower := strings.ToLower(name)
		sidecar := ingSidecarKind(name)
		isPDF := strings.HasSuffix(lower, ".pdf")
		format, _, _ := ingClassifyImportable(real, qifOrder)

		if sidecar == "" && !isPDF && format == "" {
			if len(res.Unclassified) < 500 {
				res.Unclassified = append(res.Unclassified, rel)
			}

			res.Totals["unclassified"]++

			return nil
		}

		dirRel := filepath.ToSlash(filepath.Dir(rel))

		if dirRel == "." {
			dirRel = ""
		}

		// attribute to an account
		var entity, bank, acct, last4, acctPath string
		var mrow *ingManifestRow

		for _, pf := range prefixes {
			if dirRel == pf.path || strings.HasPrefix(dirRel, pf.path+"/") {
				mrow = pf.row
				entity, bank, acct, last4, acctPath = pf.row.Entity, pf.row.Institution, pf.row.Label, pf.row.Last4, pf.row.Path
				break
			}
		}

		comps := []string{}

		if dirRel != "" {
			comps = strings.Split(dirRel, "/")
		}

		yearIdx := -1

		for i, c := range comps {
			if ingYearDir.MatchString(c) {
				yearIdx = i
				break
			}
		}

		if mrow == nil {
			switch {
			case yearIdx >= 3:
				entity, bank, acct = comps[yearIdx-3], comps[yearIdx-2], comps[yearIdx-1]
				acctPath = strings.Join(comps[:yearIdx], "/")
			case yearIdx < 0 && len(comps) >= 3:
				entity, bank, acct = comps[len(comps)-3], comps[len(comps)-2], comps[len(comps)-1]
				acctPath = strings.Join(comps, "/")
			default:
				if len(res.Unattributed) < 500 {
					res.Unattributed = append(res.Unattributed, rel)
				}

				res.Totals["unattributed"]++

				return nil
			}

			if m := ingLast4Dir.FindStringSubmatch(acct); m != nil {
				last4 = m[1]
			}
		}

		key := ingAccountKey(entity, bank, last4, acct)

		if filters["entity"] != "" && !strings.EqualFold(filters["entity"], entity) {
			return nil
		}

		if filters["institution"] != "" && !strings.EqualFold(filters["institution"], bank) {
			return nil
		}

		if f := filters["account"]; f != "" && !strings.EqualFold(f, acct) && !strings.EqualFold(f, key) && f != last4 && !strings.EqualFold(f, acctPath) {
			return nil
		}

		sa := accounts[key]

		if sa == nil {
			sa = &ingScanAccount{AccountKey: key, Entity: entity, Institution: bank, Account: acct, Last4: last4, Path: acctPath, Years: []*ingScanYear{}, MissingMonths: []string{}, DuplicateScans: []map[string]any{}, Unusable: []string{}, Undated: []string{}, Warnings: []string{}, manifest: mrow}
			accounts[key] = sa
		}

		stemKey := dirRel + "/" + ingStem(name)
		st := stmts[stemKey]

		if st == nil {
			st = &ingScanStatement{Stem: ingStem(name), Dir: dirRel, Entity: entity, Institution: bank, Account: acct, Last4: last4, AccountKey: key, Files: []string{}, Sidecars: []string{}, Importable: []string{}, Warnings: []string{}, sidecarPaths: map[string]string{}, realDir: filepath.Dir(real)}

			if yearIdx >= 0 {
				st.year = comps[yearIdx]
			}

			// period: file name first, then the {YYYY}/{MM} directories
			if date, month := ingDateFromName(name); month != "" {
				st.Period, st.PeriodDate, st.PeriodSource = month, date, "filename"
			} else if yearIdx >= 0 && yearIdx+1 < len(comps) && ingMonthDir.MatchString(comps[yearIdx+1]) {
				st.Period, st.PeriodSource = comps[yearIdx]+"-"+comps[yearIdx+1], "directory"
			} else {
				st.PeriodSource = "none"
			}

			if yearIdx >= 0 && st.Period != "" && !strings.HasPrefix(st.Period, comps[yearIdx]) && st.PeriodSource == "filename" {
				st.Warnings = append(st.Warnings, "the file name's date "+st.Period+" is not in the year directory "+comps[yearIdx])
			}

			// last4 from the file name must agree with the directory's
			if fl := ingLast4FromName(name); fl != "" && last4 != "" && fl != last4 {
				st.Warnings = append(st.Warnings, "the file name says last4 "+fl+" but the account says "+last4+"; not guessed")
			}

			stmts[stemKey] = st
			sa.stmts = append(sa.stmts, st)
		}

		st.Files = append(st.Files, rel)

		switch {
		case sidecar != "":
			st.Sidecars = append(st.Sidecars, sidecar)
			st.sidecarPaths[sidecar] = real
			res.Totals["sidecars"]++
		case isPDF:
			st.HasPDF = true
			res.Totals["pdfs"]++

			if st.Primary == "" {
				st.Primary = rel
			}
		default:
			st.Importable = append(st.Importable, format)
			res.Totals["importable"]++

			if st.Primary == "" || !st.HasPDF {
				st.Primary = rel
			}
		}

		return nil
	})

	keys := make([]string, 0, len(accounts))

	for k := range accounts {
		keys = append(keys, k)
	}

	sort.Strings(keys)

	for _, k := range keys {
		sa := accounts[k]
		ingFinishScanAccount(root, sa)
		res.Accounts = append(res.Accounts, sa)
		res.Totals["statements"] += sa.Statements
		res.Totals["missing_months"] += len(sa.MissingMonths)
		res.Totals["duplicate_scans"] += len(sa.DuplicateScans)
		res.Totals["unusable"] += len(sa.Unusable)
	}

	res.Totals["accounts"] = len(res.Accounts)

	if res.Mode == "" {
		if manifest != nil && res.Totals["importable"] > 0 {
			res.Mode = "prepared"
		} else {
			res.Mode = "raw"
		}
	}

	return res
}

// ingFinishScanAccount computes per-account summaries: years, missing months, duplicate scans
func ingFinishScanAccount(root *ingRoot, sa *ingScanAccount) {
	sort.Slice(sa.stmts, func(i, j int) bool {
		if sa.stmts[i].Period != sa.stmts[j].Period {
			return sa.stmts[i].Period < sa.stmts[j].Period
		}

		return sa.stmts[i].Dir+"/"+sa.stmts[i].Stem < sa.stmts[j].Dir+"/"+sa.stmts[j].Stem
	})

	byMonth := map[string]int{}
	bySha := map[string]string{}

	for _, st := range sa.stmts {
		sort.Strings(st.Files)
		sort.Strings(st.Sidecars)
		st.Usable = len(st.Importable) > 0

		for kind, p := range st.sidecarPaths {
			if info, err := os.Stat(p); err == nil && info.Size() > 0 {
				st.Usable = true
				_ = kind
			}
		}

		if st.Primary != "" {
			if real, err := root.Resolve(st.Primary); err == nil {
				if data, err := ingReadLimited(real, 256<<20); err == nil {
					st.Sha256 = ingSha256(data)
				}
			}
		}

		if !st.Usable {
			sa.Unusable = append(sa.Unusable, ingJoinRel(st.Dir, st.Stem))
		}

		if st.Period == "" {
			sa.Undated = append(sa.Undated, ingJoinRel(st.Dir, st.Stem))
		} else {
			byMonth[st.Period]++
		}

		if st.Sha256 != "" {
			if prev, ok := bySha[st.Sha256]; ok {
				sa.DuplicateScans = append(sa.DuplicateScans, map[string]any{"verdict": "duplicate_identical", "file": st.Primary, "same_as": prev})
			} else {
				bySha[st.Sha256] = st.Primary
			}
		}

		for _, w := range st.Warnings {
			sa.Warnings = append(sa.Warnings, ingJoinRel(st.Dir, st.Stem)+": "+w)
		}
	}

	sa.Statements = len(sa.stmts)
	months := make([]string, 0, len(byMonth))

	for m, n := range byMonth {
		months = append(months, m)

		if n > 1 {
			sa.DuplicateScans = append(sa.DuplicateScans, map[string]any{"verdict": "same_period", "month": m, "statements": n})
		}
	}

	sort.Strings(months)

	if len(months) > 0 {
		sa.First, sa.Last = months[0], months[len(months)-1]
		present := map[string]bool{}

		for _, m := range months {
			present[m] = true
		}

		for _, m := range ingMonthsBetween(sa.First+"-01", sa.Last+"-01") {
			if !present[m] {
				sa.MissingMonths = append(sa.MissingMonths, m)
			}
		}
	}

	years := map[string]*ingScanYear{}

	for _, m := range months {
		y := m[:4]

		if years[y] == nil {
			years[y] = &ingScanYear{Year: y, Months: []ingScanMonth{}}
		}

		years[y].Months = append(years[y].Months, ingScanMonth{Month: m, Statements: byMonth[m]})
		years[y].Statements += byMonth[m]
	}

	ys := make([]string, 0, len(years))

	for y := range years {
		ys = append(ys, y)
	}

	sort.Strings(ys)

	for _, y := range ys {
		sa.Years = append(sa.Years, years[y])
	}

	sort.SliceStable(sa.DuplicateScans, func(i, j int) bool {
		return ingAnyString(sa.DuplicateScans[i]) < ingAnyString(sa.DuplicateScans[j])
	})
}

func ingJoinRel(dir, name string) string {
	if dir == "" {
		return name
	}

	return dir + "/" + name
}

func ingAnyString(m map[string]any) string {
	var parts []string

	for _, k := range []string{"verdict", "month", "file", "same_as", "statements"} {
		if v, ok := m[k]; ok {
			switch t := v.(type) {
			case string:
				parts = append(parts, t)
			case int:
				parts = append(parts, strconv.Itoa(t))
			}
		}
	}

	return strings.Join(parts, "|")
}

// ingYearFilter filters scan accounts to one year (keeps the account, trims its years)
func ingYearFilter(res *ingScanResult, year string) {
	if year == "" {
		return
	}

	for _, sa := range res.Accounts {
		var ys []*ingScanYear

		for _, y := range sa.Years {
			if y.Year == year {
				ys = append(ys, y)
			}
		}

		if ys == nil {
			ys = []*ingScanYear{}
		}

		sa.Years = ys

		var missing []string

		for _, m := range sa.MissingMonths {
			if strings.HasPrefix(m, year+"-") {
				missing = append(missing, m)
			}
		}

		if missing == nil {
			missing = []string{}
		}

		sa.MissingMonths = missing
	}
}
