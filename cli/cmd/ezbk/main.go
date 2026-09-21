// Command ezbk is the operator CLI for this ezBookkeeping install. Spec: pm/cli.mdx.
package main

import (
	"os"

	"github.com/BryanStarbuck/ezbookkeeping/cli/internal/app"
	_ "github.com/BryanStarbuck/ezbookkeeping/cli/internal/commands"
	"github.com/BryanStarbuck/ezbookkeeping/cli/internal/errfile"
)

func main() {
	// Every fault ezbk sees goes to ~/T/ezbookkeeping/error.err (pm/error_err.mdx §8 N13). The
	// vendored library flushes asynchronously; the explicit flush makes the last 5 ms safe.
	flush := errfile.Install(errfile.Options{App: "ezbk", StatementsRoot: os.Getenv("EZBK_STATEMENTS_DIR")})
	code := app.Main(os.Args[1:], os.Stdout, os.Stderr)
	flush()
	app.Exit(code)
}
