// Package render turns a machine-plane envelope into what stdout carries: the envelope as JSON
// (the default), an aligned table, or RFC 4180 CSV (cli.mdx §14). It is the only package that
// writes the answer to stdout.
package render

import (
	"bytes"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
	"unicode/utf8"
)

// Format is what stdout carries
type Format string

const (
	FormatJSON  Format = "json"
	FormatTable Format = "table"
	FormatCSV   Format = "csv"
)

// ParseFormat validates --format
func ParseFormat(v string) (Format, bool) {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "", "json":
		return FormatJSON, true
	case "table":
		return FormatTable, true
	case "csv":
		return FormatCSV, true
	}

	return "", false
}

// Kind decides how a column's cell renders
type Kind int

const (
	KText Kind = iota
	KMoney
	KInt
	KBool
)

// Column is one column of a table/CSV view
type Column struct {
	Header string
	// Key is a dotted path into the row ("category.name")
	Key  string
	Kind Kind
	// CurrencyKey names the row field holding this Money column's currency; Currency fixes it
	CurrencyKey string
	Currency    string
	// Indent, when set, is a row field whose integer value indents the first column (sub-accounts)
	Indent string
}

// View says how to lay out a response for table/CSV
type View struct {
	// Rows is the dotted path to the row array inside `data`; "" means data itself is one row
	Rows    string
	Columns []Column
	// Empty is printed on stderr (by the caller) when there are no rows
	Empty string
}

// Get follows a dotted path through maps
func Get(v any, path string) any {
	if path == "" {
		return v
	}

	cur := v

	for _, part := range strings.Split(path, ".") {
		m, ok := cur.(map[string]any)

		if !ok {
			return nil
		}

		cur = m[part]
	}

	return cur
}

// PrettyJSON writes raw JSON indented, exactly as received (numbers untouched)
func PrettyJSON(w io.Writer, raw []byte) error {
	var buf bytes.Buffer

	if err := json.Indent(&buf, raw, "", "  "); err != nil {
		_, err = w.Write(raw)
		return err
	}

	buf.WriteByte('\n')
	_, err := w.Write(buf.Bytes())

	return err
}

// Rows extracts the rows a view points at
func Rows(data any, view View) []map[string]any {
	target := Get(data, view.Rows)

	switch t := target.(type) {
	case []any:
		out := make([]map[string]any, 0, len(t))

		for _, r := range t {
			if m, ok := r.(map[string]any); ok {
				out = append(out, m)
			}
		}

		return out
	case map[string]any:
		return []map[string]any{t}
	}

	return nil
}

func cell(row map[string]any, c Column, forCSV bool) []string {
	v := Get(row, c.Key)

	switch c.Kind {
	case KMoney:
		cur := c.Currency

		if c.CurrencyKey != "" {
			if s, ok := Get(row, c.CurrencyKey).(string); ok {
				cur = s
			}
		}

		n, ok := ToInt64(v)

		if forCSV {
			if !ok {
				return []string{"", "", cur}
			}

			return []string{fmt.Sprint(n), Hundredths(n, false), cur}
		}

		if !ok {
			if v == nil {
				return []string{"—"}
			}

			return []string{fmt.Sprint(v)}
		}

		return []string{Money(n, cur)}
	case KBool:
		if b, ok := v.(bool); ok {
			if b {
				return []string{"yes"}
			}

			return []string{"no"}
		}
	}

	if v == nil {
		if forCSV {
			return []string{""}
		}

		return []string{"—"}
	}

	switch t := v.(type) {
	case []any:
		parts := make([]string, 0, len(t))

		for _, p := range t {
			parts = append(parts, fmt.Sprint(p))
		}

		return []string{strings.Join(parts, ",")}
	case map[string]any:
		data, _ := json.Marshal(t)
		return []string{string(data)}
	}

	return []string{fmt.Sprint(v)}
}

func headers(cols []Column, forCSV bool) []string {
	var out []string

	for _, c := range cols {
		if forCSV && c.Kind == KMoney {
			base := strings.ToLower(strings.ReplaceAll(c.Header, " ", "_"))
			out = append(out, base+"_hundredths", base, base+"_currency")
			continue
		}

		if forCSV {
			out = append(out, strings.ToLower(strings.ReplaceAll(c.Header, " ", "_")))
		} else {
			out = append(out, c.Header)
		}
	}

	return out
}

// CSV writes rows as RFC 4180 with a header row; money columns carry hundredths, decimal and currency
func CSV(w io.Writer, rows []map[string]any, cols []Column) error {
	cw := csv.NewWriter(w)

	if err := cw.Write(headers(cols, true)); err != nil {
		return err
	}

	for _, r := range rows {
		var rec []string

		for _, c := range cols {
			rec = append(rec, cell(r, c, true)...)
		}

		if err := cw.Write(rec); err != nil {
			return err
		}
	}

	cw.Flush()

	return cw.Error()
}

// Table writes rows as aligned columns; money right-aligned
func Table(w io.Writer, rows []map[string]any, cols []Column) error {
	hdr := headers(cols, false)
	grid := [][]string{hdr}

	for _, r := range rows {
		var line []string

		for i, c := range cols {
			v := cell(r, c, false)[0]

			if i == 0 && c.Indent != "" {
				if n, ok := ToInt64(Get(r, c.Indent)); ok && n > 0 {
					v = strings.Repeat("  ", int(n)) + v
				}
			}

			line = append(line, v)
		}

		grid = append(grid, line)
	}

	widths := make([]int, len(hdr))

	for _, line := range grid {
		for i, v := range line {
			if l := utf8.RuneCountInString(v); l > widths[i] {
				widths[i] = l
			}
		}
	}

	var b strings.Builder

	for li, line := range grid {
		for i, v := range line {
			pad := widths[i] - utf8.RuneCountInString(v)

			if cols[i].Kind == KMoney || cols[i].Kind == KInt {
				b.WriteString(strings.Repeat(" ", pad) + v)
			} else {
				b.WriteString(v)

				if i < len(line)-1 {
					b.WriteString(strings.Repeat(" ", pad))
				}
			}

			if i < len(line)-1 {
				b.WriteString("  ")
			}
		}

		b.WriteByte('\n')

		if li == 0 {
			for i := range line {
				b.WriteString(strings.Repeat("─", widths[i]))

				if i < len(line)-1 {
					b.WriteString("  ")
				}
			}

			b.WriteByte('\n')
		}
	}

	_, err := io.WriteString(w, b.String())

	return err
}

// KeyValues renders one object as "key  value" lines (table form of a single-object answer)
func KeyValues(w io.Writer, m map[string]any, moneyKeys map[string]string) error {
	keys := make([]string, 0, len(m))

	for k := range m {
		keys = append(keys, k)
	}

	sort.Strings(keys)
	width := 0

	for _, k := range keys {
		if len(k) > width {
			width = len(k)
		}
	}

	var b strings.Builder

	for _, k := range keys {
		v := m[k]
		s := ""

		if cur, ok := moneyKeys[k]; ok {
			if n, ok := ToInt64(v); ok {
				if c, ok := m[cur].(string); ok {
					s = Money(n, c)
				} else {
					s = Money(n, cur)
				}
			}
		}

		if s == "" {
			switch t := v.(type) {
			case nil:
				s = "—"
			case map[string]any, []any:
				data, _ := json.Marshal(t)
				s = string(data)
			default:
				s = fmt.Sprint(t)
			}
		}

		b.WriteString(k + strings.Repeat(" ", width-len(k)) + "  " + s + "\n")
	}

	_, err := io.WriteString(w, b.String())

	return err
}
