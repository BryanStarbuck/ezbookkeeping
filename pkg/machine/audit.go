package machine

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// audit.go appends one line per write to ~/T/_ezbookkeeping/machine.audit — what happened, never
// what it was about: no amounts, no comments, no account names (apis.mdx §19.5)

var auditMu sync.Mutex

func auditWrite(mc *Ctx, changed int, ok bool) {
	defer func() { _ = recover() }() // logging can never break a write

	dir := StateDir()

	if err := os.MkdirAll(dir, 0o700); err != nil {
		return
	}

	user := ""

	if mc.User != nil {
		user = mc.User.Username
	}

	client := mc.Client

	if client == "" {
		client = "-"
	}

	route := ""
	tier := ""

	if mc.Route != nil {
		route = mc.Route.Method + " " + mc.Route.Path
		tier = mc.Route.Tier.String()
	}

	line := fmt.Sprintf("%s  route=%q  tier=%s  caller=%s  user=%s  changed=%d  ok=%t\n",
		time.Now().UTC().Format(time.RFC3339), route, tier, client, user, changed, ok)

	auditMu.Lock()
	defer auditMu.Unlock()

	f, err := os.OpenFile(filepath.Join(dir, "machine.audit"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)

	if err != nil {
		return
	}

	_, _ = f.WriteString(line)
	_ = f.Close()
}
