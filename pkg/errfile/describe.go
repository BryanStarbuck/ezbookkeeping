package errfile

import (
	"errors"
	"fmt"
	"net"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"unicode/utf8"
)

// The caps of §3.2 (mirrored in testdata/vectors.json "caps").
const (
	// MessageCap is the longest message kept, cut in the middle.
	MessageCap = 2000
	// DataCap is the longest formatted data block.
	DataCap = 1000
	// RecordCap is the longest whole record (header plus stack).
	RecordCap = 8000
	// CauseDepth is how far the Unwrap chain is walked.
	CauseDepth = 5
	// JoinMembers is how many members of an errors.Join are shown.
	JoinMembers = 3
	// StackFrames is how many trimmed frames are kept.
	StackFrames = 12
)

// KV is one key=value pair of a code suffix or a data block, already redacted.
type KV struct {
	Key   string
	Value string
}

// stripControlChars replaces \x00-\x1f, \x7f, U+2028 and U+2029 with a space, so a field can never
// end a line or forge a second header (§3.2). Every other rune passes through unchanged.
func stripControlChars(s string) string {
	clean := true

	for _, r := range s {
		if isControl(r) {
			clean = false
			break
		}
	}

	if clean {
		return s
	}

	var b strings.Builder
	b.Grow(len(s))

	for _, r := range s {
		if isControl(r) {
			b.WriteByte(' ')
		} else {
			b.WriteRune(r)
		}
	}

	return b.String()
}

func isControl(r rune) bool {
	return r < 0x20 || r == 0x7f || r == 0x2028 || r == 0x2029
}

// capMiddle keeps a string within max characters by cutting out its middle, so the start and the
// end both survive. The head keeps max/2 characters, the tail max/2-10, and the marker says how
// many were cut (" …(416 chars cut)… "). Characters are runes, never split mid-sequence.
func capMiddle(s string, max int) string {
	if len(s) <= max {
		return s
	}

	n := utf8.RuneCountInString(s)

	if n <= max {
		return s
	}

	head := max / 2
	tail := max/2 - 10

	if tail < 0 {
		tail = 0
	}

	runes := []rune(s)
	cut := n - head - tail

	return string(runes[:head]) + " …(" + strconv.Itoa(cut) + " chars cut)… " + string(runes[n-tail:])
}

// Headline renders one link of an error as "Type: message (k=v …)", control-stripped and capped.
// The vectors test drives it directly; describeError builds the same text from a live error.
func Headline(typ, message string, codes []KV) string {
	text := stripControlChars(typ)

	if message != "" {
		text += ": " + capMiddle(stripControlChars(message), MessageCap)
	}

	return text + codeSuffix(codes)
}

func codeSuffix(codes []KV) string {
	if len(codes) == 0 {
		return ""
	}

	parts := make([]string, 0, len(codes))

	for _, kv := range codes {
		parts = append(parts, stripControlChars(kv.Key)+"="+stripControlChars(kv.Value))
	}

	return " (" + strings.Join(parts, " ") + ")"
}

// described is the text form of an error: its headline, its cause chain, and the parts the fold
// key and the transient-network check need.
type described struct {
	typ      string
	message  string
	headline string
	cause    string
	codes    []KV
}

// typeName is the dynamic type of an error, as %T prints it: *fs.PathError, syscall.Errno,
// *errs.Error, *errors.errorString.
func typeName(err error) string {
	return fmt.Sprintf("%T", err)
}

// safeMessage is err.Error(), but total: a panicking Error() becomes text, never a panic.
func safeMessage(err error) (msg string) {
	defer func() {
		if r := recover(); r != nil {
			msg = "[Error() panicked: " + fmt.Sprint(r) + "]"
		}
	}()

	if err == nil {
		return ""
	}

	if isNilPointer(err) {
		return "<nil>"
	}

	return err.Error()
}

func isNilPointer(v0 any) bool {
	v := reflect.ValueOf(v0)

	return v.Kind() == reflect.Pointer && v.IsNil()
}

// describeError walks the error: headline, then " | cause: …" per Unwrap link, up to CauseDepth.
// An errors.Join shows its first JoinMembers members as causes. Every string is redacted for URL
// query values and the statements root before it is returned.
func describeError(err error) described {
	d := described{}

	if err == nil {
		return d
	}

	d.typ = typeName(err)
	d.message = redactText(safeMessage(err))
	d.codes = codesOf(err)
	d.headline = Headline(d.typ, d.message, d.codes)

	seen := map[uintptr]bool{}
	remember(seen, err)
	depth := 0
	var b strings.Builder

	for _, link := range nextLinks(err) {
		walkCause(&b, link, seen, &depth)
	}

	d.cause = b.String()

	return d
}

func walkCause(b *strings.Builder, err error, seen map[uintptr]bool, depth *int) {
	if err == nil || *depth >= CauseDepth || isNilPointer(err) {
		return
	}

	if !remember(seen, err) {
		return // a cycle
	}

	*depth++
	b.WriteString(" | cause: ")
	b.WriteString(Headline(typeName(err), redactText(safeMessage(err)), codesOf(err)))

	for _, link := range nextLinks(err) {
		walkCause(b, link, seen, depth)
	}
}

// remember records a pointer-identified error in `seen`; it returns false when it was seen before.
// Non-pointer errors cannot form cycles, so they are always new.
func remember(seen map[uintptr]bool, err error) bool {
	v := reflect.ValueOf(err)

	if v.Kind() != reflect.Pointer || v.IsNil() {
		return true
	}

	p := v.Pointer()

	if seen[p] {
		return false
	}

	seen[p] = true

	return true
}

// nextLinks returns the causes one level down: Unwrap() error, or the first JoinMembers of
// Unwrap() []error.
func nextLinks(err error) (links []error) {
	defer func() {
		if r := recover(); r != nil {
			links = nil
		}
	}()

	switch u := err.(type) {
	case interface{ Unwrap() error }:
		if next := u.Unwrap(); next != nil {
			return []error{next}
		}
	case interface{ Unwrap() []error }:
		all := u.Unwrap()

		if len(all) > JoinMembers {
			all = all[:JoinMembers]
		}

		return all
	}

	return nil
}

// codesOf collects the code-like values of ONE link: a Code() method, exported fields named Code,
// Errno, Op, Syscall, HttpStatusCode or ErrorCode, and the symbolic name of a syscall.Errno.
func codesOf(err error) (codes []KV) {
	defer func() {
		if r := recover(); r != nil {
			codes = nil
		}
	}()

	if errno, ok := err.(syscall.Errno); ok {
		return []KV{{Key: "errno", Value: errnoName(errno)}}
	}

	if dns, ok := err.(*net.DNSError); ok {
		switch {
		case dns.IsNotFound:
			return []KV{{Key: "code", Value: "ENOTFOUND"}}
		case dns.IsTimeout:
			return []KV{{Key: "code", Value: "ETIMEDOUT"}}
		case dns.IsTemporary:
			return []KV{{Key: "code", Value: "EAI_AGAIN"}}
		}

		return nil
	}

	switch c := err.(type) {
	case interface{ Code() int32 }:
		codes = append(codes, KV{Key: "code", Value: strconv.FormatInt(int64(c.Code()), 10)})
	case interface{ Code() int }:
		codes = append(codes, KV{Key: "code", Value: strconv.Itoa(c.Code())})
	case interface{ Code() string }:
		if c.Code() != "" {
			codes = append(codes, KV{Key: "code", Value: c.Code()})
		}
	}

	v := reflect.ValueOf(err)

	for v.Kind() == reflect.Pointer || v.Kind() == reflect.Interface {
		if v.IsNil() {
			return codes
		}

		v = v.Elem()
	}

	if v.Kind() != reflect.Struct {
		return codes
	}

	t := v.Type()

	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)

		if !f.IsExported() {
			continue
		}

		key := ""

		switch strings.ToLower(f.Name) {
		case "code":
			key = "code"
		case "errno":
			key = "errno"
		case "op":
			key = "op"
		case "syscall":
			key = "syscall"
		case "httpstatuscode", "statuscode":
			key = "status"
		case "errorcode":
			key = "error_code"
		default:
			continue
		}

		if key == "code" && len(codes) > 0 && codes[0].Key == "code" {
			continue
		}

		fv := v.Field(i)
		val := ""

		switch fv.Kind() {
		case reflect.String:
			val = fv.String()
		case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
			if fv.Int() != 0 {
				val = strconv.FormatInt(fv.Int(), 10)
			}
		case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
			if fv.Uint() != 0 {
				val = strconv.FormatUint(fv.Uint(), 10)
			}
		}

		if val != "" {
			codes = append(codes, KV{Key: key, Value: val})
		}
	}

	return codes
}

var errnoNames = map[syscall.Errno]string{
	syscall.EACCES:       "EACCES",
	syscall.EADDRINUSE:   "EADDRINUSE",
	syscall.EAGAIN:       "EAGAIN",
	syscall.ECONNABORTED: "ECONNABORTED",
	syscall.ECONNREFUSED: "ECONNREFUSED",
	syscall.ECONNRESET:   "ECONNRESET",
	syscall.EEXIST:       "EEXIST",
	syscall.EHOSTUNREACH: "EHOSTUNREACH",
	syscall.EINVAL:       "EINVAL",
	syscall.EIO:          "EIO",
	syscall.EISDIR:       "EISDIR",
	syscall.EMFILE:       "EMFILE",
	syscall.ENETUNREACH:  "ENETUNREACH",
	syscall.ENOENT:       "ENOENT",
	syscall.ENOSPC:       "ENOSPC",
	syscall.ENOTDIR:      "ENOTDIR",
	syscall.ENOTEMPTY:    "ENOTEMPTY",
	syscall.EPERM:        "EPERM",
	syscall.EPIPE:        "EPIPE",
	syscall.ETIMEDOUT:    "ETIMEDOUT",
}

func errnoName(e syscall.Errno) string {
	if name, ok := errnoNames[e]; ok {
		return name
	}

	return "errno" + strconv.Itoa(int(e))
}

// transientMarkers are the codes and message fragments of §11.2 (vectors "transient_network_markers").
var transientMarkers = []string{
	"ECONNREFUSED", "ENOTFOUND", "EAI_AGAIN", "ETIMEDOUT", "ECONNRESET", "ERR_NETWORK", "ECONNABORTED",
	"Network Error", "network unreachable",
}

var transientErrnos = map[syscall.Errno]bool{
	syscall.ECONNREFUSED: true, syscall.ECONNRESET: true, syscall.ETIMEDOUT: true,
	syscall.ECONNABORTED: true, syscall.EHOSTUNREACH: true, syscall.ENETUNREACH: true,
}

// isTransientNetwork reports whether an error (or anything it wraps, three links deep) is the
// "laptop lid closed" kind: a refused/reset/timed-out connection, a DNS miss, or a message that
// carries one of the markers. Such faults are WARN and fold per 10 minutes (§11.2).
func isTransientNetwork(err error) (transient bool) {
	defer func() {
		if r := recover(); r != nil {
			transient = false
		}
	}()

	current := err

	for depth := 0; depth < 3 && current != nil; depth++ {
		var errno syscall.Errno

		if errors.As(current, &errno) && transientErrnos[errno] {
			return true
		}

		var dns *net.DNSError

		if errors.As(current, &dns) && (dns.IsNotFound || dns.IsTemporary || dns.IsTimeout) {
			return true
		}

		var op *net.OpError

		if errors.As(current, &op) && op.Timeout() {
			return true
		}

		msg := safeMessage(current)

		for _, m := range transientMarkers {
			if strings.Contains(msg, m) {
				return true
			}
		}

		links := nextLinks(current)

		if len(links) == 0 {
			break
		}

		current = links[0]
	}

	return false
}

// ── stacks and source positions ────────────────────────────────────────────────────────────────

// ownFrames are function-name fragments whose frames are dropped from every stack and never chosen
// as `where` (§3.2): the runtime, this library (and its vendored copy), logrus and gin's dispatch.
var ownFrames = []string{
	"runtime.",
	"/pkg/errfile.",
	"/pkg/errfile/",
	"/cli/internal/errfile.",
	"/cli/internal/errfile/",
	"github.com/sirupsen/logrus.",
	"github.com/gin-gonic/gin.",
}

func isOwnFrame(function, file string, extra []string) bool {
	if strings.HasPrefix(function, "runtime.") {
		return true
	}

	// The library's own test files report like any caller.
	if !strings.HasSuffix(file, "_test.go") {
		for _, m := range ownFrames[1:] {
			if strings.Contains(function, m) {
				return true
			}
		}
	}

	for _, m := range extra {
		if m != "" && strings.HasPrefix(function, m) {
			return true
		}
	}

	return false
}

// frame is one captured stack frame after trimming.
type frame struct {
	function string // short: (*ingestApply).writePlan
	file     string // repo-relative
	line     int
}

func (f frame) String() string {
	return f.function + " (" + f.file + ":" + strconv.Itoa(f.line) + ")"
}

// captureFrames reads the goroutine's stack from `skip` frames above the caller and trims it:
// own frames (plus `extra` prefixes) are dropped; when `afterPanic` is set, everything up to and
// including runtime.gopanic is dropped too, so a panic site is the first frame. At most
// StackFrames frames are kept.
func captureFrames(skip int, extra []string, afterPanic bool) []frame {
	pcs := make([]uintptr, 64)
	n := runtime.Callers(skip+2, pcs)

	if n == 0 {
		return nil
	}

	iter := runtime.CallersFrames(pcs[:n])
	var out []frame
	dropping := afterPanic

	for {
		fr, more := iter.Next()

		if dropping {
			if strings.HasPrefix(fr.Function, "runtime.gopanic") || strings.HasPrefix(fr.Function, "runtime.panic") {
				dropping = false
			}

			if !more {
				break
			}

			continue
		}

		if fr.Function != "" && !isOwnFrame(fr.Function, fr.File, extra) {
			out = append(out, frame{function: shortFunction(fr.Function), file: relPath(fr.File), line: fr.Line})

			if len(out) >= StackFrames {
				break
			}
		}

		if !more {
			break
		}
	}

	return out
}

// shortFunction strips the import path and package name: "…/pkg/machine.(*ingestApply).writePlan"
// → "(*ingestApply).writePlan"; "main.main" → "main".
func shortFunction(fn string) string {
	if i := strings.LastIndex(fn, "/"); i >= 0 {
		fn = fn[i+1:]
	}

	if i := strings.Index(fn, "."); i >= 0 {
		fn = fn[i+1:]
	}

	return fn
}

// relPath makes a source path repo-relative (R14): the module path
// (github.com/mayswind/ezbookkeeping/ or …/ezbookkeeping/cli/ → cli/) and any absolute prefix up to
// and including /ezbookkeeping/ are stripped. Module-cache paths keep their module part.
func relPath(file string) string {
	file = strings.ReplaceAll(file, "\\", "/")

	if i := strings.LastIndex(file, "/ezbookkeeping/"); i >= 0 {
		return file[i+len("/ezbookkeeping/"):]
	}

	if strings.HasPrefix(file, "github.com/mayswind/ezbookkeeping/") {
		return strings.TrimPrefix(file, "github.com/mayswind/ezbookkeeping/")
	}

	if i := strings.Index(file, "/pkg/mod/"); i >= 0 {
		return file[i+len("/pkg/mod/"):]
	}

	if i := strings.Index(file, "/go/src/"); i >= 0 {
		return file[i+len("/go/src/"):]
	}

	for _, marker := range []string{"/pkg/", "/cmd/", "/cli/", "/scripts/"} {
		if i := strings.Index(file, marker); i >= 0 {
			return file[i+1:]
		}
	}

	return file
}

// whereFrom is the `where` of a report: the first kept frame as file.go:line.
func whereFrom(frames []frame) string {
	if len(frames) == 0 {
		return "?"
	}

	return frames[0].file + ":" + strconv.Itoa(frames[0].line)
}

// whereFile is `where` without its :line, for the fold key (R10: a loop reports from one line).
func whereFile(where string) string {
	if i := strings.LastIndex(where, ":"); i > 0 {
		if _, err := strconv.Atoi(where[i+1:]); err == nil {
			return where[:i]
		}
	}

	return where
}

// CallerWhere returns the repo-relative file:line of the nearest caller whose function is outside
// this library and outside the given function-name prefixes. The logrus hook uses it to point at
// the caller of log.Errorf rather than at pkg/log.
func CallerWhere(skipPrefixes ...string) string {
	return whereFrom(captureFrames(1, skipPrefixes, false))
}

// Describe renders an error as "Type: message (code=…) | cause: …" with no stack, for code that has
// to SHOW a message (a CLI line, an envelope). It must never be used to build a report by hand.
func Describe(err error) string {
	if err == nil {
		return ""
	}

	d := describeError(err)

	return d.headline + d.cause
}
