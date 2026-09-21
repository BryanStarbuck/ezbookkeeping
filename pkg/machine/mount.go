package machine

import (
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/mayswind/ezbookkeeping/pkg/api"
	"github.com/mayswind/ezbookkeeping/pkg/core"
	"github.com/mayswind/ezbookkeeping/pkg/log"
	"github.com/mayswind/ezbookkeeping/pkg/settings"
	"github.com/mayswind/ezbookkeeping/pkg/utils"
)

// BasePath is where the plane is mounted
const BasePath = "/machine/v1"

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
				log.Errorf(web, "[machine.wrap] panic in %s %s: %v", r.Method, r.Path, rec)
				writeFail(c, &Fail{Code: CodeInternal, Message: "internal error", Hint: "read log/ezbookkeeping.log for the server-side detail"}, mc, "")
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
			if strings.HasPrefix(strings.ToLower(mc.Client), "ezbookkeeping-mcp") {
				writeFail(c, NewFail(CodeForbidden, "admin routes are for the CLI only (ezbk … --write --yes)", "the MCP server is never given the admin tier"), mc, "")
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
				writeFail(c, toFail(err), mc, "")
				return
			}

			mc.User = user
			mc.Uid = user.Uid
		}

		result, err := r.Handler(mc)

		if err != nil {
			writeFail(c, toFail(err), mc, "")
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
				writeFail(c, toFail(ierr), mc, "")
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
