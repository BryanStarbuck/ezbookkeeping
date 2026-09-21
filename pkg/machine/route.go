package machine

import (
	"sort"
	"strings"

	"github.com/mayswind/ezbookkeeping/pkg/settings"
)

// Tier is the permission tier a route needs (apis.mdx §8.1)
type Tier int

const (
	TierRead Tier = iota
	TierWrite
	TierAdmin
)

func (t Tier) String() string {
	switch t {
	case TierWrite:
		return "write"
	case TierAdmin:
		return "admin"
	default:
		return "read"
	}
}

// Route status values published in /capabilities (apis.mdx §8.3a)
const (
	StatusLive             = "live"
	StatusPlanned          = "planned"
	StatusDisabledUpstream = "disabled_upstream"
)

// HandlerFunc is a machine-plane handler. It returns the value placed in the envelope's `data`,
// or an error (a *Fail, an upstream *errs.Error via UpErr, or anything else, which becomes internal).
type HandlerFunc func(mc *Ctx) (any, error)

// FeatureCheck reports whether an upstream feature a route depends on is switched on, and which
// .ini key switches it
type FeatureCheck func(config *settings.Config) (enabled bool, iniKey string)

// RouteDef is one route of the plane. /capabilities is projected from the same slice the router is
// built from, so there is no second list (apis.mdx §8.3).
type RouteDef struct {
	Method  string
	Path    string // gin syntax, relative to /machine/v1, e.g. "/accounts/:id"
	Tier    Tier
	Summary string
	Handler HandlerFunc

	// Status is StatusLive unless the route is declared but not built yet (StatusPlanned)
	Status string
	// Phase names the build phase a planned route arrives in
	Phase string
	// NoUser marks the diagnostic routes that answer without a bound user
	NoUser bool
	// DryRunnable write routes may be previewed with the write tier off; the apply step checks the
	// tier (RunWrite does this). Write routes without it are refused at the gate when the tier is off.
	DryRunnable bool
	// Composed marks a route that makes more than one service call (R1's named exception)
	Composed bool
	// Feature, when set, gates the route on an upstream .ini switch
	Feature FeatureCheck
	// Untrusted names the response fields that carry operator- or third-party-written text
	Untrusted []string
	// Features are capability tags this route contributes to /capabilities `features`
	Features []string
	// NoIntegerize returns `data` verbatim, skipping the amount-string conversion. Only the
	// /api/* passthrough sets it: there `data` is upstream's `result` as upstream shaped it (§16).
	NoIntegerize bool
}

// routeSources are the family route lists. Each family file appends itself here from init(); the
// order families register in does not matter because Routes() sorts.
var routeSources []func() []RouteDef

func registerRoutes(fn func() []RouteDef) {
	routeSources = append(routeSources, fn)
}

var cachedRoutes []RouteDef

// Routes returns every route of the plane, the ONE array
func Routes() []RouteDef {
	if cachedRoutes != nil {
		return cachedRoutes
	}

	var all []RouteDef

	for _, fn := range routeSources {
		for _, r := range fn() {
			if r.Status == "" {
				r.Status = StatusLive
			}

			all = append(all, r)
		}
	}

	sort.SliceStable(all, func(i, j int) bool {
		if all[i].Path != all[j].Path {
			return all[i].Path < all[j].Path
		}

		return all[i].Method < all[j].Method
	})

	cachedRoutes = all

	return all
}

// capabilityRoute is the /capabilities projection of one path
type capabilityRoute struct {
	Path     string            `json:"path"`
	Methods  []string          `json:"methods"`
	Tier     map[string]string `json:"tier"`
	Status   map[string]string `json:"status"`
	Summary  map[string]string `json:"summary"`
	Composed bool              `json:"composed,omitempty"`
}

func projectCapabilities(config *settings.Config) ([]capabilityRoute, []string) {
	byPath := map[string]*capabilityRoute{}
	var order []string
	featureSet := map[string]bool{}

	for _, r := range Routes() {
		cr, ok := byPath[r.Path]

		if !ok {
			cr = &capabilityRoute{Path: r.Path, Tier: map[string]string{}, Status: map[string]string{}, Summary: map[string]string{}}
			byPath[r.Path] = cr
			order = append(order, r.Path)
		}

		status := r.Status

		if status == StatusLive && r.Feature != nil {
			if enabled, _ := r.Feature(config); !enabled {
				status = StatusDisabledUpstream
			}
		}

		cr.Methods = append(cr.Methods, r.Method)
		cr.Tier[r.Method] = r.Tier.String()
		cr.Status[r.Method] = status
		cr.Summary[r.Method] = r.Summary
		cr.Composed = cr.Composed || r.Composed

		if r.Status == StatusLive {
			for _, f := range r.Features {
				featureSet[f] = true
			}
		}
	}

	out := make([]capabilityRoute, 0, len(order))

	for _, p := range order {
		out = append(out, *byPath[p])
	}

	features := make([]string, 0, len(featureSet))

	for f := range featureSet {
		features = append(features, f)
	}

	sort.Strings(features)

	return out, features
}

// planned declares a mounted-but-unbuilt route
func planned(method, path string, tier Tier, phase, summary string) RouteDef {
	return RouteDef{
		Method:  method,
		Path:    path,
		Tier:    tier,
		Status:  StatusPlanned,
		Phase:   phase,
		Summary: strings.TrimSpace(summary + " NOT IMPLEMENTED YET (phase " + phase + ")."),
		Handler: func(mc *Ctx) (any, error) {
			return nil, NewFail(CodeNotReady, "this route arrives in phase "+phase+"; see GET /machine/v1/capabilities", "route %s %s is planned, not built yet", method, path)
		},
	}
}

// Common upstream feature checks
var (
	featureImport = func(c *settings.Config) (bool, string) { return c.EnableDataImport, "[data] enable_import" }
	featureExport = func(c *settings.Config) (bool, string) { return c.EnableDataExport, "[data] enable_export" }
)
