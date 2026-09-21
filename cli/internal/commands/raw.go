package commands

// raw.go — ESCAPE HATCH: `ezbk raw GET|POST <upstream-path> [--query k=v]... [--data JSON]`
// (cli.mdx §6, §17; apis.mdx §16).
//
// The passthrough forwards to upstream's /api/v1 handler of the same path, as the bound user, behind
// the same gates. It is the upstream API, not the machine plane's write protocol: no dry run, no
// confirm token, no ceiling, no journal. So every write through it prints a one-line warning, and
// the tier still applies (any POST is write-tier; deletes and clear-data are admin-tier; the token,
// 2FA, external-auth, avatar and LLM paths are refused outright).

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"os"
	"sort"
	"strings"

	"github.com/BryanStarbuck/ezbookkeeping/cli/internal/app"
	"github.com/BryanStarbuck/ezbookkeeping/cli/internal/render"
)

const orGroupEscape = "ESCAPE HATCH"

// orRawMaxBody mirrors the plane's body cap (apis.mdx §7.7)
const orRawMaxBody = 16 << 20

func init() {
	app.Register(app.Verb{
		Name:    "raw",
		Group:   orGroupEscape,
		Args:    "GET|POST <upstream-path>",
		MinArgs: 2,
		MaxArgs: 2,
		Summary: "call an upstream /api/v1 path through the plane's passthrough — writes bypass dry run, confirm, ceiling and journal",
		Flags: []app.Flag{
			{Name: "query", Value: "k=v", Repeat: true, Help: "a query-string argument (repeatable), e.g. --query count=50"},
			{Name: "data", Value: "JSON|@file|-", Help: "the JSON body for a POST: inline, @path to read a file, or - for stdin"},
		},
		Run: orRunRaw,
	})
}

// orRawRefused are the upstream path classes the passthrough never forwards (apis.mdx §16)
var orRawRefused = []string{"tokens/", "users/2fa/", "users/external_auth/", "users/avatar/", "users/verify_email/", "llm/"}

// orRawNormalizePath accepts `accounts/list.json`, `/accounts/list.json`, `/api/v1/accounts/list.json`
// or `api/accounts/list.json` and returns the path relative to upstream's /api/v1 ("accounts/list.json")
func orRawNormalizePath(p string) (string, error) {
	p = strings.TrimSpace(p)

	if p == "" {
		return "", fmt.Errorf("an empty upstream path")
	}

	if strings.Contains(p, "://") {
		return "", fmt.Errorf("give the upstream path (e.g. accounts/list.json), not a URL")
	}

	if i := strings.IndexAny(p, "?#"); i >= 0 {
		return "", fmt.Errorf("put query arguments in --query k=v, not in the path")
	}

	p = strings.TrimLeft(p, "/")

	for _, prefix := range []string{"machine/v1/api/", "api/v1/", "api/"} {
		if strings.HasPrefix(p, prefix) {
			p = strings.TrimPrefix(p, prefix)
			break
		}
	}

	p = strings.TrimLeft(p, "/")

	if p == "" {
		return "", fmt.Errorf("the path names no upstream route")
	}

	for _, seg := range strings.Split(p, "/") {
		if seg == ".." || seg == "." {
			return "", fmt.Errorf("the path may not contain . or .. segments")
		}
	}

	return p, nil
}

// orRawRefusedPath reports whether the passthrough refuses this path class
func orRawRefusedPath(p string) bool {
	lower := strings.ToLower(p)

	for _, r := range orRawRefused {
		if strings.HasPrefix(lower, r) || lower == strings.TrimSuffix(r, "/") || strings.HasPrefix(lower, strings.TrimSuffix(r, "/")+".") {
			return true
		}
	}

	return false
}

// orRawTier is the tier the passthrough will demand, for the warning and the error explanation
func orRawTier(method, p string) string {
	lower := strings.ToLower(p)

	if method == "GET" {
		return "read"
	}

	if strings.HasSuffix(lower, "/delete.json") || strings.HasSuffix(lower, "/batch_delete.json") ||
		strings.HasPrefix(lower, "data/clear/") || strings.HasSuffix(lower, "/remove_unused.json") {
		return "admin"
	}

	return "write"
}

// orRawQuery parses --query k=v pairs
func orRawQuery(pairs []string) (url.Values, error) {
	q := url.Values{}

	for _, kv := range pairs {
		eq := strings.Index(kv, "=")

		if eq <= 0 {
			return nil, fmt.Errorf("--query %q is not k=v", kv)
		}

		q.Add(kv[:eq], kv[eq+1:])
	}

	return q, nil
}

// orRawBody reads --data: inline JSON, @file, or - for stdin; it must be valid JSON
func orRawBody(v string, stdin io.Reader) ([]byte, error) {
	var data []byte

	switch {
	case v == "-":
		b, err := io.ReadAll(io.LimitReader(stdin, orRawMaxBody+1))

		if err != nil {
			return nil, fmt.Errorf("reading the body from stdin: %w", err)
		}

		data = b
	case strings.HasPrefix(v, "@"):
		f, err := os.Open(v[1:])

		if err != nil {
			return nil, fmt.Errorf("--data %s: %w", v, err)
		}

		defer f.Close()

		b, err := io.ReadAll(io.LimitReader(f, orRawMaxBody+1))

		if err != nil {
			return nil, fmt.Errorf("--data %s: %w", v, err)
		}

		data = b
	default:
		data = []byte(v)
	}

	if len(data) > orRawMaxBody {
		return nil, fmt.Errorf("the body is larger than the plane's 16 MiB cap")
	}

	data = bytes.TrimSpace(data)

	if !json.Valid(data) {
		return nil, fmt.Errorf("--data is not valid JSON")
	}

	return data, nil
}

func orRunRaw(c *app.Ctx) error {
	method := strings.ToUpper(strings.TrimSpace(c.Args[0]))

	if method != "GET" && method != "POST" {
		return app.Usage(fmt.Sprintf("the method must be GET or POST, got %q", c.Args[0]), "upstream's API only uses GET and POST: ezbk raw GET accounts/list.json")
	}

	path, err := orRawNormalizePath(c.Args[1])

	if err != nil {
		return app.Usage(err.Error(), "e.g. ezbk raw GET accounts/list.json")
	}

	if orRawRefusedPath(path) {
		return app.Usage("the passthrough never forwards "+path+" (tokens, 2FA, external auth, avatars, email verification and LLM are refused — apis.mdx §16)",
			"do this in the browser")
	}

	q, err := orRawQuery(c.Flags("query"))

	if err != nil {
		return app.Usage(err.Error(), "e.g. --query count=50 --query page=1")
	}

	var body any

	if c.Has("data") {
		if method == "GET" {
			return app.Usage("--data is only for POST", "pass GET arguments with --query k=v")
		}

		b, err := orRawBody(c.String("data"), os.Stdin)

		if err != nil {
			return app.Usage(err.Error(), `e.g. --data '{"id":"3401855937219633152"}'  or  --data @body.json`)
		}

		body = b
	} else if method == "POST" {
		body = []byte("{}")
	}

	tier := orRawTier(method, path)

	if method != "GET" {
		c.Warnf("ezbk raw %s %s bypasses the dry run, the confirm token, the change ceiling and the journal — `ezbk undo` cannot reverse it (%s tier)", method, path, tier)
		c.Info("target %s", c.BaseURL())
	}

	env, err := c.Call(method, "/api/"+path, q, body)

	if err != nil {
		if ee, ok := err.(*app.ExitError); ok && tier != "read" && (ee.APICode == "write_disabled" || ee.APICode == "forbidden") && ee.Hint == "" {
			ee.Hint = "this upstream path is " + tier + "-tier; restart with: ezbk stop && ezbk up --allow-write" + map[bool]string{true: " --allow-admin", false: ""}[tier == "admin"]
		}

		return err
	}

	if c.Format == render.FormatJSON {
		return c.Emit(env, render.View{})
	}

	var data any

	if err := env.Decode(&data); err != nil {
		return err
	}

	switch t := data.(type) {
	case map[string]any:
		if rows := orFindRows(t); len(rows) > 0 && c.Format == render.FormatCSV {
			return render.CSV(c.Out, rows, orAutoColumns(rows))
		}

		if c.Format == render.FormatCSV {
			return render.CSV(c.Out, []map[string]any{t}, orAutoColumns([]map[string]any{t}))
		}

		return render.KeyValues(c.Out, t, nil)
	case []any:
		rows := orMaps(t)

		if len(rows) == len(t) && len(rows) > 0 {
			if c.Format == render.FormatCSV {
				return render.CSV(c.Out, rows, orAutoColumns(rows))
			}

			return render.Table(c.Out, rows, orAutoColumns(rows))
		}
	}

	return c.Emit(env, render.View{})
}

// orAutoColumns lays out rows whose shape is not known in advance (the passthrough's upstream
// results): every scalar key, `id` and `name` first, the rest sorted. Values print verbatim —
// upstream's integer-string amounts are not converted on this path (apis.mdx §16).
func orAutoColumns(rows []map[string]any) []render.Column {
	seen := map[string]bool{}
	var keys []string

	for _, r := range rows {
		for k, v := range r {
			if seen[k] {
				continue
			}

			switch v.(type) {
			case map[string]any:
				continue
			}

			seen[k] = true
			keys = append(keys, k)
		}
	}

	rank := func(k string) int {
		switch k {
		case "id":
			return 0
		case "name":
			return 1
		}

		return 2
	}

	sort.SliceStable(keys, func(i, j int) bool {
		if rank(keys[i]) != rank(keys[j]) {
			return rank(keys[i]) < rank(keys[j])
		}

		return keys[i] < keys[j]
	})

	cols := make([]render.Column, 0, len(keys))

	for _, k := range keys {
		cols = append(cols, render.Column{Header: k, Key: k})
	}

	return cols
}
