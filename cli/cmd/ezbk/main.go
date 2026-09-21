// Command ezbk is the operator CLI for this ezBookkeeping install. Spec: pm/cli.mdx.
package main

import (
	"os"

	"github.com/BryanStarbuck/ezbookkeeping/cli/internal/app"
	_ "github.com/BryanStarbuck/ezbookkeeping/cli/internal/commands"
)

func main() {
	app.Exit(app.Main(os.Args[1:], os.Stdout, os.Stderr))
}
