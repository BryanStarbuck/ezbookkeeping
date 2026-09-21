package machine

import (
	"os"
	"time"

	"github.com/mayswind/ezbookkeeping/pkg/core"
	"github.com/mayswind/ezbookkeeping/pkg/datastore"
)

// routes_plane.go — ping · whoami · capabilities · health (apis.mdx §10.0). These answer WITHOUT a
// bound user: a diagnostic that needs the thing it diagnoses is no diagnostic.

func init() {
	registerRoutes(planeRoutes)
}

func planeRoutes() []RouteDef {
	return []RouteDef{
		{Method: "GET", Path: "/ping", Tier: TierRead, NoUser: true, Summary: "Liveness, key fingerprint, tiers, server version.", Handler: handlePing},
		{Method: "GET", Path: "/whoami", Tier: TierRead, NoUser: true, Summary: "Install, bound user, default currency, timezone, tiers, key fingerprint, statements root.", Handler: handleWhoami},
		{Method: "GET", Path: "/capabilities", Tier: TierRead, NoUser: true, Summary: "The machine-readable route table of this build.", Handler: handleCapabilities},
		{Method: "GET", Path: "/health", Tier: TierRead, NoUser: true, Summary: "Database open? user bindable? upstream doors on? what to do next.", Handler: handleHealth},
	}
}

func tiersOf(st *planeState) map[string]bool {
	return map[string]bool{"read": true, "write": st != nil && st.allowWrite, "admin": st != nil && st.allowAdmin}
}

func handlePing(mc *Ctx) (any, error) {
	st := currentState()

	return map[string]any{
		"pong":           true,
		"keyFingerprint": st.fingerprint,
		"tiers":          tiersOf(st),
		"serverVersion":  core.Version,
		"apiVersion":     "v1",
	}, nil
}

func handleWhoami(mc *Ctx) (any, error) {
	st := currentState()
	out := map[string]any{
		"app":             "ezbookkeeping",
		"apiVersion":      "v1",
		"serverVersion":   core.Version,
		"commit":          core.CommitHash,
		"keyFingerprint":  st.fingerprint,
		"credentialsFile": st.credsPath,
		"tiers":           tiersOf(st),
		"timezone":        mc.Loc.String(),
		"armedAt":         st.armedAt.UTC().Format(time.RFC3339),
	}

	if wd, err := os.Getwd(); err == nil {
		out["installPath"] = wd
	}

	if creds, err := ReadCredentials(); err == nil {
		if creds.StatementsRoot != "" {
			out["statementsRoot"] = creds.StatementsRoot
		} else {
			out["statementsRoot"] = nil
		}
	}

	user, err := ResolveBoundUser(mc.Web)

	if err != nil {
		f := toFail(err)
		out["user"] = nil
		out["userProblem"] = map[string]any{"message": f.Message, "hint": f.Hint}
	} else {
		mc.User, mc.Uid = user, user.Uid
		out["user"] = map[string]any{
			"username":        user.Username,
			"nickname":        user.Nickname,
			"defaultCurrency": user.DefaultCurrency,
			"firstDayOfWeek":  user.FirstDayOfWeek,
		}
	}

	return out, nil
}

func handleCapabilities(mc *Ctx) (any, error) {
	st := currentState()
	routes, features := projectCapabilities(mc.Config)
	_, userErr := ResolveBoundUser(mc.Web)

	return map[string]any{
		"apiVersion":    "v1",
		"serverVersion": core.Version,
		"tiers":         tiersOf(st),
		"userBound":     userErr == nil,
		"upstreamFeatures": map[string]bool{
			"dataImport":            mc.Config.EnableDataImport,
			"dataExport":            mc.Config.EnableDataExport,
			"transactionPictures":   mc.Config.EnableTransactionPictures,
			"scheduledTransactions": mc.Config.EnableScheduledTransaction,
			"apiToken":              mc.Config.EnableAPIToken,
			"mcp":                   mc.Config.EnableMCPServer,
		},
		"routes":     routes,
		"limits":     map[string]int{"maxLimit": MaxLimit, "defaultLimit": DefaultLimit, "maxChangesDefault": DefaultMaxChanges, "maxBodyBytes": MaxBodyBytes},
		"errorCodes": AllCodes,
		"features":   features,
	}, nil
}

func handleHealth(mc *Ctx) (any, error) {
	st := currentState()
	dbOpen := datastore.Container != nil && datastore.Container.UserDataStore != nil
	out := map[string]any{
		"databaseOpen": dbOpen,
		"tiers":        tiersOf(st),
		"upstreamDoors": map[string]bool{
			"apiToken": mc.Config.EnableAPIToken,
			"mcp":      mc.Config.EnableMCPServer,
		},
		"listenAddress": mc.Config.HttpAddr,
	}

	var next []string

	user, err := ResolveBoundUser(mc.Web)

	if err != nil {
		f := toFail(err)
		out["userBound"] = false
		out["userProblem"] = map[string]any{"message": f.Message, "hint": f.Hint}
		next = append(next, f.Hint)

		if d, ok := f.Details.(map[string]any); ok {
			out["usernames"] = d["usernames"]
		}
	} else {
		out["userBound"] = true
		out["user"] = user.Username
	}

	if mc.Config.EnableAPIToken || mc.Config.EnableMCPServer {
		next = append(next, "upstream's full-access API-token or MCP door is on; switch [security] enable_api_token and [mcp] enable_mcp off unless you meant it")
	}

	out["healthy"] = dbOpen && err == nil
	out["next"] = next

	return out, nil
}
