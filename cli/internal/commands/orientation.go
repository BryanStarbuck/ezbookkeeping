package commands

// orientation.go — ORIENTATION (cli.mdx §3.3, §6, §8, §12): bare `ezbk`, status, doctor, up, stop,
// logs, key init|show|rotate, whoami.
//
// These verbs answer "what state is this install in?" and "get it into a working state". They are
// the only verbs that look at the machine as well as the plane (ports, pids, the credentials file,
// the toolchain), and none of them computes anything about money.
//
// Output rule for the report verbs (bare ezbk, status, doctor, key show, up, stop): with no
// --format they print the human report the spec shows; --format json prints the same facts as one
// JSON object; --format table is the human report; --format csv is the rows (doctor) or key,value
// pairs. The report is the answer, so it goes to stdout; everything else goes to stderr.

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/BryanStarbuck/ezbookkeeping/cli/internal/app"
	"github.com/BryanStarbuck/ezbookkeeping/cli/internal/bringup"
	"github.com/BryanStarbuck/ezbookkeeping/cli/internal/client"
	"github.com/BryanStarbuck/ezbookkeeping/cli/internal/credentials"
	"github.com/BryanStarbuck/ezbookkeeping/cli/internal/errfile"
	"github.com/BryanStarbuck/ezbookkeeping/cli/internal/exitcode"
	"github.com/BryanStarbuck/ezbookkeeping/cli/internal/logger"
	"github.com/BryanStarbuck/ezbookkeeping/cli/internal/render"
)

const orGroupOrientation = "ORIENTATION"

// orDevPort is the Vite dev server (`just dev`). It is reported, never gated on: FRONTEND UP ≠ APP UP.
const orDevPort = 8081

func init() {
	app.BareVerb = &app.Verb{
		Name:      "",
		Summary:   "the orientation report — status and what needs doing (never starts the app)",
		Group:     orGroupOrientation,
		NoServer:  true,
		NoBringup: true,
		MaxArgs:   0,
		Run:       orRunBare,
	}

	app.Register(
		app.Verb{
			Name:      "status",
			Summary:   "port, pid, health, the machine-plane ping, tiers, bound user, key fingerprint",
			Group:     orGroupOrientation,
			NoServer:  true,
			NoBringup: true,
			MaxArgs:   0,
			Run:       orRunStatus,
		},
		app.Verb{
			Name:      "doctor",
			Summary:   "is this environment sane? exits 1 if a REQUIRED check fails (never starts the app)",
			Group:     orGroupOrientation,
			NoServer:  true,
			NoBringup: true,
			MaxArgs:   0,
			Flags: []app.Flag{
				{Name: "path", Value: "dir", Help: "the statements root to check (overrides EZBK_STATEMENTS_DIR and the credentials file)"},
			},
			Run: orRunDoctor,
		},
		app.Verb{
			Name:     "up",
			Summary:  "bring the app up and wait for health; already-up is a success unless a tier is missing",
			Group:    orGroupOrientation,
			NoServer: true,
			MaxArgs:  0,
			Flags: []app.Flag{
				{Name: "allow-write", Help: "start the server with the write tier (EZBK_MACHINE_ALLOW_WRITE=1)"},
				{Name: "allow-admin", Help: "start the server with the admin tier (EZBK_MACHINE_ALLOW_ADMIN=1) — deletes and clear-data"},
			},
			Run: orRunUp,
		},
		app.Verb{
			Name:     "stop",
			Summary:  "stop OUR server (the pid ezbk recorded), never a foreign process",
			Group:    orGroupOrientation,
			NoServer: true,
			MaxArgs:  0,
			Run:      orRunStop,
		},
		app.Verb{
			Name:     "logs",
			Summary:  "tail server.log (bring-up), log/ezbookkeeping.log (the app's own) and cli.err",
			Group:    orGroupOrientation,
			NoServer: true,
			MaxArgs:  0,
			Flags: []app.Flag{
				{Name: "follow", Help: "keep printing new lines until Ctrl-C"},
				{Name: "lines", Value: "N", Help: "lines per file (default 40)"},
				{Name: "only", Value: "server|app|cli", Help: "just one of the three logs"},
			},
			Run: orRunLogs,
		},
		app.Verb{
			Name:     "key init",
			Summary:  "mint the API secret key if absent and print its fingerprint (never the key)",
			Group:    orGroupOrientation,
			NoServer: true,
			MaxArgs:  0,
			Run:      orRunKeyInit,
		},
		app.Verb{
			Name:     "key show",
			Summary:  "path, mode, owner, fingerprint, created, created_by, label, bound username — never the key",
			Group:    orGroupOrientation,
			NoServer: true,
			MaxArgs:  0,
			Run:      orRunKeyShow,
		},
		app.Verb{
			Name:     "key rotate",
			Summary:  "mint a NEW API secret key (needs --yes), print its fingerprint; restart the server after",
			Group:    orGroupOrientation,
			NoServer: true,
			MaxArgs:  0,
			Run:      orRunKeyRotate,
		},
		app.Verb{
			Name:    "whoami",
			Summary: "bound user, default currency, timezone, tiers, key fingerprint, statements root",
			Group:   orGroupOrientation,
			MaxArgs: 0,
			Run:     orRunWhoami,
		},
	)
}

// ---------------------------------------------------------------------------------------------
// The probe — everything the report verbs look at, gathered without ever starting the app
// ---------------------------------------------------------------------------------------------

// orPingState classifies the machine-plane ping
type orPingState string

const (
	orPingOK           orPingState = "ok"
	orPingNoKey        orPingState = "no_key"
	orPingKeyProblem   orPingState = "key_problem"
	orPingUnauthorized orPingState = "unauthorized"
	orPingNoPlane      orPingState = "no_plane"
	orPingUnreachable  orPingState = "unreachable"
	orPingSkipped      orPingState = "skipped"
)

// orProbe is one look at the install
type orProbe struct {
	BaseURL string `json:"baseUrl"`
	Local   bool   `json:"local"`
	Port    int    `json:"port"`

	PortOpen    bool   `json:"portOpen"`
	PortPid     int    `json:"portPid,omitempty"`
	PortCmd     string `json:"portCommand,omitempty"`
	RecordedPid int    `json:"recordedPid,omitempty"`
	PidAlive    bool   `json:"recordedPidAlive"`
	PidFile     string `json:"pidFile"`
	DevUIUp     bool   `json:"devUiUp"`

	Healthy       bool   `json:"healthy"`
	ServerVersion string `json:"serverVersion,omitempty"`
	Commit        string `json:"commit,omitempty"`
	HealthError   string `json:"healthError,omitempty"`

	CredentialsFile string `json:"credentialsFile"`
	KeySource       string `json:"keySource,omitempty"`
	KeyFingerprint  string `json:"keyFingerprint,omitempty"`
	KeyProblem      string `json:"keyProblem,omitempty"`

	Ping              orPingState     `json:"ping"`
	PingError         string          `json:"pingError,omitempty"`
	ServerFingerprint string          `json:"serverKeyFingerprint,omitempty"`
	Tiers             map[string]bool `json:"tiers,omitempty"`

	UserBound   *bool          `json:"userBound,omitempty"`
	User        map[string]any `json:"user,omitempty"`
	UserProblem map[string]any `json:"userProblem,omitempty"`
	Usernames   any            `json:"usernames,omitempty"`
	Timezone    string         `json:"timezone,omitempty"`
	InstallPath string         `json:"installPath,omitempty"`

	UpstreamDoors  map[string]bool `json:"upstreamDoors,omitempty"`
	DoorsFrom      string          `json:"upstreamDoorsFrom,omitempty"`
	ListenAddress  string          `json:"listenAddress,omitempty"`
	StatementsRoot string          `json:"statementsRoot,omitempty"`

	key string
	cl  *client.Client
}

// orProbeInstall gathers the facts. It never starts the app and never fails; every problem is a
// field. deep additionally asks /whoami and /health (the plane must be armed for those).
func orProbeInstall(c *app.Ctx, deep bool) *orProbe {
	p := &orProbe{BaseURL: c.BaseURL(), Port: bringup.Port(), PidFile: bringup.PidFile()}
	p.Local = client.IsLoopbackURL(p.BaseURL)

	if u, err := url.Parse(p.BaseURL); err == nil {
		if n, err := strconv.Atoi(u.Port()); err == nil {
			p.Port = n
		}
	}

	if p.Local {
		p.PortOpen = bringup.PortOpen(p.Port)

		if p.PortOpen {
			p.PortPid, p.PortCmd = bringup.PortHolder(p.Port)
		}

		p.RecordedPid, p.PidAlive = bringup.ReadPid()
		p.DevUIUp = bringup.PortOpen(orDevPort)
	}

	if m, err := client.Healthz(p.BaseURL, 2*time.Second); err != nil {
		p.HealthError = err.Error()
	} else {
		p.Healthy = fmt.Sprint(m["status"]) == "ok"
		p.ServerVersion = orAnyString(m["version"])
		p.Commit = orAnyString(m["commit"])

		if !p.Healthy {
			p.HealthError = "healthz.json answered status " + fmt.Sprint(m["status"])
		}
	}

	info := credentials.Inspect()
	p.CredentialsFile = info.Path

	key, source, err := credentials.Resolve()

	switch {
	case err != nil:
		p.KeyProblem = err.Error()
		p.KeySource = source
	case key == "":
		p.KeySource = ""
	default:
		p.key = key
		p.KeySource = source
		p.KeyFingerprint = credentials.Fingerprint(key)
	}

	if creds, err := credentials.Read(); err == nil {
		p.StatementsRoot = creds.StatementsRoot
	}

	if v := strings.TrimSpace(os.Getenv("EZBK_STATEMENTS_DIR")); v != "" {
		p.StatementsRoot = v
	}

	switch {
	case !p.Healthy:
		p.Ping = orPingSkipped
	case p.KeyProblem != "":
		p.Ping = orPingKeyProblem
	case p.key == "":
		p.Ping = orPingNoKey
	default:
		if cerr := client.CheckTarget(p.BaseURL); cerr != nil {
			p.Ping, p.PingError = orPingUnreachable, cerr.Error()
			break
		}

		p.cl = &client.Client{BaseURL: p.BaseURL, Key: p.key, Timezone: c.Location().String(), Timeout: 5 * time.Second}
		env, err := p.cl.Do("GET", "/ping", nil, nil)
		p.Ping, p.PingError = orClassifyPing(err)

		if p.Ping == orPingOK && env != nil {
			m := env.DataMap()
			p.ServerFingerprint = orAnyString(m["keyFingerprint"])
			p.Tiers = orBoolMap(m["tiers"])

			if v := orAnyString(m["serverVersion"]); v != "" && p.ServerVersion == "" {
				p.ServerVersion = v
			}
		}
	}

	if deep && p.Ping == orPingOK {
		if env, err := p.cl.Do("GET", "/whoami", nil, nil); err == nil {
			m := env.DataMap()

			if u, ok := m["user"].(map[string]any); ok {
				p.User = u
				t := true
				p.UserBound = &t
			}

			if up, ok := m["userProblem"].(map[string]any); ok {
				p.UserProblem = up
				f := false
				p.UserBound = &f
			}

			p.Timezone = orAnyString(m["timezone"])
			p.InstallPath = orAnyString(m["installPath"])

			if v := orAnyString(m["statementsRoot"]); v != "" && p.StatementsRoot == "" {
				p.StatementsRoot = v
			}
		}

		if env, err := p.cl.Do("GET", "/health", nil, nil); err == nil {
			m := env.DataMap()
			p.UpstreamDoors = orBoolMap(m["upstreamDoors"])
			p.DoorsFrom = "server"
			p.ListenAddress = orAnyString(m["listenAddress"])

			if u, ok := m["usernames"]; ok && u != nil {
				p.Usernames = u
			}

			if b, ok := m["userBound"].(bool); ok && p.UserBound == nil {
				p.UserBound = &b
			}

			if up, ok := m["userProblem"].(map[string]any); ok && p.UserProblem == nil {
				p.UserProblem = up
			}
		}
	}

	if p.UpstreamDoors == nil {
		if root, err := bringup.RepoRoot(); err == nil {
			p.UpstreamDoors = orIniDoors(root)
			p.DoorsFrom = "conf/ezbookkeeping.ini"
		}
	}

	return p
}

// orClassifyPing maps a /ping failure to the precise diagnosis (cli.mdx §3.2): 401 and 404 have
// completely different fixes
func orClassifyPing(err error) (orPingState, string) {
	if err == nil {
		return orPingOK, ""
	}

	var ae *client.APIError

	if errors.As(err, &ae) {
		switch ae.Status {
		case 401:
			return orPingUnauthorized, "the server holds a different API secret key than the file"
		case 404:
			return orPingNoPlane, "the server has no machine plane (/machine/v1 answered 404)"
		}

		return orPingUnreachable, ae.Error()
	}

	return orPingUnreachable, err.Error()
}

// orIniDoors reads upstream's full-access doors from the checked-in .ini, honouring the EBK_ env
// overrides upstream applies (used only when the server cannot tell us itself)
func orIniDoors(root string) map[string]bool {
	ini := filepath.Join(root, "conf", "ezbookkeeping.ini")
	token, _ := orIniValue(ini, "security", "enable_api_token")
	mcp, _ := orIniValue(ini, "mcp", "enable_mcp")

	if v := os.Getenv("EBK_SECURITY_ENABLE_API_TOKEN"); v != "" {
		token = v
	}

	if v := os.Getenv("EBK_MCP_ENABLE_MCP"); v != "" {
		mcp = v
	}

	return map[string]bool{"apiToken": orTruthy(token), "mcp": orTruthy(mcp)}
}

// orIniValue reads one key from one section of an .ini file
func orIniValue(path, section, key string) (string, bool) {
	f, err := os.Open(path)

	if err != nil {
		errfile.Expected("opening the optional ini file", err)
		return "", false
	}

	defer f.Close()

	return orIniValueFrom(f, section, key)
}

// orIniValueFrom is orIniValue over a reader (pure, for tests)
func orIniValueFrom(r io.Reader, section, key string) (string, bool) {
	sc := bufio.NewScanner(r)
	cur := ""

	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())

		if line == "" || strings.HasPrefix(line, ";") || strings.HasPrefix(line, "#") {
			continue
		}

		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			cur = strings.TrimSpace(line[1 : len(line)-1])
			continue
		}

		if cur != section {
			continue
		}

		eq := strings.Index(line, "=")

		if eq < 0 {
			continue
		}

		if strings.TrimSpace(line[:eq]) == key {
			return strings.Trim(strings.TrimSpace(line[eq+1:]), `"`), true
		}
	}

	return "", false
}

func orTruthy(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "1", "true", "yes", "on":
		return true
	}

	return false
}

func orAnyString(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return t
	}

	return fmt.Sprint(v)
}

func orBoolMap(v any) map[string]bool {
	m, ok := v.(map[string]any)

	if !ok {
		return nil
	}

	out := map[string]bool{}

	for k, x := range m {
		if b, ok := x.(bool); ok {
			out[k] = b
		}
	}

	return out
}

func orOnOff(b bool) string {
	if b {
		return "on"
	}

	return "off"
}

// orHome shortens nothing: paths are full absolute paths, always (cli.mdx §8)
func orAbs(p string) string {
	if p == "" {
		return p
	}

	if strings.HasPrefix(p, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			p = filepath.Join(home, p[2:])
		}
	}

	if a, err := filepath.Abs(p); err == nil {
		return a
	}

	return p
}

// orHumanDefault reports whether a report verb prints its human form (no --format, or table)
func orHumanDefault(c *app.Ctx) bool {
	return !c.Has("format") || c.Format == render.FormatTable
}

// orEmitReport prints a report verb's answer: the human text, or the facts as JSON, or key,value
// CSV pairs
func orEmitReport(c *app.Ctx, human string, facts any, pairs [][2]string) error {
	switch {
	case orHumanDefault(c):
		return c.EmitText(human)
	case c.Format == render.FormatCSV:
		rows := make([][]string, 0, len(pairs))

		for _, p := range pairs {
			rows = append(rows, []string{p[0], p[1]})
		}

		return c.EmitCSV([]string{"key", "value"}, rows)
	}

	return c.EmitJSONValue(facts)
}

// ---------------------------------------------------------------------------------------------
// bare `ezbk` — the orientation report (cli.mdx §8)
// ---------------------------------------------------------------------------------------------

// orBooks is the one-line summary of the books for the orientation report. Every number is the
// server's; the CLI only counts distinct currency codes in the account list it was given.
type orBooks struct {
	Accounts     *int64 `json:"accounts,omitempty"`
	Currencies   int    `json:"currencies,omitempty"`
	Transactions *int64 `json:"transactions,omitempty"`
	LastDate     string `json:"lastTransactionDate,omitempty"`
}

func orProbeBooks(p *orProbe) *orBooks {
	if p.cl == nil || p.Ping != orPingOK || (p.UserBound != nil && !*p.UserBound) {
		return nil
	}

	b := &orBooks{}

	if env, err := p.cl.Do("GET", "/data/statistics", nil, nil); err == nil {
		m := env.DataMap()

		if inner, ok := m["statistics"].(map[string]any); ok {
			m = inner
		}

		if n, ok := render.ToInt64(orFirst(m, "totalAccountCount", "accounts", "accountCount")); ok {
			b.Accounts = &n
		}

		if n, ok := render.ToInt64(orFirst(m, "totalTransactionCount", "transactions", "transactionCount")); ok {
			b.Transactions = &n
		}
	}

	if env, err := p.cl.Do("GET", "/accounts", url.Values{"include_hidden": {"true"}}, nil); err == nil {
		data := env.DataMap()

		if curs, ok := data["currencies"].([]any); ok {
			b.Currencies = len(curs)
		} else {
			seen := map[string]bool{}

			for _, a := range orFlattenAccounts(orFindRows(data, "accounts")) {
				if cur := orAnyString(a["currency"]); cur != "" && cur != "---" {
					seen[cur] = true
				}
			}

			b.Currencies = len(seen)
		}

		if b.Accounts == nil {
			if n, ok := render.ToInt64(data["accountCount"]); ok {
				b.Accounts = &n
			}
		}
	}

	if env, err := p.cl.Do("GET", "/transactions", url.Values{"limit": {"1"}}, nil); err == nil {
		rows := orFindRows(env.DataMap(), "transactions", "items")

		if len(rows) > 0 {
			b.LastDate = orTxnDate(rows[0], nil)
		}
	}

	if b.Accounts == nil && b.Transactions == nil && b.LastDate == "" {
		return nil
	}

	return b
}

func orFirst(m map[string]any, keys ...string) any {
	for _, k := range keys {
		if v, ok := m[k]; ok && v != nil {
			return v
		}
	}

	return nil
}

func orRunBare(c *app.Ctx) error {
	p := orProbeInstall(c, true)
	books := orProbeBooks(p)

	var b strings.Builder
	line := func(label, value string) { fmt.Fprintf(&b, "  %-12s %s\n", label, value) }

	b.WriteString("ezBookkeeping — this machine\n")
	line("server", orServerLine(p))
	line("machine key", orKeyLine(p))
	line("plane", orPlaneLine(p))

	switch {
	case books != nil:
		parts := []string{}

		if books.Accounts != nil {
			s := fmt.Sprintf("%s accounts", orGroupInt(*books.Accounts))

			if books.Currencies > 0 {
				s += fmt.Sprintf(" (%d currenc%s)", books.Currencies, map[bool]string{true: "y", false: "ies"}[books.Currencies == 1])
			}

			parts = append(parts, s)
		}

		if books.Transactions != nil {
			parts = append(parts, orGroupInt(*books.Transactions)+" transactions")
		}

		if books.LastDate != "" {
			parts = append(parts, "last "+books.LastDate)
		}

		line("books", strings.Join(parts, "   "))
	case p.Ping == orPingOK:
		line("books", "—   (the plane answered, but the books could not be read: ezbk doctor)")
	default:
		line("books", "—   (needs the running app)")
	}

	line("upstream", orDoorsLine(p))
	line("statements", orStatementsLine(p.StatementsRoot))
	b.WriteString("\n")

	suggestions := orSuggestions(p)

	for _, s := range suggestions {
		fmt.Fprintf(&b, "  %-26s %s\n", s[0], s[1])
	}

	next := make([]map[string]string, 0, len(suggestions))

	for _, s := range suggestions {
		next = append(next, map[string]string{"command": s[0], "why": s[1]})
	}

	facts := map[string]any{"install": p, "books": books, "statementsRoot": p.StatementsRoot, "next": next}
	pairs := [][2]string{{"server", orServerLine(p)}, {"machine key", orKeyLine(p)}, {"plane", orPlaneLine(p)}, {"upstream", orDoorsLine(p)}, {"statements", orStatementsLine(p.StatementsRoot)}}

	return orEmitReport(c, b.String(), facts, pairs)
}

func orGroupInt(n int64) string {
	s := strconv.FormatInt(n, 10)
	neg := strings.HasPrefix(s, "-")
	s = strings.TrimPrefix(s, "-")

	var out []string

	for len(s) > 3 {
		out = append([]string{s[len(s)-3:]}, out...)
		s = s[:len(s)-3]
	}

	out = append([]string{s}, out...)
	r := strings.Join(out, ",")

	if neg {
		r = "-" + r
	}

	return r
}

func orServerLine(p *orProbe) string {
	url := strings.TrimRight(strings.Replace(p.BaseURL, "127.0.0.1", "localhost", 1), "/") + "/"

	if p.Healthy {
		detail := []string{}

		if p.PidAlive && p.RecordedPid > 0 && (p.PortPid == 0 || p.PortPid == p.RecordedPid) {
			detail = append(detail, fmt.Sprintf("pid %d", p.RecordedPid))
		} else if p.PortPid > 0 {
			detail = append(detail, fmt.Sprintf("pid %d, not started by ezbk", p.PortPid))
		}

		if p.ServerVersion != "" {
			detail = append(detail, "v"+strings.TrimPrefix(p.ServerVersion, "v"))
		}

		s := url + "   UP"

		if len(detail) > 0 {
			s += "    (" + strings.Join(detail, ", ") + ")"
		}

		return s
	}

	if p.Local && p.PortOpen {
		return fmt.Sprintf("%s   NOT HEALTHY   (port %d held by pid %d %s; healthz.json did not answer ok)", url, p.Port, p.PortPid, p.PortCmd)
	}

	return url + "   DOWN   → ezbk up"
}

func orKeyLine(p *orProbe) string {
	path := orAbs(p.CredentialsFile)

	switch {
	case p.KeyProblem != "":
		return path + "   PROBLEM   " + p.KeyProblem
	case p.KeyFingerprint == "":
		return path + "   none yet   → ezbk key init (or ezbk up — the app mints it on first boot)"
	}

	src := path

	if p.KeySource != "" && p.KeySource != p.CredentialsFile {
		src = p.KeySource + " (overrides " + path + ")"
	}

	return fmt.Sprintf("%s   ok   (%s)", src, strings.Replace(p.KeyFingerprint, "…/", "… / ", 1))
}

func orPlaneLine(p *orProbe) string {
	switch p.Ping {
	case orPingOK:
		s := "armed"

		switch {
		case p.User != nil:
			s += fmt.Sprintf("   user %q", orAnyString(p.User["username"]))
		case p.UserProblem != nil:
			s += "   NO USER BOUND (" + orAnyString(p.UserProblem["message"]) + ")"
		}

		return s + fmt.Sprintf("   writes %s   admin %s", orOnOff(p.Tiers["write"]), orOnOff(p.Tiers["admin"]))
	case orPingUnauthorized:
		return "KEY MISMATCH — the server holds a different key (started before a rotation?) → ezbk stop && ezbk up"
	case orPingNoPlane:
		return "absent — this binary predates the machine plane → just build && ezbk stop && ezbk up"
	case orPingNoKey:
		return "unknown — no API secret key to ask with → ezbk key init"
	case orPingKeyProblem:
		return "unknown — the key could not be read (see machine key)"
	case orPingUnreachable:
		return "unreachable — " + p.PingError
	}

	return "—   (the app is not running)"
}

func orDoorsLine(p *orProbe) string {
	if p.UpstreamDoors == nil {
		return "unknown   (cannot find conf/ezbookkeeping.ini; set EZBK_REPO)"
	}

	s := fmt.Sprintf("API tokens %s   MCP %s", orOnOff(p.UpstreamDoors["apiToken"]), orOnOff(p.UpstreamDoors["mcp"]))

	if p.UpstreamDoors["apiToken"] || p.UpstreamDoors["mcp"] {
		s += "   ← WARNING: a full-access bearer door is open that none of the machine plane's gates guard"
	}

	if p.DoorsFrom != "" && p.DoorsFrom != "server" {
		s += "   (from " + p.DoorsFrom + ")"
	}

	return s
}

func orStatementsLine(root string) string {
	if root == "" {
		return "not configured   (set EZBK_STATEMENTS_DIR or ezbookkeeping.statements.root in the credentials file)"
	}

	abs := orAbs(root)
	st, err := os.Stat(abs)

	switch {
	case err != nil:
		return abs + "   NOT READABLE (" + orShortErr(err) + ")"
	case !st.IsDir():
		return abs + "   NOT A DIRECTORY"
	}

	return abs + "   → ezbk statements scan"
}

func orShortErr(err error) string {
	switch {
	case errors.Is(err, os.ErrNotExist):
		return "does not exist"
	case errors.Is(err, os.ErrPermission):
		return "permission denied"
	}

	return err.Error()
}

// orSuggestions is the "what needs doing" block: the fix for whatever is wrong first, then the
// usual next commands
func orSuggestions(p *orProbe) [][2]string {
	var out [][2]string

	switch {
	case p.KeyProblem != "":
		out = append(out, [2]string{"ezbk key show", "the credentials file needs attention"})
	case !p.Healthy && p.Local:
		out = append(out, [2]string{"ezbk up", "start the app (it mints the key on first boot)"})
	case p.Ping == orPingUnauthorized:
		out = append(out, [2]string{"ezbk stop && ezbk up", "restart so the server reads the current key"})
	case p.Ping == orPingNoPlane:
		out = append(out, [2]string{"just build && ezbk stop && ezbk up", "rebuild — the running binary has no machine plane"})
	case p.Ping == orPingNoKey:
		out = append(out, [2]string{"ezbk key init", "mint the API secret key"})
	case p.UserProblem != nil:
		out = append(out, [2]string{orAnyString(p.UserProblem["hint"]), "bind a user"})
	}

	out = append(out,
		[2]string{"ezbk doctor", "check the environment"},
		[2]string{"ezbk statements scan", "look at the statements tree"},
		[2]string{"ezbk analytics summary", "this month so far"},
	)

	return out
}

// ---------------------------------------------------------------------------------------------
// ezbk status
// ---------------------------------------------------------------------------------------------

func orStatusPairs(p *orProbe) [][2]string {
	pairs := [][2]string{
		{"target", p.BaseURL},
		{"healthy", strconv.FormatBool(p.Healthy)},
		{"server_version", p.ServerVersion},
		{"port", strconv.Itoa(p.Port)},
		{"port_open", strconv.FormatBool(p.PortOpen)},
		{"port_pid", orIntOrEmpty(p.PortPid)},
		{"recorded_pid", orIntOrEmpty(p.RecordedPid)},
		{"recorded_pid_alive", strconv.FormatBool(p.PidAlive)},
		{"dev_ui_8081", strconv.FormatBool(p.DevUIUp)},
		{"credentials_file", orAbs(p.CredentialsFile)},
		{"key_source", p.KeySource},
		{"key_fingerprint", p.KeyFingerprint},
		{"ping", string(p.Ping)},
		{"tier_write", strconv.FormatBool(p.Tiers["write"])},
		{"tier_admin", strconv.FormatBool(p.Tiers["admin"])},
	}

	if p.User != nil {
		pairs = append(pairs, [2]string{"user", orAnyString(p.User["username"])}, [2]string{"default_currency", orAnyString(p.User["defaultCurrency"])})
	}

	if p.Timezone != "" {
		pairs = append(pairs, [2]string{"timezone", p.Timezone})
	}

	return pairs
}

func orIntOrEmpty(n int) string {
	if n == 0 {
		return ""
	}

	return strconv.Itoa(n)
}

func orStatusText(p *orProbe) string {
	var b strings.Builder
	line := func(label, value string) { fmt.Fprintf(&b, "  %-13s %s\n", label, value) }

	b.WriteString("ezBookkeeping — status\n")
	line("server", orServerLine(p))

	if p.Local {
		switch {
		case p.PortOpen:
			line(fmt.Sprintf("port %d", p.Port), fmt.Sprintf("held by pid %d %s", p.PortPid, p.PortCmd))
		default:
			line(fmt.Sprintf("port %d", p.Port), "free")
		}

		switch {
		case p.RecordedPid > 0 && p.PidAlive:
			line("pid file", fmt.Sprintf("%s   pid %d (alive, started by ezbk)", orAbs(p.PidFile), p.RecordedPid))
		case p.RecordedPid > 0:
			line("pid file", fmt.Sprintf("%s   pid %d (stale — not running)", orAbs(p.PidFile), p.RecordedPid))
		default:
			line("pid file", orAbs(p.PidFile)+"   none")
		}

		devState := "not running"

		if p.DevUIUp {
			devState = "up"
		}

		line(fmt.Sprintf("dev UI :%d", orDevPort), devState+"   (Vite; never the gate — FRONTEND UP ≠ APP UP)")
	}

	line("machine key", orKeyLine(p))

	if p.ServerFingerprint != "" && p.KeyFingerprint != "" && p.ServerFingerprint != p.KeyFingerprint {
		line("", "the server reports "+p.ServerFingerprint+" — it holds a different key")
	}

	line("plane", orPlaneLine(p))

	if p.User != nil {
		line("user", fmt.Sprintf("%q   default currency %s   first day of week %s",
			orAnyString(p.User["username"]), orAnyString(p.User["defaultCurrency"]), orAnyString(p.User["firstDayOfWeek"])))
	}

	if p.Timezone != "" {
		line("timezone", p.Timezone)
	}

	if p.InstallPath != "" {
		line("install", orAbs(p.InstallPath))
	}

	line("server log", orAbs(bringup.ServerLog()))

	return b.String()
}

func orRunStatus(c *app.Ctx) error {
	p := orProbeInstall(c, true)

	if err := orEmitReport(c, orStatusText(p), p, orStatusPairs(p)); err != nil {
		return err
	}

	return orProbeExit(p)
}

// orProbeExit turns a probe into the exit-code contract for status: 0 armed, 5 down or no plane,
// 6 key mismatch, 2 no key / unreadable key
func orProbeExit(p *orProbe) error {
	switch {
	case !p.Healthy:
		return &app.ExitError{Code: exitcode.Unreachable, Msg: "ezBookkeeping is not running at " + p.BaseURL, Hint: "ezbk up"}
	case p.Ping == orPingUnauthorized:
		return &app.ExitError{Code: exitcode.Unauthorized, Msg: "the server at " + p.BaseURL + " holds a different API secret key than " + p.KeySource, Hint: "it was probably started before a key rotation; restart it: ezbk stop && ezbk up"}
	case p.Ping == orPingNoPlane:
		return &app.ExitError{Code: exitcode.Unreachable, Msg: "the server at " + p.BaseURL + " has no machine plane", Hint: "rebuild and restart: just build && ezbk stop && ezbk up"}
	case p.Ping == orPingUnreachable:
		return &app.ExitError{Code: exitcode.Unreachable, Msg: "the machine plane could not be reached: " + p.PingError, Hint: "ezbk logs"}
	case p.Ping == orPingNoKey:
		return &app.ExitError{Code: exitcode.Usage, Msg: "no API secret key for " + p.BaseURL, Hint: "ezbk key init"}
	case p.Ping == orPingKeyProblem:
		return &app.ExitError{Code: exitcode.Usage, Msg: p.KeyProblem, Hint: "ezbk key show"}
	}

	return nil
}

// ---------------------------------------------------------------------------------------------
// ezbk whoami
// ---------------------------------------------------------------------------------------------

func orRunWhoami(c *app.Ctx) error {
	env, err := c.Call("GET", "/whoami", nil, nil)

	if err != nil {
		return err
	}

	if c.Format == render.FormatJSON {
		return c.Emit(env, render.View{})
	}

	flat := orFlatten("", env.DataMap())

	if c.Format == render.FormatCSV {
		return c.EmitCSV([]string{"key", "value"}, orSortedPairs(flat))
	}

	return render.KeyValues(c.Out, flat, nil)
}

// orFlatten turns nested objects into dotted keys (a presentation of one object as key/value)
func orFlatten(prefix string, m map[string]any) map[string]any {
	out := map[string]any{}

	for k, v := range m {
		key := k

		if prefix != "" {
			key = prefix + "." + k
		}

		if sub, ok := v.(map[string]any); ok {
			for k2, v2 := range orFlatten(key, sub) {
				out[k2] = v2
			}

			continue
		}

		out[key] = v
	}

	return out
}

func orSortedPairs(m map[string]any) [][]string {
	keys := make([]string, 0, len(m))

	for k := range m {
		keys = append(keys, k)
	}

	sort.Strings(keys)
	out := make([][]string, 0, len(keys))

	for _, k := range keys {
		v := m[k]
		s := ""

		if v != nil {
			s = fmt.Sprint(v)
		}

		out = append(out, []string{k, s})
	}

	return out
}

// ---------------------------------------------------------------------------------------------
// ezbk up / ezbk stop (cli.mdx §3.3)
// ---------------------------------------------------------------------------------------------

// orMissingTiers names the tiers asked for that the running server does not have
func orMissingTiers(wantWrite, wantAdmin bool, have map[string]bool) []string {
	var missing []string

	if wantWrite && !have["write"] {
		missing = append(missing, "write")
	}

	if wantAdmin && !have["admin"] {
		missing = append(missing, "admin")
	}

	return missing
}

// orUpFlags renders the flags that would start a server with these tiers
func orUpFlags(write, admin bool) string {
	s := "ezbk up"

	if write {
		s += " --allow-write"
	}

	if admin {
		s += " --allow-admin"
	}

	return s
}

func orRunUp(c *app.Ctx) error {
	wantWrite, wantAdmin := c.Bool("allow-write"), c.Bool("allow-admin")

	if c.Has("api") && c.BaseURL() != bringup.LocalURL() {
		return app.Usage("`ezbk up` starts this checkout's server on "+bringup.LocalURL()+"; it cannot start "+c.BaseURL(),
			"start the other install from its own checkout (EZBK_PORT=<port> ezbk up there)")
	}

	if c.Bool("no-bringup") {
		return app.Usage("`ezbk up --no-bringup` contradicts itself", "drop --no-bringup")
	}

	base := bringup.LocalURL()

	if bringup.Healthy(base) {
		p := orProbeInstall(c, false)

		if p.Ping == orPingOK {
			if missing := orMissingTiers(wantWrite, wantAdmin, p.Tiers); len(missing) > 0 {
				return &app.ExitError{
					Code: exitcode.TierRefused,
					Msg:  fmt.Sprintf("the running server at %s was started without the %s tier", base, strings.Join(missing, " and ")),
					Hint: "a tier is a boot-time decision and `ezbk up` never restarts a server silently; stop it first: ezbk stop && " + orUpFlags(wantWrite || p.Tiers["write"], wantAdmin || p.Tiers["admin"]),
				}
			}

			if (p.Tiers["write"] && !wantWrite) || (p.Tiers["admin"] && !wantAdmin) {
				c.Info("note: the running server has writes %s, admin %s — more than you asked for; `ezbk stop && ezbk up` restarts it read-only", orOnOff(p.Tiers["write"]), orOnOff(p.Tiers["admin"]))
			}
		} else if wantWrite || wantAdmin {
			c.Warnf("cannot confirm the running server's tiers (%s)", orPlaneLine(p))
		}

		c.Info("already up at %s", base)

		return orEmitUp(c, p, true)
	}

	logger.Info("up: bringing the server up write=%t admin=%t", wantWrite, wantAdmin)

	if err := bringup.Up(bringup.Options{AllowWrite: wantWrite, AllowAdmin: wantAdmin, Quiet: c.Quiet()}); err != nil {
		var be *bringup.Error

		if errors.As(err, &be) {
			return &app.ExitError{Code: exitcode.Unreachable, Msg: be.Msg, Hint: orNonEmpty(be.Hint, "ezbk logs"), LogTail: be.LogTail}
		}

		return &app.ExitError{Code: exitcode.Unreachable, Msg: err.Error(), Hint: "ezbk logs", LogTail: bringup.Tail(bringup.ServerLog(), 30)}
	}

	p := orProbeInstall(c, false)

	if p.Ping == orPingOK {
		if missing := orMissingTiers(wantWrite, wantAdmin, p.Tiers); len(missing) > 0 {
			return &app.ExitError{
				Code: exitcode.TierRefused,
				Msg:  "the server came up without the " + strings.Join(missing, " and ") + " tier",
				Hint: "a server started by `just run` or another shell may have answered first; ezbk status",
			}
		}
	}

	return orEmitUp(c, p, false)
}

func orNonEmpty(s, def string) string {
	if strings.TrimSpace(s) == "" {
		return def
	}

	return s
}

func orEmitUp(c *app.Ctx, p *orProbe, already bool) error {
	state := "UP"

	if already {
		state = "UP (already running)"
	}

	pid := p.RecordedPid

	if !p.PidAlive {
		pid = p.PortPid
	}

	human := fmt.Sprintf("%s   %s   pid %d   %s\n", strings.TrimRight(p.BaseURL, "/")+"/", state, pid, orPlaneLine(p))
	facts := map[string]any{"url": p.BaseURL, "up": p.Healthy, "alreadyRunning": already, "pid": pid, "ping": p.Ping, "tiers": p.Tiers, "keyFingerprint": p.KeyFingerprint, "serverLog": orAbs(bringup.ServerLog())}
	pairs := [][2]string{{"url", p.BaseURL}, {"up", strconv.FormatBool(p.Healthy)}, {"already_running", strconv.FormatBool(already)}, {"pid", orIntOrEmpty(pid)}, {"ping", string(p.Ping)}, {"tier_write", strconv.FormatBool(p.Tiers["write"])}, {"tier_admin", strconv.FormatBool(p.Tiers["admin"])}}

	if err := orEmitReport(c, human, facts, pairs); err != nil {
		return err
	}

	switch p.Ping {
	case orPingUnauthorized:
		return &app.ExitError{Code: exitcode.Unauthorized, Msg: "the server is up but holds a different API secret key than " + p.KeySource, Hint: "restart it so it reads the current key: ezbk stop && ezbk up"}
	case orPingNoPlane:
		return &app.ExitError{Code: exitcode.Unreachable, Msg: "the server is up but has no machine plane", Hint: "rebuild and restart: just build && ezbk stop && ezbk up"}
	}

	return nil
}

func orRunStop(c *app.Ctx) error {
	port := bringup.Port()
	pid, err := bringup.Stop()

	if err != nil {
		return &app.ExitError{Code: exitcode.Failed, Msg: "could not stop pid " + strconv.Itoa(pid) + ": " + err.Error(), Hint: "ezbk status"}
	}

	if pid == 0 {
		if bringup.PortOpen(port) {
			holder, cmd := bringup.PortHolder(port)
			facts := map[string]any{"stopped": false, "port": port, "portPid": holder, "portCommand": cmd}

			if err := orEmitReport(c, fmt.Sprintf("nothing stopped — port %d is held by pid %d %s, which ezbk did not start\n", port, holder, cmd), facts, [][2]string{{"stopped", "false"}, {"port_pid", orIntOrEmpty(holder)}}); err != nil {
				return err
			}

			return &app.ExitError{
				Code: exitcode.Failed,
				Msg:  fmt.Sprintf("port %d is held by pid %d (%s) that ezbk did not start; ezbk stops only the server it recorded in %s", port, holder, cmd, orAbs(bringup.PidFile())),
				Hint: "stop it where it was started (Ctrl-C in its `just run` terminal), or `kill " + strconv.Itoa(holder) + "` yourself if you are sure",
			}
		}

		c.Info("no server started by ezbk is running")

		return orEmitReport(c, "not running\n", map[string]any{"stopped": false, "running": false}, [][2]string{{"stopped", "false"}})
	}

	logger.Info("stop: stopped pid=%d", pid)

	if bringup.PortOpen(port) {
		c.Warnf("pid %d was stopped but port %d is still answering — another process holds it now (ezbk status)", pid, port)
	}

	return orEmitReport(c, fmt.Sprintf("stopped pid %d\n", pid), map[string]any{"stopped": true, "pid": pid}, [][2]string{{"stopped", "true"}, {"pid", strconv.Itoa(pid)}})
}

// ---------------------------------------------------------------------------------------------
// ezbk logs (cli.mdx §12.2)
// ---------------------------------------------------------------------------------------------

// orLogFile is one of the three logs
type orLogFile struct {
	Name string
	Path string
}

// orLogFiles returns the logs in display order: bring-up's server.log, the app's own log (the
// .ini's [log] log_path, relative to the checkout), and cli.err
func orLogFiles() []orLogFile {
	files := []orLogFile{{Name: "server", Path: bringup.ServerLog()}}

	if root, err := bringup.RepoRoot(); err == nil {
		rel, ok := orIniValue(filepath.Join(root, "conf", "ezbookkeeping.ini"), "log", "log_path")

		if v := os.Getenv("EBK_LOG_LOG_PATH"); v != "" {
			rel, ok = v, true
		}

		if !ok || rel == "" {
			rel = "log/ezbookkeeping.log"
		}

		if !filepath.IsAbs(rel) {
			rel = filepath.Join(root, rel)
		}

		files = append(files, orLogFile{Name: "app", Path: rel})
	}

	return append(files, orLogFile{Name: "cli", Path: filepath.Join(logger.StateDir(), "cli.err")})
}

func orRunLogs(c *app.Ctx) error {
	n, err := c.Int("lines", 40)

	if err != nil {
		return err
	}

	if n < 0 {
		return app.Usage("--lines must be 0 or more", "ezbk logs --lines 100")
	}

	files := orLogFiles()

	if only := strings.ToLower(c.String("only")); only != "" {
		var picked []orLogFile

		for _, f := range files {
			if f.Name == only {
				picked = append(picked, f)
			}
		}

		if len(picked) == 0 {
			return app.Usage("--only must be server, app or cli", "ezbk logs --only server")
		}

		files = picked
	}

	for _, f := range files {
		c.Info("%-6s %s", f.Name, orAbs(f.Path))
	}

	var b strings.Builder

	for _, f := range files {
		fmt.Fprintf(&b, "==> %s <==\n", orAbs(f.Path))

		if _, err := os.Stat(f.Path); err != nil {
			errfile.Expected("probing for the log file to show", err)
			fmt.Fprintf(&b, "(absent)\n\n")
			continue
		}

		if t := bringup.Tail(f.Path, n); t != "" && n > 0 {
			b.WriteString(t + "\n")
		}

		b.WriteString("\n")
	}

	if err := c.EmitText(strings.TrimRight(b.String(), "\n")); err != nil {
		return err
	}

	if !c.Bool("follow") {
		return nil
	}

	return orFollow(c, files)
}

// orFollow prints what is appended to the logs until interrupted. A file that shrinks was rotated
// and is read again from the start.
func orFollow(c *app.Ctx, files []orLogFile) error {
	offsets := map[string]int64{}

	for _, f := range files {
		if st, err := os.Stat(f.Path); err == nil {
			offsets[f.Path] = st.Size()
		}
	}

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(sig)

	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()

	last := ""

	for {
		select {
		case <-sig:
			return nil
		case <-ticker.C:
		}

		for _, f := range files {
			st, err := os.Stat(f.Path)

			if err != nil {
				errfile.Expected("probing the followed log file", err)
				continue
			}

			off := offsets[f.Path]

			if st.Size() < off {
				off = 0
			}

			if st.Size() == off {
				continue
			}

			chunk, err := orReadFrom(f.Path, off, st.Size())

			if err != nil {
				errfile.Expected("reading the next chunk of the followed log file", err)
				continue
			}

			offsets[f.Path] = st.Size()
			out := ""

			if last != f.Path && len(files) > 1 {
				out = "\n==> " + orAbs(f.Path) + " <==\n"
			}

			last = f.Path

			if err := c.EmitText(out + strings.TrimRight(string(chunk), "\n")); err != nil {
				return err
			}
		}
	}
}

func orReadFrom(path string, from, to int64) ([]byte, error) {
	f, err := os.Open(path)

	if err != nil {
		return nil, err
	}

	defer f.Close()

	if _, err := f.Seek(from, io.SeekStart); err != nil {
		return nil, err
	}

	return io.ReadAll(io.LimitReader(f, to-from))
}

// ---------------------------------------------------------------------------------------------
// ezbk key init | show | rotate (cli.mdx §4, §12.3). The key itself is never printed.
// ---------------------------------------------------------------------------------------------

func orKeyEnvOverride() string {
	for _, name := range []string{"EZBK_API_KEY", "EZBK_API_KEY_FILE"} {
		if strings.TrimSpace(os.Getenv(name)) != "" {
			return name
		}
	}

	return ""
}

func orRunKeyInit(c *app.Ctx) error {
	path, err := credentials.Path()

	if err != nil {
		return app.Fail(exitcode.Failed, "cannot locate the credentials file: "+err.Error(), "set EZBK_CREDENTIALS_FILE")
	}

	before, err := credentials.Read()

	if err != nil {
		if errors.Is(err, credentials.ErrTooPermissive) {
			return app.Usage(err.Error(), "")
		}

		return app.Fail(exitcode.Usage, err.Error(), "ezbk key show")
	}

	existed := credentials.IsWellFormed(before.APIKey)
	key, err := credentials.Mint("ezbk", false)

	if err != nil {
		return app.Fail(exitcode.Failed, "could not mint the API secret key: "+err.Error(), "check that "+filepath.Dir(path)+" is writable by you")
	}

	fp := credentials.Fingerprint(key)
	state := "minted"

	if existed {
		state = "already present (kept)"
	} else {
		logger.Info("key init: minted a new API secret key fp=%s", fp)
	}

	if env := orKeyEnvOverride(); env != "" {
		c.Warnf("%s is set in the environment and overrides the file for every ezbk call", env)
	}

	human := fmt.Sprintf("%s   %s   %s\n", orAbs(path), state, fp)
	facts := map[string]any{"credentialsFile": orAbs(path), "minted": !existed, "keyFingerprint": fp}

	return orEmitReport(c, human, facts, [][2]string{{"credentials_file", orAbs(path)}, {"minted", strconv.FormatBool(!existed)}, {"key_fingerprint", fp}})
}

func orRunKeyShow(c *app.Ctx) error {
	info := credentials.Inspect()
	path := orAbs(info.Path)
	facts := map[string]any{"credentialsFile": path, "exists": info.Exists}
	var pairs [][2]string
	var b strings.Builder
	line := func(label, value string) { fmt.Fprintf(&b, "  %-12s %s\n", label, value) }

	b.WriteString("API secret key (the key itself is never printed)\n")
	line("path", path)
	pairs = append(pairs, [2]string{"path", path})

	if !info.Exists {
		line("status", "absent — ezbk key init (or ezbk up; the app mints it on first boot)")
		pairs = append(pairs, [2]string{"exists", "false"})
		facts["problem"] = "absent"
	} else {
		mode := fmt.Sprintf("%04o", uint32(info.Mode))
		owner := "you"

		if !info.OwnerOK {
			owner = "ANOTHER USER"
		}

		line("mode", mode+map[bool]string{true: "   ok", false: "   TOO PERMISSIVE — chmod 600 " + path}[info.Mode&0o077 == 0])
		line("owner", owner)
		facts["mode"], facts["ownerOk"] = mode, info.OwnerOK
		pairs = append(pairs, [2]string{"exists", "true"}, [2]string{"mode", mode}, [2]string{"owner_ok", strconv.FormatBool(info.OwnerOK)})

		if info.Problem != "" {
			line("problem", info.Problem)
			facts["problem"] = info.Problem
			pairs = append(pairs, [2]string{"problem", info.Problem})
		}
	}

	if creds, err := credentials.Read(); err == nil {
		fp := "none"

		if creds.APIKey != "" {
			if credentials.IsWellFormed(creds.APIKey) {
				fp = credentials.Fingerprint(creds.APIKey)
			} else {
				fp = "MALFORMED — ezbk key rotate --yes"
			}
		}

		for _, kv := range [][2]string{
			{"fingerprint", fp}, {"created", creds.Created}, {"created_by", creds.CreatedBy}, {"label", creds.Label},
			{"username", orNonEmpty(creds.Username, "(not set — the server binds the only user)")},
			{"statements", orNonEmpty(creds.StatementsRoot, "(not set)")}, {"timezone", orNonEmpty(creds.Timezone, "(not set — the system zone)")},
		} {
			line(kv[0], orNonEmpty(kv[1], "—"))
			pairs = append(pairs, kv)
		}

		facts["keyFingerprint"], facts["created"], facts["createdBy"], facts["label"] = fp, creds.Created, creds.CreatedBy, creds.Label
		facts["username"], facts["statementsRoot"], facts["timezone"] = creds.Username, creds.StatementsRoot, creds.Timezone
	}

	if env := orKeyEnvOverride(); env != "" {
		if key, _, err := credentials.Resolve(); err == nil && key != "" {
			line("in effect", env+"   "+credentials.Fingerprint(key)+"   (overrides the file)")
			facts["inEffect"] = map[string]any{"source": env, "keyFingerprint": credentials.Fingerprint(key)}
		} else if err != nil {
			line("in effect", env+"   PROBLEM: "+err.Error())
		}
	}

	return orEmitReport(c, b.String(), facts, pairs)
}

func orRunKeyRotate(c *app.Ctx) error {
	if !c.Bool("yes") {
		return app.Usage("`ezbk key rotate` replaces the API secret key and needs --yes",
			"ezbk key rotate --yes — then restart the server: ezbk stop && ezbk up")
	}

	path, err := credentials.Path()

	if err != nil {
		return app.Fail(exitcode.Failed, "cannot locate the credentials file: "+err.Error(), "set EZBK_CREDENTIALS_FILE")
	}

	var oldFP string

	if creds, err := credentials.Read(); err == nil && credentials.IsWellFormed(creds.APIKey) {
		oldFP = credentials.Fingerprint(creds.APIKey)
	} else if err != nil {
		if errors.Is(err, credentials.ErrTooPermissive) {
			return app.Usage(err.Error(), "")
		}

		return app.Fail(exitcode.Usage, err.Error(), "ezbk key show")
	}

	key, err := credentials.Mint("ezbk", true)

	if err != nil {
		return app.Fail(exitcode.Failed, "could not rotate the API secret key: "+err.Error(), "check that "+filepath.Dir(path)+" is writable by you")
	}

	fp := credentials.Fingerprint(key)
	logger.Info("key rotate: old=%s new=%s", orNonEmpty(oldFP, "none"), fp)

	running := bringup.Healthy(bringup.LocalURL())

	if running {
		c.Warnf("the running server still holds the old key; every call will be refused (exit 6) until you restart it: ezbk stop && ezbk up")
	} else {
		c.Info("the server is not running; it will read the new key when it starts (ezbk up)")
	}

	if env := orKeyEnvOverride(); env != "" {
		c.Warnf("%s is set in the environment and still overrides the file", env)
	}

	human := fmt.Sprintf("%s   rotated   %s → %s\nrestart the server so it reads the new key: ezbk stop && ezbk up   (the MCP reads the same file)\n", orAbs(path), orNonEmpty(oldFP, "none"), fp)
	facts := map[string]any{"credentialsFile": orAbs(path), "oldKeyFingerprint": oldFP, "keyFingerprint": fp, "restartNeeded": true, "serverRunning": running}

	return orEmitReport(c, human, facts, [][2]string{{"credentials_file", orAbs(path)}, {"old_key_fingerprint", oldFP}, {"key_fingerprint", fp}, {"restart_needed", "true"}})
}

// ---------------------------------------------------------------------------------------------
// ezbk doctor (cli.mdx §12.1)
// ---------------------------------------------------------------------------------------------

// orCheck is one doctor row
type orCheck struct {
	Name     string `json:"name"`
	Required bool   `json:"required"`
	Status   string `json:"status"` // ok | fail | warn | skip
	Detail   string `json:"detail"`
	Fix      string `json:"fix,omitempty"`
}

const (
	orOK   = "ok"
	orFAIL = "fail"
	orWARN = "warn"
	orSKIP = "skip"
)

var orGoVersionRe = regexp.MustCompile(`go(\d+(?:\.\d+){0,2})`)

// orParseVersion splits "1.27.1" into comparable integers
func orParseVersion(v string) []int {
	var out []int

	for _, p := range strings.Split(strings.TrimPrefix(strings.TrimSpace(v), "go"), ".") {
		digits := p

		if i := strings.IndexFunc(p, func(r rune) bool { return r < '0' || r > '9' }); i >= 0 {
			digits = p[:i]
		}

		n, err := strconv.Atoi(digits)

		if err != nil {
			errfile.Expected("parsing a version component", err)
			break
		}

		out = append(out, n)
	}

	return out
}

// orVersionAtLeast reports whether have >= want, segment by segment (missing segments are 0)
func orVersionAtLeast(have, want string) bool {
	h, w := orParseVersion(have), orParseVersion(want)

	for i := 0; i < len(h) || i < len(w); i++ {
		a, b := 0, 0

		if i < len(h) {
			a = h[i]
		}

		if i < len(w) {
			b = w[i]
		}

		if a != b {
			return a > b
		}
	}

	return true
}

// orGoModVersion reads the `go` directive of a go.mod
func orGoModVersion(path string) string {
	data, err := os.ReadFile(path)

	if err != nil {
		errfile.Expected("reading the optional go.mod", err)
		return ""
	}

	for _, line := range strings.Split(string(data), "\n") {
		f := strings.Fields(line)

		if len(f) == 2 && f[0] == "go" {
			return f[1]
		}
	}

	return ""
}

func orCheckGo(root string) orCheck {
	ch := orCheck{Name: "go toolchain", Required: true}
	gobin, err := exec.LookPath("go")

	if err != nil {
		errfile.Expected("probing for the go toolchain", err)
		ch.Status, ch.Detail, ch.Fix = orFAIL, "go is not on PATH — the server will not build", "install Go (https://go.dev/dl/) and put it on PATH"
		return ch
	}

	out, err := exec.Command(gobin, "env", "GOVERSION").Output()
	have := strings.TrimSpace(string(out))

	if err != nil || have == "" {
		errfile.Expected("reading GOVERSION from go env", err)
		if o, e := exec.Command(gobin, "version").Output(); e == nil {
			if m := orGoVersionRe.FindStringSubmatch(string(o)); m != nil {
				have = "go" + m[1]
			}
		}
	}

	want := ""

	if root != "" {
		want = orGoModVersion(filepath.Join(root, "go.mod"))
	}

	switch {
	case have == "":
		ch.Status, ch.Detail, ch.Fix = orWARN, gobin+" did not report a version", "go version"
	case want != "" && !orVersionAtLeast(have, want):
		ch.Status, ch.Detail, ch.Fix = orFAIL, fmt.Sprintf("%s (%s) is older than go.mod's go %s", have, gobin, want), "upgrade Go to "+want+" or newer"
	default:
		ch.Status, ch.Detail = orOK, fmt.Sprintf("%s (%s)", have, gobin)

		if want != "" {
			ch.Detail += "  ≥ go " + want
		}
	}

	return ch
}

func orCheckNode() orCheck {
	ch := orCheck{Name: "node / npm", Required: true}
	node, e1 := exec.LookPath("node")
	npm, e2 := exec.LookPath("npm")

	switch {
	case e1 != nil && e2 != nil:
		errfile.Expected("probing for node and npm", errors.Join(e1, e2))
		ch.Status, ch.Detail, ch.Fix = orFAIL, "neither node nor npm is on PATH — the UI will not build", "install Node.js (it brings npm)"
	case e1 != nil:
		errfile.Expected("probing for node", e1)
		ch.Status, ch.Detail, ch.Fix = orFAIL, "node is not on PATH", "install Node.js"
	case e2 != nil:
		errfile.Expected("probing for npm", e2)
		ch.Status, ch.Detail, ch.Fix = orFAIL, "npm is not on PATH", "install npm"
	default:
		v := ""

		if out, err := exec.Command(node, "--version").Output(); err == nil {
			v = strings.TrimSpace(string(out)) + " "
		}

		ch.Status, ch.Detail = orOK, fmt.Sprintf("node %s(%s), npm (%s)", v, node, npm)
	}

	return ch
}

func orCheckStateDir() orCheck {
	dir := logger.StateDir()
	ch := orCheck{Name: "state dir writable", Required: true, Detail: orAbs(dir)}

	if err := os.MkdirAll(dir, 0o700); err != nil {
		ch.Status, ch.Fix = orFAIL, "mkdir -p "+orAbs(dir)+" (or set EZBK_STATE_DIR)"
		ch.Detail += " — " + orShortErr(err)

		return ch
	}

	f, err := os.CreateTemp(dir, ".doctor-*")

	if err != nil {
		ch.Status, ch.Fix = orFAIL, "make "+orAbs(dir)+" writable by you (no logs, no pid file otherwise)"
		ch.Detail += " — " + orShortErr(err)

		return ch
	}

	name := f.Name()
	_ = f.Close()
	_ = os.Remove(name)
	ch.Status = orOK

	return ch
}

func orCheckCredentials() orCheck {
	info := credentials.Inspect()
	ch := orCheck{Name: "credentials file", Required: true, Detail: orAbs(info.Path)}

	if env := orKeyEnvOverride(); env != "" {
		if key, _, err := credentials.Resolve(); err == nil && key != "" {
			ch.Status = orOK
			ch.Detail = env + " is set (" + credentials.Fingerprint(key) + "); the file is not consulted"

			return ch
		} else if err != nil {
			ch.Status, ch.Detail, ch.Fix = orFAIL, env+": "+err.Error(), "unset "+env+" or fix its value"
			return ch
		}
	}

	switch {
	case info.Problem != "":
		ch.Status, ch.Detail = orFAIL, info.Problem
		ch.Fix = "chmod 600 " + orAbs(info.Path)

		if !strings.Contains(info.Problem, "permissive") {
			ch.Fix = "make it a regular file owned by you at mode 0600 (ezbk key show)"
		}
	case !info.Exists:
		ch.Status, ch.Fix = orFAIL, "ezbk key init  (or ezbk up — the app mints it on first boot)"
		ch.Detail += " — absent"
	default:
		creds, err := credentials.Read()

		switch {
		case err != nil:
			ch.Status, ch.Detail, ch.Fix = orFAIL, err.Error(), "repair the JSON, or move it aside and run ezbk key init"
		case creds.APIKey == "":
			ch.Status, ch.Fix = orFAIL, "ezbk key init"
			ch.Detail += " — no ezbookkeeping.machine.api_key yet"
		case !credentials.IsWellFormed(creds.APIKey):
			ch.Status, ch.Fix = orFAIL, "ezbk key rotate --yes"
			ch.Detail += " — the api_key is malformed"
		default:
			ch.Status = orOK
			ch.Detail += fmt.Sprintf(" — 0%o, yours, %s", uint32(info.Mode), credentials.Fingerprint(creds.APIKey))
		}
	}

	return ch
}

func orCheckBuilt(root string) orCheck {
	ch := orCheck{Name: "server + UI built"}

	if root == "" {
		ch.Status, ch.Detail, ch.Fix = orWARN, "cannot find the checkout", "export EZBK_REPO=/path/to/ezbookkeeping"
		return ch
	}

	var missing []string

	for _, rel := range []string{"ezbookkeeping", filepath.Join("dist", "index.html")} {
		if _, err := os.Stat(filepath.Join(root, rel)); err != nil {
			errfile.Expected("probing for a build artifact", err)
			missing = append(missing, rel)
		}
	}

	if len(missing) > 0 {
		ch.Status, ch.Detail, ch.Fix = orWARN, "missing: "+strings.Join(missing, ", ")+" under "+root, "cd "+root+" && just build"
		return ch
	}

	ch.Status, ch.Detail = orOK, filepath.Join(root, "ezbookkeeping")+" and dist/index.html"

	return ch
}

func orCheckHealth(p *orProbe) orCheck {
	ch := orCheck{Name: "healthz.json"}

	if p.Healthy {
		ch.Status, ch.Detail = orOK, p.BaseURL+"/healthz.json ok"

		if p.ServerVersion != "" {
			ch.Detail += " (v" + strings.TrimPrefix(p.ServerVersion, "v") + ")"
		}

		return ch
	}

	ch.Status, ch.Detail, ch.Fix = orWARN, p.BaseURL+" is not answering ("+orNonEmpty(p.HealthError, "down")+")", "ezbk up"

	if p.Local && p.PortOpen && !(p.PidAlive && p.PortPid == p.RecordedPid) {
		ch.Detail = fmt.Sprintf("port %d is held by pid %d %s, which is not a healthy ezBookkeeping", p.Port, p.PortPid, p.PortCmd)
		ch.Fix = fmt.Sprintf("free the port, or use another: EZBK_PORT=%d ezbk up", p.Port+10)
	}

	return ch
}

func orCheckPing(p *orProbe) orCheck {
	ch := orCheck{Name: "machine plane ping"}

	switch p.Ping {
	case orPingOK:
		ch.Status = orOK
		ch.Detail = fmt.Sprintf("armed; writes %s, admin %s", orOnOff(p.Tiers["write"]), orOnOff(p.Tiers["admin"]))
	case orPingSkipped:
		ch.Status, ch.Detail = orSKIP, "the app is not running"
	case orPingNoKey:
		ch.Status, ch.Detail, ch.Fix = orWARN, "no key to ask with", "ezbk key init"
	case orPingKeyProblem:
		ch.Status, ch.Detail, ch.Fix = orWARN, "the key could not be read", "see the credentials file check"
	case orPingUnauthorized:
		ch.Status, ch.Detail, ch.Fix = orWARN, "401 — the server holds a different key than "+p.KeySource, "restart it: ezbk stop && ezbk up"
	case orPingNoPlane:
		ch.Status, ch.Detail, ch.Fix = orWARN, "404 — the running binary predates the machine plane", "rebuild: just build && ezbk stop && ezbk up"
	default:
		ch.Status, ch.Detail, ch.Fix = orWARN, p.PingError, "ezbk logs"
	}

	return ch
}

func orCheckUser(p *orProbe) orCheck {
	ch := orCheck{Name: "bindable user"}

	switch {
	case p.Ping != orPingOK:
		ch.Status, ch.Detail = orSKIP, "needs the machine plane"
	case p.UserBound != nil && *p.UserBound:
		ch.Status = orOK
		ch.Detail = "bound to " + strconv.Quote(orAnyString(orFirstNonNil(p.User, "username")))
	case p.UserProblem != nil:
		ch.Status = orWARN
		ch.Detail = orAnyString(p.UserProblem["message"])
		ch.Fix = orNonEmpty(orAnyString(p.UserProblem["hint"]), "0 users → register one in the browser; 2+ → set ezbookkeeping.machine.username")

		if p.Usernames != nil {
			ch.Detail += fmt.Sprintf(" (users: %v)", p.Usernames)
		}
	default:
		ch.Status, ch.Detail = orSKIP, "the server did not report a binding"
	}

	return ch
}

func orFirstNonNil(m map[string]any, key string) any {
	if m == nil {
		return nil
	}

	return m[key]
}

func orCheckDoors(p *orProbe) orCheck {
	ch := orCheck{Name: "upstream token/MCP doors"}

	if p.UpstreamDoors == nil {
		ch.Status, ch.Detail, ch.Fix = orSKIP, "cannot find conf/ezbookkeeping.ini", "export EZBK_REPO=/path/to/ezbookkeeping"
		return ch
	}

	var open []string

	if p.UpstreamDoors["apiToken"] {
		open = append(open, "[security] enable_api_token")
	}

	if p.UpstreamDoors["mcp"] {
		open = append(open, "[mcp] enable_mcp")
	}

	from := ""

	if p.DoorsFrom != "" && p.DoorsFrom != "server" {
		from = " (read from " + p.DoorsFrom + ")"
	}

	if len(open) > 0 {
		ch.Status = orWARN
		ch.Detail = "!!! " + strings.Join(open, " and ") + " is ON — a full-access bearer door is open that none of the machine plane's gates guard" + from
		ch.Fix = "set them to false in conf/ezbookkeeping.ini (and unset EBK_SECURITY_ENABLE_API_TOKEN / EBK_MCP_ENABLE_MCP), then restart"

		return ch
	}

	ch.Status, ch.Detail = orOK, "API tokens off, upstream MCP off"+from

	return ch
}

// orIsLoopbackAddr reports whether a listen address binds loopback only
func orIsLoopbackAddr(addr string) bool {
	host := strings.TrimSpace(addr)

	if h, _, err := orSplitHostPort(host); err == nil {
		host = h
	}

	host = strings.Trim(host, "[]")

	switch host {
	case "localhost", "127.0.0.1", "::1":
		return true
	}

	return strings.HasPrefix(host, "127.")
}

func orSplitHostPort(s string) (string, string, error) {
	if i := strings.LastIndex(s, ":"); i > 0 && !strings.Contains(s[:i], ":") {
		return s[:i], s[i+1:], nil
	}

	return s, "", errors.New("no port")
}

func orCheckListen(p *orProbe, root string) orCheck {
	ch := orCheck{Name: "listens on loopback only"}
	addr, from := p.ListenAddress, "server"

	if addr == "" && root != "" {
		v, _ := orIniValue(filepath.Join(root, "conf", "ezbookkeeping.ini"), "server", "http_addr")
		addr, from = v, "conf/ezbookkeeping.ini"
	}

	switch {
	case addr == "":
		ch.Status, ch.Detail = orSKIP, "listen address unknown"
	case orIsLoopbackAddr(addr):
		ch.Status, ch.Detail = orOK, addr+" ("+from+")"
	case from != "server":
		// the .ini says 0.0.0.0 but `ezbk up` and `just run` override it with EBK_SERVER_HTTP_ADDR=127.0.0.1
		ch.Status = orOK
		ch.Detail = "the .ini says http_addr = " + addr + ", but ezbk up / just run override it with EBK_SERVER_HTTP_ADDR=127.0.0.1"
	default:
		ch.Status = orWARN
		ch.Detail = "the server listens on " + addr + " — the browser API (/api/v1) is reachable from the network"
		ch.Fix = "restart with ezbk stop && ezbk up (it sets EBK_SERVER_HTTP_ADDR=127.0.0.1)"
	}

	return ch
}

func orCheckSecretKey(root string) orCheck {
	ch := orCheck{Name: "upstream secret_key"}

	if root == "" {
		ch.Status, ch.Detail = orSKIP, "cannot find the checkout"
		return ch
	}

	path := filepath.Join(logger.StateDir(), "data", ".secret_key")
	data, err := os.ReadFile(path)
	val := strings.TrimSpace(string(data))

	if err != nil || val == "" {
		errfile.Expected("reading the optional data/.secret_key", err)
		iniVal, _ := orIniValue(filepath.Join(root, "conf", "ezbookkeeping.ini"), "security", "secret_key")

		if env := os.Getenv("EBK_SECURITY_SECRET_KEY"); env != "" {
			iniVal = env
		}

		switch {
		case iniVal != "" && iniVal != "ezbookkeeping":
			ch.Status, ch.Detail = orOK, "set in the .ini / environment (data/.secret_key absent)"
		default:
			ch.Status = orWARN
			ch.Detail = path + " is absent — 2FA secrets would be encrypted with the public default \"ezbookkeeping\""
			ch.Fix = "ezbk up (or just run) generates it"
		}

		return ch
	}

	if val == "ezbookkeeping" {
		ch.Status, ch.Detail, ch.Fix = orWARN, path+" holds the public default", "openssl rand -hex 24 > "+path+" and restart (existing 2FA enrolments must be redone)"
		return ch
	}

	ch.Status, ch.Detail = orOK, path+" set (not the default) — this is upstream's secret_key, NOT the API secret key"

	return ch
}

func orCheckStatements(c *app.Ctx, p *orProbe) orCheck {
	ch := orCheck{Name: "statements root"}
	root := c.String("path")

	if root == "" {
		root = p.StatementsRoot
	}

	if root == "" {
		ch.Status, ch.Detail = orSKIP, "not configured — only `ezbk statements` needs it"
		ch.Fix = "export EZBK_STATEMENTS_DIR=/path/to/statements (or ezbookkeeping.statements.root in the credentials file)"

		return ch
	}

	abs := orAbs(root)
	st, err := os.Stat(abs)

	switch {
	case err != nil:
		ch.Status, ch.Detail, ch.Fix = orWARN, abs+" — "+orShortErr(err), "fix the path in EZBK_STATEMENTS_DIR or the credentials file"
	case !st.IsDir():
		ch.Status, ch.Detail, ch.Fix = orWARN, abs+" is not a directory", "point it at the directory holding {ENTITY}/{BANK}/{ACCOUNT}"
	default:
		if f, err := os.Open(abs); err != nil {
			ch.Status, ch.Detail, ch.Fix = orWARN, abs+" — "+orShortErr(err), "make it readable by you"
		} else {
			_ = f.Close()
			ch.Status, ch.Detail = orOK, abs+" readable"
		}
	}

	return ch
}

func orRealpath(p string) string {
	if r, err := filepath.EvalSymlinks(p); err == nil {
		p = r
	}

	if a, err := filepath.Abs(p); err == nil {
		return a
	}

	return p
}

func orCheckOnPath(root string) orCheck {
	ch := orCheck{Name: "ezbk on PATH"}
	onPath, err := exec.LookPath("ezbk")
	want := ""

	if root != "" {
		want = orRealpath(filepath.Join(root, "cli", "bin", "ezbk"))
	}

	if err != nil {
		errfile.Expected("probing for ezbk on PATH", err)
		ch.Status, ch.Detail = orWARN, "ezbk is not on PATH"

		if root != "" {
			ch.Fix = `export PATH="` + filepath.Join(root, "cli", "bin") + `:$PATH"   (in ~/.zshrc, once — or: just install-cli)`
		}

		return ch
	}

	got := orRealpath(onPath)

	switch {
	case want == "":
		ch.Status, ch.Detail = orOK, got
	case got == want:
		ch.Status, ch.Detail = orOK, got
	default:
		ch.Status = orWARN
		ch.Detail = "PATH's ezbk is " + got + ", not this checkout's " + want + " — a second checkout?"
		ch.Fix = "put " + filepath.Dir(want) + " first on PATH, or remove the other copy"
	}

	return ch
}

// orDoctorChecks runs every check in the spec's order
func orDoctorChecks(c *app.Ctx) []orCheck {
	root, _ := bringup.RepoRoot()
	p := orProbeInstall(c, true)

	return []orCheck{
		orCheckGo(root),
		orCheckNode(),
		orCheckStateDir(),
		orCheckCredentials(),
		orCheckBuilt(root),
		orCheckHealth(p),
		orCheckPing(p),
		orCheckUser(p),
		orCheckDoors(p),
		orCheckListen(p, root),
		orCheckSecretKey(root),
		orCheckStatements(c, p),
		orCheckOnPath(root),
	}
}

// orDoctorFailures returns the required checks that failed
func orDoctorFailures(checks []orCheck) []orCheck {
	var out []orCheck

	for _, ch := range checks {
		if ch.Required && ch.Status == orFAIL {
			out = append(out, ch)
		}
	}

	return out
}

var orDoctorColumns = []render.Column{
	{Header: "CHECK", Key: "name"},
	{Header: "REQ", Key: "req"},
	{Header: "STATUS", Key: "status"},
	{Header: "DETAIL", Key: "detail"},
	{Header: "FIX", Key: "fix"},
}

func orRunDoctor(c *app.Ctx) error {
	checks := orDoctorChecks(c)
	failed := orDoctorFailures(checks)

	switch {
	case c.Has("format") && c.Format == render.FormatJSON:
		if err := c.EmitJSONValue(map[string]any{"ok": len(failed) == 0, "checks": checks}); err != nil {
			return err
		}
	default:
		rows := make([]map[string]any, 0, len(checks))

		for _, ch := range checks {
			req := ""

			if ch.Required {
				req = "yes"
			}

			status := strings.ToUpper(ch.Status)

			rows = append(rows, map[string]any{"name": ch.Name, "req": req, "status": status, "detail": ch.Detail, "fix": ch.Fix})
		}

		var err error

		if c.Format == render.FormatCSV {
			err = render.CSV(c.Out, rows, orDoctorColumns)
		} else {
			err = render.Table(c.Out, rows, orDoctorColumns)
		}

		if err != nil {
			return err
		}
	}

	for _, ch := range checks {
		if ch.Name == "upstream token/MCP doors" && ch.Status == orWARN {
			c.Warnf("%s", strings.TrimPrefix(ch.Detail, "!!! "))
		}
	}

	if len(failed) > 0 {
		names := make([]string, 0, len(failed))

		for _, f := range failed {
			names = append(names, f.Name)
		}

		return &app.ExitError{Code: exitcode.Failed, Msg: fmt.Sprintf("%d required check(s) failed: %s", len(failed), strings.Join(names, ", ")), Hint: failed[0].Fix}
	}

	return nil
}
