// Package app is the CLI's spine: the verb registry, the argument contract (cli.mdx §7), the call
// context every verb runs with, the two-step write flow, and the exit-code mapping.
package app

import (
	"sort"
	"strings"
	"sync"
)

// Flag is one flag a verb declares. The parser derives which flags take a value from these
// declarations — there is no second, hand-maintained list (cli.mdx §7.3).
type Flag struct {
	Name string
	// Value is the placeholder for a value-taking flag ("path", "YYYY-MM-DD", "N"); empty means boolean
	Value string
	Help  string
	// Repeat lets the flag be given more than once (values accumulate)
	Repeat bool
}

// Verb is one command
type Verb struct {
	// Name is the words typed after `ezbk`, e.g. "accounts list"
	Name    string
	Summary string
	// Args describes the positional arguments for help, e.g. "<id|name>"
	Args    string
	MinArgs int
	// MaxArgs is the maximum positional count; -1 means unlimited
	MaxArgs int
	Flags   []Flag
	// Group orders the help listing (ORIENTATION, READING THE BOOKS, …)
	Group string
	// NoServer verbs never need the machine plane (help, key, bare ezbk, up/stop)
	NoServer bool
	// NoBringup verbs use the plane but must never start the server (status, doctor)
	NoBringup bool
	// LongRunning verbs ignore --timeout (cli.mdx §7.4)
	LongRunning bool
	Run         func(c *Ctx) error
}

var registry = struct {
	sync.Mutex
	verbs map[string]*Verb
}{verbs: map[string]*Verb{}}

// Register adds verbs (call from a command family's init())
func Register(verbs ...Verb) {
	registry.Lock()
	defer registry.Unlock()

	for i := range verbs {
		v := verbs[i]

		if v.MaxArgs == 0 && v.MinArgs == 0 && v.Args == "" {
			v.MaxArgs = 0
		}

		if _, dup := registry.verbs[v.Name]; dup {
			panic("duplicate verb: " + v.Name)
		}

		registry.verbs[v.Name] = &v
	}
}

// All returns every verb sorted by group then name
func All() []*Verb {
	registry.Lock()
	defer registry.Unlock()

	out := make([]*Verb, 0, len(registry.verbs))

	for _, v := range registry.verbs {
		out = append(out, v)
	}

	sort.Slice(out, func(i, j int) bool {
		gi, gj := groupRank(out[i].Group), groupRank(out[j].Group)

		if gi != gj {
			return gi < gj
		}

		return out[i].Name < out[j].Name
	})

	return out
}

// Lookup finds the longest registered verb name that prefixes the words
func Lookup(words []string) (*Verb, int) {
	registry.Lock()
	defer registry.Unlock()

	for n := len(words); n >= 1; n-- {
		name := strings.Join(words[:n], " ")

		if v, ok := registry.verbs[name]; ok {
			return v, n
		}
	}

	return nil, 0
}

// Groups in help order
var groupOrder = []string{"ORIENTATION", "READING THE BOOKS", "ANALYTICS", "THE STATEMENTS PIPELINE", "WRITING", "ESCAPE HATCH", "ADMIN"}

func groupRank(g string) int {
	for i, name := range groupOrder {
		if name == g {
			return i
		}
	}

	return len(groupOrder)
}

// UniversalFlags are accepted by every verb (cli.mdx §7.1)
var UniversalFlags = []Flag{
	{Name: "format", Value: "json|table|csv", Help: "what stdout carries (default json)"},
	{Name: "write", Help: "actually write; without it every write verb is a dry run"},
	{Name: "yes", Help: "confirm a write over --max-changes, or an admin verb"},
	{Name: "max-changes", Value: "N", Help: "the write ceiling for this run (default 200)"},
	{Name: "tz", Value: "IANA", Help: "timezone for dates in this call"},
	{Name: "api", Value: "url", Help: "talk to a different install (loopback or https only)"},
	{Name: "timeout", Value: "ms", Help: "per-call timeout (ignored by the long verbs)"},
	{Name: "no-bringup", Help: "never start the app; fail with exit 5 if it is down"},
	{Name: "quiet", Help: "no spinner, no informational stderr"},
	{Name: "verbose", Help: "informational stderr, the resolved target and key fingerprint"},
	{Name: "json-errors", Help: "errors on stderr as one JSON object per line"},
	{Name: "help", Help: "usage for this verb"},
}
