package app

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/BryanStarbuck/ezbookkeeping/cli/internal/errfile"
	"github.com/BryanStarbuck/ezbookkeeping/cli/internal/exitcode"
	"github.com/BryanStarbuck/ezbookkeeping/cli/internal/logger"
	"github.com/BryanStarbuck/ezbookkeeping/cli/internal/render"
)

// BareVerb is what `ezbk` with no arguments runs (registered by the orientation family)
var BareVerb *Verb

// Main runs the CLI and returns the exit code
func Main(argv []string, stdout, stderr io.Writer) (code int) {
	jsonErrors := false

	for _, a := range argv {
		if a == "--json-errors" {
			jsonErrors = true
		}
	}

	verbName := ""

	defer func() {
		if r := recover(); r != nil {
			// pm/error_err.mdx §7 G5: the record and the stack go to error.err; cli.err keeps the
			// usage trail
			errfile.Recovered("running ezbk", r, errfile.F("verb", verbName))
			errfile.Flush()
			logger.Error("panic: %v", r)
			printError(stderr, &ExitError{Code: exitcode.Failed, Msg: fmt.Sprintf("internal error: %v", r), Hint: "the stack is in ~/T/ezbookkeeping/error.err"}, jsonErrors)
			code = exitcode.Failed
		}
	}()

	argv = HoistLeadingFlags(argv)

	// bare `ezbk` (possibly with universal flags only, e.g. `ezbk --format json`)
	if len(argv) == 0 || (strings.HasPrefix(argv[0], "-") && argv[0] != "--help" && argv[0] != "-h") {
		if BareVerb != nil {
			p, err := ParseFor(BareVerb, argv)

			if err != nil {
				msg, hint := err.Error(), ""
				var ue *UsageError

				if errors.As(err, &ue) {
					hint = ue.Hint
				}

				printError(stderr, &ExitError{Code: exitcode.Usage, Msg: msg, Hint: hint}, jsonErrors)

				return exitcode.Usage
			}

			return runVerb(p, stdout, stderr, jsonErrors)
		}
	}

	if argv[0] == "help" || argv[0] == "--help" || argv[0] == "-h" {
		if len(argv) > 1 {
			if v, _ := Lookup(argv[1:]); v != nil {
				PrintVerbHelp(stdout, v)
				return exitcode.OK
			}

			PrintHelp(stderr)
			printError(stderr, &ExitError{Code: exitcode.Usage, Msg: "unknown command " + strings.Join(argv[1:], " ")}, jsonErrors)

			return exitcode.Usage
		}

		PrintHelp(stdout)

		return exitcode.OK
	}

	p, err := Parse(argv)

	if p != nil && p.Verb != nil {
		verbName = p.Verb.Name
	}

	if err != nil {
		var ue *UsageError

		if errors.As(err, &ue) {
			if strings.HasPrefix(ue.Msg, "unknown command") {
				PrintHelp(stderr)
			}

			printError(stderr, &ExitError{Code: exitcode.Usage, Msg: ue.Msg, Hint: ue.Hint}, jsonErrors)

			return exitcode.Usage
		}

		printError(stderr, &ExitError{Code: exitcode.Usage, Msg: err.Error()}, jsonErrors)

		return exitcode.Usage
	}

	if p.Verb == nil {
		PrintHelp(stdout)
		return exitcode.OK
	}

	if p.Bools["help"] {
		PrintVerbHelp(stdout, p.Verb)
		return exitcode.OK
	}

	return runVerb(p, stdout, stderr, jsonErrors)
}

func runVerb(p *Parsed, stdout, stderr io.Writer, jsonErrors bool) int {
	rawFormat := firstOr(p.Values["format"], "")

	// --format tsv is the export verbs' spelling (cli.mdx §9: `export --format csv|tsv`); those
	// verbs carry a --tsv switch, so it maps onto that and the CSV renderer
	if strings.EqualFold(strings.TrimSpace(rawFormat), "tsv") {
		if !verbHasFlag(p.Verb, "tsv") {
			printError(stderr, &ExitError{Code: exitcode.Usage, Msg: "--format tsv is only for exports (transactions export, data export); use json, table or csv"}, jsonErrors)
			return exitcode.Usage
		}

		if p.Bools == nil {
			p.Bools = map[string]bool{}
		}

		p.Bools["tsv"] = true
		rawFormat = "csv"
	}

	format, ok := render.ParseFormat(rawFormat)

	if !ok {
		printError(stderr, &ExitError{Code: exitcode.Usage, Msg: "--format must be json, table or csv"}, jsonErrors)
		return exitcode.Usage
	}

	c := &Ctx{Verb: p.Verb, Args: p.Args, values: p.Values, bools: p.Bools, Out: stdout, Err: stderr, Format: format}
	logger.Info("verb=%q args=%d format=%s", p.Verb.Name, len(p.Args), format)

	if err := p.Verb.Run(c); err != nil {
		var ee *ExitError

		if !errors.As(err, &ee) {
			ee = &ExitError{Code: exitcode.Failed, Msg: err.Error()}
		}

		logger.Error("verb=%q exit=%d %s", p.Verb.Name, ee.Code, ee.Msg)
		printError(stderr, ee, jsonErrors)

		return ee.Code
	}

	return exitcode.OK
}

func firstOr(v []string, d string) string {
	if len(v) > 0 {
		return v[len(v)-1]
	}

	return d
}

func printError(w io.Writer, e *ExitError, jsonErrors bool) {
	if jsonErrors {
		data, _ := json.Marshal(map[string]any{"exit": e.Code, "error": e.Msg, "hint": e.Hint, "code": e.APICode})
		fmt.Fprintln(w, string(data))

		return
	}

	fmt.Fprintf(w, "ezbk: %s\n", e.Msg)

	if e.Hint != "" {
		fmt.Fprintf(w, "  %s\n", e.Hint)
	}

	if e.LogTail != "" {
		fmt.Fprintf(w, "  --- last lines of the server log ---\n%s\n", indent(e.LogTail))
	}
}

func indent(s string) string {
	lines := strings.Split(s, "\n")

	for i, l := range lines {
		lines[i] = "    " + l
	}

	return strings.Join(lines, "\n")
}

// PrintHelp prints the verb catalogue
func PrintHelp(w io.Writer) {
	fmt.Fprintln(w, "ezbk — the terminal front door to this ezBookkeeping install (pm/cli.mdx)")
	fmt.Fprintln(w, "")
	fmt.Fprintln(w, "usage: ezbk <command> [args] [flags]      ezbk help <command>")

	group := ""

	for _, v := range All() {
		if v.Group != group {
			group = v.Group
			fmt.Fprintf(w, "\n%s\n", group)
		}

		name := v.Name

		if v.Args != "" {
			name += " " + v.Args
		}

		fmt.Fprintf(w, "  %-46s %s\n", name, v.Summary)
	}

	fmt.Fprintln(w, "\nUNIVERSAL FLAGS")

	for _, f := range UniversalFlags {
		fmt.Fprintf(w, "  %-46s %s\n", flagLabel(f), f.Help)
	}

	fmt.Fprintln(w, "\nAmounts are integer hundredths (1250 = 12.50). Dates are YYYY-MM-DD. Writes are dry runs without --write.")
}

// PrintVerbHelp prints one verb's declared flags — generated from the same declaration the parser uses
func PrintVerbHelp(w io.Writer, v *Verb) {
	usage := "ezbk " + v.Name

	if v.Args != "" {
		usage += " " + v.Args
	}

	fmt.Fprintf(w, "usage: %s [flags]\n\n%s\n", usage, v.Summary)

	if len(v.Flags) > 0 {
		fmt.Fprintln(w, "\nFLAGS")

		for _, f := range v.Flags {
			label := flagLabel(f)

			if f.Repeat {
				label += " (repeatable)"
			}

			fmt.Fprintf(w, "  %-40s %s\n", label, f.Help)
		}
	}

	fmt.Fprintln(w, "\nUNIVERSAL FLAGS: --format --write --yes --max-changes --tz --api --timeout --no-bringup --quiet --verbose --json-errors")
}

func flagLabel(f Flag) string {
	if f.Value == "" {
		return "--" + f.Name
	}

	return "--" + f.Name + " <" + f.Value + ">"
}

// Exit is a convenience for main()
func Exit(code int) {
	os.Exit(code)
}

func verbHasFlag(v *Verb, name string) bool {
	for _, f := range v.Flags {
		if f.Name == name {
			return true
		}
	}

	return false
}
