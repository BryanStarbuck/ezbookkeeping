package machine

import (
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/mayswind/ezbookkeeping/pkg/api"
	"github.com/mayswind/ezbookkeeping/pkg/core"
	"github.com/mayswind/ezbookkeeping/pkg/errfile"
	errfileserver "github.com/mayswind/ezbookkeeping/pkg/errfile/server"
	"github.com/mayswind/ezbookkeeping/pkg/settings"
	"github.com/mayswind/ezbookkeeping/pkg/utils"
)

// EnvCanary switches the hidden canary route on (pm/error_err.mdx §15.3)
const EnvCanary = "EZBK_ERROR_FILE_CANARY"

// errorFileHint is the hint every internal / upstream_error Fail carries (pm/error_err.mdx §8 N8)
const errorFileHint = "read ~/T/ezbookkeeping/error.err for the server-side detail"

// BasePath is where the plane is mounted
const BasePath = "/machine/v1"

// mcpAdminRoutes are the admin routes the MCP client may reach (apis.mdx §8.1): exactly one, the
// ids-only transaction delete the operator asked the agent surface to have. Every other admin route
// still refuses the MCP with forbidden, and the admin tier must be on either way.
var mcpAdminRoutes = map[string]bool{"DELETE /transactions/bulk": true}

// Mount registers the machine plane on the router. It is one of the two lines this fork adds to
// upstream's cmd/webserver.go (apis.mdx §4.1). Until Arm() succeeds every route answers 404.
func Mount(router *gin.Engine, config *settings.Config) {
	upstreamEngine = router

	group := router.Group(BasePath)
	group.Use(gateMiddleware(config))

	for i := range Routes() {
		r := &Routes()[i]
		group.Handle(r.Method, r.Path, wrap(r, config))
	}

	// The hidden canary (pm/error_err.mdx §15.3): only under EZBK_ERROR_FILE_CANARY=1, never in
	// Routes() (so never in /capabilities), behind the same gates, and it panics inside wrap().
	if os.Getenv(EnvCanary) == "1" {
		group.Handle(canaryRoute.Method, canaryRoute.Path, wrap(&canaryRoute, config))
	}

	// POST /error-report — the browser's delivery route (pm/error_err.mdx §9). Outside /machine/v1
	// (no machine key: the browser never holds it) and outside /api/v1 (no JWT: a fault on the
	// login page still arrives). Its safety is the plane's own loopback check plus the guards inside.
	router.POST("/error-report", errfileserver.HandlerWithPeerCheck(func(c *gin.Context) bool {
		return isLoopbackSocket(c, config)
	}))

	// Unknown /machine/v1 paths get the plane's own 404 (behind the same gates); everything else
	// keeps upstream's NoRoute/NoMethod behaviour.
	upstreamNotFound := bindUpstreamApi(api.Default.ApiNotFound, config)
	upstreamNoMethod := bindUpstreamApi(api.Default.MethodNotAllowed, config)
	gates := gateMiddleware(config)

	router.NoRoute(func(c *gin.Context) {
		if strings.HasPrefix(c.Request.URL.Path, BasePath+"/") || c.Request.URL.Path == BasePath {
			gates(c)

			if !c.IsAborted() {
				writeFail(c, NewFail(CodeNotFound, "GET /machine/v1/capabilities lists every route", "no route %s %s", c.Request.Method, c.Request.URL.Path), nil, "")
			}

			return
		}

		upstreamNotFound(c)
	})

	router.NoMethod(func(c *gin.Context) {
		if strings.HasPrefix(c.Request.URL.Path, BasePath+"/") {
			gates(c)

			if !c.IsAborted() {
				writeFail(c, NewFail(CodeNotFound, "GET /machine/v1/capabilities lists every route and its methods", "no route %s %s", c.Request.Method, c.Request.URL.Path), nil, "")
			}

			return
		}

		upstreamNoMethod(c)
	})
}

func bindUpstreamApi(fn core.ApiHandlerFunc, config *settings.Config) gin.HandlerFunc {
	return func(ginCtx *gin.Context) {
		c := core.WrapWebContext(ginCtx, config.TrustedProxyIPs)
		result, err := fn(c)

		if err != nil {
			utils.PrintJsonErrorResult(c, err)
		} else {
			utils.PrintJsonSuccessResult(c, result)
		}
	}
}

// wrap runs gates 4–5 around a handler and renders the envelope
func wrap(r *RouteDef, config *settings.Config) gin.HandlerFunc {
	return func(c *gin.Context) {
		started := time.Now()
		st := currentState()
		web := core.WrapWebContext(c, config.TrustedProxyIPs)

		if web.GetContextId() == "" {
			web.SetContextId("machine-" + started.UTC().Format("20060102T150405.000000"))
		}

		mc := &Ctx{
			Gin:         c,
			Web:         web,
			Config:      config,
			Route:       r,
			Loc:         ResolveLocation(c.GetHeader(core.ClientTimezoneNameHeaderName)),
			Client:      c.GetHeader(HeaderClient),
			WriteTierOn: st != nil && st.allowWrite,
		}

		defer func() {
			if rec := recover(); rec != nil {
				errfile.Recovered("handling "+r.Method+" "+r.Path, rec, errfile.F("request_id", web.GetContextId()))
				writeFail(c, &Fail{Code: CodeInternal, Message: "internal error", Hint: errorFileHint}, mc, "")
			}
		}()

		// gate 4 — tier
		switch r.Tier {
		case TierWrite:
			if !mc.WriteTierOn && !r.DryRunnable {
				writeFail(c, NewFail(CodeWriteDisabled, "restart the app with writes allowed: ezbk stop && ezbk up --allow-write (sets EZBK_MACHINE_ALLOW_WRITE=1)", "the write tier is off on this server"), mc, "")
				return
			}
		case TierAdmin:
			if strings.HasPrefix(strings.ToLower(mc.Client), "ezbookkeeping-mcp") && !mcpAdminRoutes[r.Method+" "+r.Path] {
				writeFail(c, NewFail(CodeForbidden, "admin routes are for the CLI only (ezbk … --write --yes)", "the MCP server is given no admin route but DELETE /transactions/bulk"), mc, "")
				return
			}

			if st == nil || !st.allowAdmin {
				writeFail(c, NewFail(CodeForbidden, "restart the app with admin allowed: ezbk stop && ezbk up --allow-write --allow-admin (sets EZBK_MACHINE_ALLOW_ADMIN=1)", "the admin tier is off on this server"), mc, "")
				return
			}

			mc.WriteTierOn = true
		}

		if r.Feature != nil {
			if enabled, key := r.Feature(config); !enabled {
				writeFail(c, NewFail(CodeForbidden, "switch on "+key+" in conf/ezbookkeeping.ini and restart", "this feature is switched off upstream (%s)", key), mc, "")
				return
			}
		}

		if !r.NoUser {
			user, err := ResolveBoundUser(web)

			if err != nil {
				writeFail(c, reportFail(r, web, err), mc, "")
				return
			}

			mc.User = user
			mc.Uid = user.Uid
		}

		result, err := r.Handler(mc)

		if err != nil {
			writeFail(c, reportFail(r, web, err), mc, "")
			return
		}

		if raw, ok := result.(*RawResult); ok {
			if raw.FileName != "" {
				c.Header("Content-Disposition", "attachment;filename="+raw.FileName)
			}

			c.Data(http.StatusOK, raw.ContentType, raw.Data)
			return
		}

		data := result

		if !r.NoIntegerize {
			converted, ierr := Integerize(result)

			if ierr != nil {
				writeFail(c, reportFail(r, web, ierr), mc, "")
				return
			}

			data = converted
		}

		meta := baseMeta(mc, started)

		for k, v := range mc.meta {
			if k == "journalId" {
				continue
			}

			meta[k] = v
		}

		if len(r.Untrusted) > 0 {
			meta["untrusted"] = r.Untrusted
		}

		if r.Composed {
			meta["composed"] = true
		}

		c.JSON(http.StatusOK, gin.H{"ok": true, "data": data, "meta": meta})
	}
}

// reportFail normalises a handler error into a Fail and reports only what is a real fault
// (pm/error_err.mdx §7 G7): internal and upstream_error are Caught with the request id; every
// other code is a gate refusal or a validation answer, which is Expected (R7).
func reportFail(r *RouteDef, web *core.WebContext, err error) *Fail {
	f := toFail(err)

	if f.Code == CodeInternal || f.Code == CodeUpstreamError {
		errfile.Caught("handling "+r.Method+" "+r.Path, err, errfile.F("request_id", web.GetContextId()), errfile.F("code", f.Code))
	} else {
		errfile.Expected("handling "+r.Method+" "+r.Path, err)
	}

	return f
}

// canaryRoute is the deliberate fault of §15.3: `POST /machine/v1/__canary` panics inside wrap()
var canaryRoute = RouteDef{
	Method:  "POST",
	Path:    "/__canary",
	Tier:    TierRead,
	NoUser:  true,
	Summary: "hidden: panic on purpose so the error file can be checked end to end",
	Handler: func(mc *Ctx) (any, error) {
		panic("errfile canary: a deliberate panic in the machine plane")
	},
}

func baseMeta(mc *Ctx, started time.Time) map[string]any {
	meta := map[string]any{
		"target":        "local",
		"serverVersion": core.Version,
		"commit":        core.CommitHash,
		"asOf":          time.Now().UTC().Format(time.RFC3339),
		"tookMs":        time.Since(started).Milliseconds(),
		"truncated":     false,
	}

	if mc != nil {
		if mc.Loc != nil {
			meta["timezone"] = mc.Loc.String()
		}

		if mc.Route != nil {
			meta["tier"] = mc.Route.Tier.String()
		}

		if mc.User != nil {
			meta["user"] = mc.User.Username
			meta["defaultCurrency"] = mc.User.DefaultCurrency
		}
	}

	return meta
}

func writeFail(c *gin.Context, f *Fail, mc *Ctx, _ string) {
	body := gin.H{"ok": false, "error": f}

	if mc != nil {
		body["meta"] = baseMeta(mc, time.Now())
	}

	c.AbortWithStatusJSON(f.HTTPStatus(), body)
}
