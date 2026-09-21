package app

import (
	"encoding/csv"
	"fmt"
	"io"
	"net/url"
	"strings"
	"time"

	"github.com/BryanStarbuck/ezbookkeeping/cli/internal/client"
	"github.com/BryanStarbuck/ezbookkeeping/cli/internal/progress"
	"github.com/BryanStarbuck/ezbookkeeping/cli/internal/render"
)

// helpers_as.go — small, generic stdout emitters for verbs whose answer is not a single envelope
// view: a CSV with a fixed column contract (the analytics charting series, cli.mdx §9.3), one line
// per item (`ezbk statements missing`, cli.mdx §10.3), and a pre-laid-out text report (the per-account
// plan summary, cli.mdx §10.9). They keep stdout writes inside the app layer, beside Emit.

// EmitCSV writes an RFC 4180 CSV with a header row to stdout
func (c *Ctx) EmitCSV(header []string, records [][]string) error {
	cw := csv.NewWriter(c.Out)

	if err := cw.Write(header); err != nil {
		return err
	}

	for _, rec := range records {
		if err := cw.Write(rec); err != nil {
			return err
		}
	}

	cw.Flush()

	return cw.Error()
}

// EmitLines writes one item per line to stdout (nothing at all for no items, so `| wc -l` counts)
func (c *Ctx) EmitLines(lines []string) error {
	if len(lines) == 0 {
		return nil
	}

	_, err := io.WriteString(c.Out, strings.Join(lines, "\n")+"\n")

	return err
}

// EmitText writes a pre-laid-out report to stdout, ending it with exactly one newline
func (c *Ctx) EmitText(text string) error {
	if text == "" {
		return nil
	}

	if !strings.HasSuffix(text, "\n") {
		text += "\n"
	}

	_, err := io.WriteString(c.Out, text)

	return err
}

// Flags returns every raw value given for a flag, without comma splitting (for values that may
// themselves contain commas, such as a --column-map or a file path)
func (c *Ctx) Flags(name string) []string {
	return append([]string(nil), c.values[name]...)
}

// NewTestCtx builds a Ctx for unit tests of a verb's pure logic (no server is contacted)
func NewTestCtx(verb *Verb, args []string, values map[string][]string, bools map[string]bool, out, errOut io.Writer) *Ctx {
	if values == nil {
		values = map[string][]string{}
	}

	if bools == nil {
		bools = map[string]bool{}
	}

	return &Ctx{Verb: verb, Args: args, values: values, bools: bools, Out: out, Err: errOut, Format: "json"}
}

// CallStreamStages is CallStream for the ingest routes, whose NDJSON progress lines are
// {"type":"progress","stage":…,"done":…,"total":…}: the spinner shows "<phase> — <stage>" and the
// honest count. A route that answers with plain JSON (no progress) works too.
func (c *Ctx) CallStreamStages(method, path string, query url.Values, body any, phase string) (*client.Envelope, error) {
	cl, err := c.Client()

	if err != nil {
		return nil, err
	}

	sp := progress.New(c.Quiet())
	sp.Start(phase)
	started := time.Now()

	env, err := cl.DoStream(method, path, query, body, func(m map[string]any) {
		label := phase

		for _, k := range []string{"stage", "phase"} {
			if s, ok := m[k].(string); ok && s != "" {
				label = phase + " — " + s
				break
			}
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
