// GENERATED from pkg/errfile — run scripts/sync-errfile.sh
package errfile

import (
	"strings"
)

// Record → line(s) (§3.2):
//
//	[ts] [LEVEL] [app] [where] doing — Type: message (code=…) {k=v …} | cause: …
//	    at frame
//	    at frame

func field(s string) string {
	return stripControlChars(s)
}

// FormatData renders the data block, " {k=v k=v}", capped at DataCap; "" when empty.
func FormatData(data []KV) string {
	if len(data) == 0 {
		return ""
	}

	pairs := make([]string, 0, len(data))

	for _, kv := range data {
		pairs = append(pairs, field(kv.Key)+"="+field(kv.Value))
	}

	return " {" + capMiddle(strings.Join(pairs, " "), DataCap) + "}"
}

// FormatHeader renders the single header line, without a trailing newline.
func FormatHeader(r *Record) string {
	app := r.App

	if app == "" {
		app = "?"
	}

	var b strings.Builder
	b.WriteString("[" + r.TS.UTC().Format("2006-01-02T15:04:05.000Z") + "] ")
	b.WriteString("[" + field(string(r.Level)) + "] ")
	b.WriteString("[" + field(app) + "] ")
	b.WriteString("[" + field(r.Where) + "] ")
	b.WriteString(field(r.Doing))

	if r.Error != "" {
		b.WriteString(" — " + field(r.Error))
	}

	b.WriteString(FormatData(r.Data))
	b.WriteString(field(r.Cause))

	return b.String()
}

// FormatRecord renders the whole record — header plus indented stack lines — capped at RecordCap
// and newline-terminated. The header is kept whole when it fits; the stack is what gets cut.
func FormatRecord(r *Record) string {
	header := FormatHeader(r)
	text := header

	if len(r.Stack) > 0 {
		var b strings.Builder
		b.WriteString(header)

		for i, fr := range r.Stack {
			if i >= StackFrames {
				break
			}

			b.WriteString("\n    at " + field(fr))
		}

		text = b.String()
	}

	if len(text) <= RecordCap {
		return text + "\n"
	}

	if len(header) >= RecordCap {
		return capMiddle(header, RecordCap) + "\n"
	}

	runes := []rune(text)

	if len(runes) > RecordCap {
		runes = runes[:RecordCap]
	}

	return string(runes) + "\n"
}
