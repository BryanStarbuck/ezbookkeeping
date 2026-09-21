package errfile

import (
	"fmt"
	"net/url"
	"path"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"sync/atomic"
)

// Privacy (§12). The library enforces this; call sites are not trusted to (R12). A data key is
// tested against the ledger pattern FIRST (dropped), then the secret pattern (redacted); string
// values then pass through the URL query redaction and the statements-root rewrite.

// Redacted replaces the value of a secret-named key.
const Redacted = "[redacted]"

// LedgerRefused replaces the value of a ledger-named key.
const LedgerRefused = "[ledger-field refused]"

// EnvStatementsDir names the statements root the library learns at Install (§12.7).
const EnvStatementsDir = "EZBK_STATEMENTS_DIR"

var (
	secretKeyRe = regexp.MustCompile(`(?i)pass(word)?|secret|token|auth|cookie|session|key|signature|credential|bearer|totp|otp|passcode`)
	ledgerKeyRe = regexp.MustCompile(`(?i)amount|balance|payee|comment|notes?|memo|account_?name|category_?name|tag_?name|description|statement|iban|card|number`)
	urlRe       = regexp.MustCompile(`(?i)\b[a-z][a-z0-9+.\-]*://[^\s"'<>]+`)
	dataKeyRe   = regexp.MustCompile(`[\s={}]`)
)

// redactedQueryParams are the URL query parameters whose values are replaced (names kept).
var redactedQueryParams = map[string]bool{
	"token": true, "code": true, "state": true, "key": true, "password": true,
	"secret": true, "signature": true, "sig": true, "passcode": true, "api_key": true,
}

// valueCap bounds one data value; the whole block is capped again when formatted.
const valueCap = 300

// maxDataFields bounds the number of pairs kept from one report.
const maxDataFields = 40

// Field is one data pair of a report: an allowlisted primitive (§12). Build it with F.
type Field struct {
	Key   string
	Value any
}

// F builds a data field. Only opaque ids, counts, booleans, route and verb names, file names,
// currency codes and error codes belong here; a ledger-named key is refused and a secret-named
// key is redacted whatever the value.
func F(key string, value any) Field {
	return Field{Key: key, Value: value}
}

// RedactURLs replaces the values of sensitive query parameters in every URL inside text.
func RedactURLs(text string) string {
	if !strings.Contains(text, "://") {
		return text
	}

	return urlRe.ReplaceAllStringFunc(text, redactOneURL)
}

func redactOneURL(raw string) string {
	q := strings.IndexByte(raw, '?')

	if q < 0 {
		return raw
	}

	rest := ""
	query := raw[q+1:]

	if h := strings.IndexByte(query, '#'); h >= 0 {
		rest = query[h:]
		query = query[:h]
	}

	pairs := strings.Split(query, "&")

	for i, pair := range pairs {
		eq := strings.IndexByte(pair, '=')

		if eq < 0 {
			continue
		}

		name := pair[:eq]
		decoded, err := url.QueryUnescape(name)

		if err != nil {
			decoded = name
		}

		if redactedQueryParams[strings.ToLower(decoded)] {
			pairs[i] = name + "=" + Redacted
		}
	}

	return raw[:q+1] + strings.Join(pairs, "&") + rest
}

// ── the statements root (§12.7) ───────────────────────────────────────────────────────────────

type statementsRootMatcher struct {
	root string
	re   *regexp.Regexp
}

var statementsRoot atomic.Pointer[statementsRootMatcher]

// SetStatementsRoot tells the library the private statements directory, so any absolute path
// under it is rewritten to <statements>/…/basename before it is written. An empty root clears it.
// It is learned from Options.StatementsRoot / EZBK_STATEMENTS_DIR at Install, or from the
// credentials file by the process that reads it — never from a constant.
func SetStatementsRoot(root string) {
	root = strings.TrimRight(strings.ReplaceAll(strings.TrimSpace(root), "\\", "/"), "/")

	if root == "" {
		statementsRoot.Store(nil)
		return
	}

	re, err := regexp.Compile(regexp.QuoteMeta(root) + `(?:/[^\s:'"<>]*|\b)`)

	if err != nil {
		statementsRoot.Store(nil)
		return
	}

	statementsRoot.Store(&statementsRootMatcher{root: root, re: re})
}

// RedactStatementsPath rewrites absolute paths under `root` inside text to <statements>/…/basename.
// The vectors test drives it with an explicit root; the library uses the installed one.
func RedactStatementsPath(text, root string) string {
	if root == "" {
		return text
	}

	prev := statementsRoot.Load()
	SetStatementsRoot(root)
	out := redactStatements(text)
	statementsRoot.Store(prev)

	return out
}

func redactStatements(text string) string {
	m := statementsRoot.Load()

	if m == nil || !strings.Contains(text, m.root) {
		return text
	}

	return m.re.ReplaceAllStringFunc(text, func(p string) string {
		if p == m.root || p == m.root+"/" {
			return "<statements>"
		}

		return "<statements>/…/" + path.Base(strings.ReplaceAll(p, "\\", "/"))
	})
}

// redactText applies the two text defences to a message: URL query values and the statements root.
func redactText(text string) string {
	return redactStatements(RedactURLs(text))
}

// ── data fields ───────────────────────────────────────────────────────────────────────────────

// RedactValue applies §12 to one key/value. Numbers and booleans pass through as they are; a
// string is redacted, control-stripped and capped; nil stays nil; a map, struct or slice becomes
// "[object]" / "[array]" — ledger rows hide in those, so their contents are never printed.
func RedactValue(key string, value any) any {
	if ledgerKeyRe.MatchString(key) {
		return LedgerRefused
	}

	if secretKeyRe.MatchString(key) {
		return Redacted
	}

	if value == nil {
		return nil
	}

	switch v := value.(type) {
	case string:
		return capMiddle(stripControlChars(redactText(v)), valueCap)
	case bool, int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64, float32, float64:
		return v
	case error:
		return capMiddle(stripControlChars(redactText(safeMessage(v))), valueCap)
	case fmt.Stringer:
		return capMiddle(stripControlChars(redactText(safeStringer(v))), valueCap)
	}

	rv := reflect.ValueOf(value)

	for rv.Kind() == reflect.Pointer || rv.Kind() == reflect.Interface {
		if rv.IsNil() {
			return nil
		}

		rv = rv.Elem()
	}

	switch rv.Kind() {
	case reflect.Slice, reflect.Array:
		return "[array]"
	case reflect.Map, reflect.Struct, reflect.Func, reflect.Chan:
		return "[object]"
	case reflect.String:
		return capMiddle(stripControlChars(redactText(rv.String())), valueCap)
	case reflect.Bool:
		return rv.Bool()
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return rv.Int()
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		return rv.Uint()
	case reflect.Float32, reflect.Float64:
		return rv.Float()
	}

	return "[" + rv.Kind().String() + "]"
}

func safeStringer(s fmt.Stringer) (out string) {
	defer func() {
		if r := recover(); r != nil {
			out = "[String() panicked]"
		}
	}()

	if isNilPointer(s) {
		return "<nil>"
	}

	return s.String()
}

// valueText is the formatted form of a redacted value.
func valueText(v any) string {
	switch t := v.(type) {
	case nil:
		return "null"
	case string:
		return t
	case bool:
		return strconv.FormatBool(t)
	case int:
		return strconv.Itoa(t)
	case int64:
		return strconv.FormatInt(t, 10)
	case uint64:
		return strconv.FormatUint(t, 10)
	case float64:
		return strconv.FormatFloat(t, 'f', -1, 64)
	case float32:
		return strconv.FormatFloat(float64(t), 'f', -1, 32)
	}

	return fmt.Sprint(v)
}

// redactFields turns call-site fields into the ordered, redacted pairs of a record. It never panics:
// a hostile value degrades to a placeholder.
func redactFields(fields []Field) (out []KV) {
	if len(fields) == 0 {
		return nil
	}

	defer func() {
		if r := recover(); r != nil {
			out = append(out, KV{Key: "data", Value: "[unreadable data]"})
		}
	}()

	for i, f := range fields {
		if i >= maxDataFields {
			break
		}

		key := dataKeyRe.ReplaceAllString(stripControlChars(f.Key), "_")

		if len(key) > 60 {
			key = key[:60]
		}

		if key == "" {
			continue
		}

		out = append(out, KV{Key: key, Value: valueText(RedactValue(key, f.Value))})
	}

	return out
}

// RedactData applies §12 to a map that arrived from outside the process (the ingest route re-applies
// it to a browser's data and trusts none of it). Keys come out sorted so the line is stable.
func RedactData(data map[string]any) []KV {
	if len(data) == 0 {
		return nil
	}

	fields := make([]Field, 0, len(data))

	for k, v := range data {
		fields = append(fields, Field{Key: k, Value: v})
	}

	sortFields(fields)

	return redactFields(fields)
}

func sortFields(fields []Field) {
	for i := 1; i < len(fields); i++ {
		for j := i; j > 0 && fields[j].Key < fields[j-1].Key; j-- {
			fields[j], fields[j-1] = fields[j-1], fields[j]
		}
	}
}
