package app

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/BryanStarbuck/ezbookkeeping/cli/internal/bringup"
	"github.com/BryanStarbuck/ezbookkeeping/cli/internal/client"
	"github.com/BryanStarbuck/ezbookkeeping/cli/internal/credentials"
	"github.com/BryanStarbuck/ezbookkeeping/cli/internal/exitcode"
	"github.com/BryanStarbuck/ezbookkeeping/cli/internal/logger"
	"github.com/BryanStarbuck/ezbookkeeping/cli/internal/progress"
	"github.com/BryanStarbuck/ezbookkeeping/cli/internal/render"
)

// Ctx is what every verb runs with
type Ctx struct {
	Verb   *Verb
	Args   []string
	values map[string][]string
	bools  map[string]bool

	// Out is stdout — the ONE payload. Err is stderr — everything else (cli.mdx §14.1).
	Out io.Writer
	Err io.Writer

	Format render.Format
	client *client.Client
	loc    *time.Location
}

// ExitError carries an exit code, a message and a hint to Main
type ExitError struct {
	Code    int
	Msg     string
	Hint    string
	LogTail string
	// APICode is the plane's error code, when the failure came from the plane
	APICode string
	Details any
}

func (e *ExitError) Error() string { return e.Msg }

// Fail builds an ExitError
func Fail(code int, msg, hint string) error {
	return &ExitError{Code: code, Msg: msg, Hint: hint}
}

// Usage builds an exit-2 error
func Usage(msg, hint string) error {
	return &ExitError{Code: exitcode.Usage, Msg: msg, Hint: hint}
}

// String returns a value flag ("" when absent)
func (c *Ctx) String(name string) string {
	if v := c.values[name]; len(v) > 0 {
		return v[len(v)-1]
	}

	return ""
}

// Strings returns every value of a repeatable flag, splitting commas
func (c *Ctx) Strings(name string) []string {
	var out []string

	for _, v := range c.values[name] {
		for _, p := range strings.Split(v, ",") {
			if p = strings.TrimSpace(p); p != "" {
				out = append(out, p)
			}
		}
	}

	return out
}

// Has reports whether a value flag was given
func (c *Ctx) Has(name string) bool {
	return len(c.values[name]) > 0
}

// Bool returns a boolean flag
func (c *Ctx) Bool(name string) bool {
	return c.bools[name]
}

// Int returns an integer flag or def
func (c *Ctx) Int(name string, def int) (int, error) {
	v := c.String(name)

	if v == "" {
		return def, nil
	}

	n, err := strconv.Atoi(v)

	if err != nil {
		return 0, Usage(fmt.Sprintf("--%s must be a whole number, got %q", name, v), "ezbk help "+c.Verb.Name)
	}

	return n, nil
}

// Amount returns an integer-of-hundredths flag; a decimal point is refused with a suggestion
func (c *Ctx) Amount(name string) (int64, bool, error) {
	v := c.String(name)

	if v == "" {
		return 0, false, nil
	}

	n, msg := render.ParseHundredthsArg("--"+name, v)

	if msg != "" {
		return 0, true, Usage(msg, "")
	}

	return n, true, nil
}

// Quiet / Verbose
func (c *Ctx) Quiet() bool   { return c.bools["quiet"] }
func (c *Ctx) Verbose() bool { return c.bools["verbose"] && !c.bools["quiet"] }

// Info prints an informational stderr line (suppressed by --quiet)
func (c *Ctx) Info(format string, args ...any) {
	if !c.Quiet() {
		fmt.Fprintf(c.Err, format+"\n", args...)
	}
}

// Warnf prints a warning to stderr and cli.err
func (c *Ctx) Warnf(format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	logger.Warn("%s", msg)
	fmt.Fprintln(c.Err, "warning: "+msg)
}

// Location is the timezone for dates in this call: --tz, then the credentials file, then TZ /
// the system zone (cli.mdx §7.5)
func (c *Ctx) Location() *time.Location {
	if c.loc != nil {
		return c.loc
	}

	candidates := []string{c.String("tz")}

	if creds, err := credentials.Read(); err == nil {
		candidates = append(candidates, creds.Timezone)
	}

	candidates = append(candidates, strings.TrimPrefix(os.Getenv("TZ"), ":"))

	if target, err := os.Readlink("/etc/localtime"); err == nil {
		if i := strings.Index(target, "zoneinfo/"); i >= 0 {
			candidates = append(candidates, target[i+len("zoneinfo/"):])
		}
	}

	for _, name := range candidates {
		if name == "" {
			continue
		}

		if loc, err := time.LoadLocation(name); err == nil {
			c.loc = loc
			return loc
		}
	}

	c.loc = time.Local

	return c.loc
}

// BaseURL is the resolved target (cli.mdx §7.2)
func (c *Ctx) BaseURL() string {
	if v := c.String("api"); v != "" {
		return strings.TrimRight(v, "/")
	}

	if v := strings.TrimSpace(os.Getenv("EZBK_API_URL")); v != "" {
		return strings.TrimRight(v, "/")
	}

	return bringup.LocalURL()
}

// Client resolves the key (never minting silently — R6), brings the app up if needed, and returns
// a ready client. It checks /machine/v1/ping so a 401 or a 404 (old binary) is diagnosed precisely.
func (c *Ctx) Client() (*client.Client, error) {
	if c.client != nil {
		return c.client, nil
	}

	base := c.BaseURL()

	if err := client.CheckTarget(base); err != nil {
		return nil, Usage(err.Error(), "use --api http://127.0.0.1:<port> or an https URL")
	}

	key, source, err := credentials.Resolve()

	if err != nil {
		if errors.Is(err, credentials.ErrTooPermissive) {
			return nil, Usage(err.Error(), "")
		}

		return nil, Usage(err.Error(), "ezbk key show")
	}

	local := client.IsLoopbackURL(base)
	isDefaultLocal := local && base == bringup.LocalURL()

	if !bringup.Healthy(base) {
		if c.Bool("no-bringup") || c.Verb.NoBringup || !isDefaultLocal {
			return nil, &ExitError{Code: exitcode.Unreachable, Msg: "ezBookkeeping is not running at " + base, Hint: "ezbk up"}
		}

		if err := bringup.Up(bringup.Options{Quiet: c.Quiet()}); err != nil {
			var be *bringup.Error

			if errors.As(err, &be) {
				return nil, &ExitError{Code: exitcode.Unreachable, Msg: be.Msg, Hint: be.Hint, LogTail: be.LogTail}
			}

			return nil, &ExitError{Code: exitcode.Unreachable, Msg: err.Error(), Hint: "ezbk logs"}
		}

		// the server mints the key on its first boot; read it again
		if key == "" {
			key, source, _ = credentials.Resolve()
		}
	}

	if key == "" {
		path, _ := credentials.Path()

		return nil, &ExitError{
			Code: exitcode.Usage,
			Msg:  "refused: no API secret key for " + base,
			Hint: "Looked in: EZBK_API_KEY, EZBK_API_KEY_FILE,\n             " + path + "  (ezbookkeeping.machine.api_key)\n  The app mints this on first boot. Start it and try again:\n    ezbk up\n  Or mint it here without starting anything:\n    ezbk key init",
		}
	}

	timeout := 30 * time.Second

	if ms, err := c.Int("timeout", 0); err == nil && ms > 0 {
		timeout = time.Duration(ms) * time.Millisecond
	}

	if c.Verb.LongRunning {
		if c.Has("timeout") {
			c.Info("note: --timeout is ignored by `ezbk %s` (it can legitimately run for minutes)", c.Verb.Name)
		}

		timeout = 0
	}

	cl := &client.Client{BaseURL: base, Key: key, Timezone: c.Location().String(), Timeout: timeout}

	if c.Verbose() {
		fmt.Fprintf(c.Err, "target %s  key %s (from %s)  tz %s\n", base, credentials.Fingerprint(key), source, cl.Timezone)
	}

	env, err := cl.Do("GET", "/ping", nil, nil)

	if err != nil {
		var ae *client.APIError

		if errors.As(err, &ae) {
			switch ae.Status {
			case 401:
				return nil, &ExitError{Code: exitcode.Unauthorized, Msg: "the server at " + base + " holds a different API secret key than " + source, Hint: "it was probably started before a key rotation; restart it: ezbk stop && ezbk up"}
			case 404:
				return nil, &ExitError{Code: exitcode.Unreachable, Msg: "the server at " + base + " has no machine plane (/machine/v1 answered 404)", Hint: "rebuild and restart: just build && ezbk stop && ezbk up"}
			}
		}

		return nil, &ExitError{Code: exitcode.Unreachable, Msg: "the machine plane at " + base + " could not be reached: " + err.Error(), Hint: "ezbk status"}
	}

	_ = env
	c.client = cl

	return cl, nil
}

// Call makes one machine-plane call, logging it, and maps a plane error to an ExitError
func (c *Ctx) Call(method, path string, query url.Values, body any) (*client.Envelope, error) {
	cl, err := c.Client()

	if err != nil {
		return nil, err
	}

	started := time.Now()
	env, err := cl.Do(method, path, query, body)

	return c.finishCall(method, path, started, env, err)
}

// CallStream is Call for the long routes that stream NDJSON progress
func (c *Ctx) CallStream(method, path string, query url.Values, body any, phase string) (*client.Envelope, error) {
	cl, err := c.Client()

	if err != nil {
		return nil, err
	}

	sp := progress.New(c.Quiet())
	sp.Start(phase)
	started := time.Now()

	env, err := cl.DoStream(method, path, query, body, func(m map[string]any) {
		label, _ := m["phase"].(string)

		if label == "" {
			label, _ = m["stage"].(string)
		}

		count := ""

		if done, ok := render.ToInt64(m["done"]); ok {
			if total, ok := render.ToInt64(m["total"]); ok && total > 0 {
				count = fmt.Sprintf("%d/%d", done, total)
			} else {
				count = fmt.Sprintf("%d", done)
			}
		}

		sp.Tick(label, count)
	})

	sp.Stop()

	return c.finishCall(method, path, started, env, err)
}

func (c *Ctx) finishCall(method, path string, started time.Time, env *client.Envelope, err error) (*client.Envelope, error) {
	route := path

	if i := strings.Index(route, "?"); i >= 0 {
		route = route[:i]
	}

	if err != nil {
		var ae *client.APIError

		if errors.As(err, &ae) {
			logger.APICall(method, route, time.Since(started), ae.Code)

			return env, &ExitError{Code: ae.ExitCode(), Msg: ae.Error(), Hint: ae.Hint, APICode: ae.Code, Details: ae.Details}
		}

		logger.APICall(method, route, time.Since(started), "unreachable")

		return nil, &ExitError{Code: exitcode.Unreachable, Msg: err.Error(), Hint: "ezbk status"}
	}

	logger.APICall(method, route, time.Since(started), "ok")

	return env, nil
}

// Emit prints an envelope in the chosen format. For table/CSV, view lays the rows out.
func (c *Ctx) Emit(env *client.Envelope, view render.View) error {
	if env == nil {
		return nil
	}

	if len(env.Raw) > 0 && len(env.Data) == 0 && env.Meta == nil {
		_, err := c.Out.Write(env.Raw)
		return err
	}

	switch c.Format {
	case render.FormatJSON:
		return render.PrettyJSON(c.Out, env.Raw)
	}

	var data any

	if err := env.Decode(&data); err != nil {
		return err
	}

	rows := render.Rows(data, view)

	if len(rows) == 0 && view.Empty != "" {
		c.Info("%s", view.Empty)
	}

	if len(view.Columns) == 0 {
		if m, ok := data.(map[string]any); ok && c.Format == render.FormatTable {
			return render.KeyValues(c.Out, m, nil)
		}

		return render.PrettyJSON(c.Out, env.Data)
	}

	c.printProvenance(env)

	if c.Format == render.FormatCSV {
		return render.CSV(c.Out, rows, view.Columns)
	}

	return render.Table(c.Out, rows, view.Columns)
}

// printProvenance puts the answer's scope on stderr for non-JSON output: timezone, truncation
func (c *Ctx) printProvenance(env *client.Envelope) {
	if c.Quiet() || env.Meta == nil {
		return
	}

	if t, ok := env.Meta["truncated"].(bool); ok && t {
		fmt.Fprintf(c.Err, "note: truncated at %v rows (more exist; narrow the filter or fetch the next page)\n", env.Meta["limitApplied"])
	}
}

// EmitJSONValue prints any Go value as JSON (for verbs that compose their own answer)
func (c *Ctx) EmitJSONValue(v any) error {
	data, err := json.MarshalIndent(v, "", "  ")

	if err != nil {
		return err
	}

	_, err = c.Out.Write(append(data, '\n'))

	return err
}
