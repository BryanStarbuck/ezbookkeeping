// Package exitcode is the CLI's exit-code contract (cli.mdx §15). Scripts branch on these.
package exitcode

const (
	OK           = 0  // it worked, including "zero results"
	Failed       = 1  // it ran and failed
	Usage        = 2  // usage, or a local refusal before any call
	NotFound     = 3  // a fact: no such account, no statement, no bindable user
	Conflict     = 4  // a statement conflict, a changed plan, undo refusing over a browser edit
	Unreachable  = 5  // the app could not be brought up, or the plane could not be reached
	Unauthorized = 6  // a 401 from a server that IS reachable
	TierRefused  = 7  // --write given but the server was not started with the tier
	NotBuilt     = 69 // EX_UNAVAILABLE
)
