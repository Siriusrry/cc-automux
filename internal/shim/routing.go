package shim

import (
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

type routingTable struct {
	routes []proxyRoute
	// classifierTarget is the optional single upstream every auto-mode classifier
	// request is routed to (set iff Classifier.TargetBaseURL is configured); nil
	// means classifier requests fall back to their prefix's default upstream.
	classifierTarget *upstreamPool
	classifierClient *http.Client
	// classifierTargetType is the resolved upstream platform of classifierTarget
	// dispatchClassifier consults it — instead of the matched prefix — to pick
	// the per-platform classifier adaptation when a global target is configured. It
	// is meaningful only when classifierTarget != nil; otherwise it stays
	// platformGeneric (the zero value) and is never read.
	classifierTargetType classifierPlatform
}

type proxyRoute struct {
	name        string
	prefix      string
	upstreams   *upstreamPool
	credentials credentialProvider
	client      *http.Client
	strategy    proxyStrategy
}
type resolvedTarget struct {
	routeName string
	baseURLs  *upstreamPool
	key       string
	client    *http.Client
	profile   proxyStrategy
	// globalTarget is true when dispatchClassifier redirected this request to the
	// configured single global classifier target (rather than the matched
	// prefix's default upstream). Such a request must never draw from the
	// prefix's account pool: with an empty target_key it forwards the client's
	// own auth, and it is never charged against an AnyRouter account's health.
	globalTarget bool
}

func buildRoutingTable(compiled *compiledRuntimeConfig) (*routingTable, error) {
	if compiled == nil {
		return nil, fmt.Errorf("compiled runtime config is nil")
	}
	table := &routingTable{routes: []proxyRoute{
		{
			name:        "anyrouter",
			prefix:      anyRouterPrefix,
			upstreams:   newUpstreamPool(compiled.anyRouterUpstreams),
			credentials: newAccountPool(compiled.runtime.AnyRouter.Accounts),
			client:      newAnyRouterHTTPClient(),
			strategy:    anyRouterStrategy{},
		},
		{
			name:      "cliproxy",
			prefix:    cliproxyPrefix,
			upstreams: newUpstreamPool([]*url.URL{compiled.cliproxyUpstream}),
			// CPA owns a single shim key (cpa.key) — the degenerate one-element
			// credentialProvider. The main path selects it the same way it selects an
			// AnyRouter rotation key; an empty cpa.key forwards the client's own auth.
			credentials: staticKeyProvider{key: compiled.runtime.CPA.Key},
			client:      newCliproxyHTTPClient(compiled.cliproxyTLSConfig),
			strategy:    cliproxyStrategy{},
		},
	}}
	// A configured classifier target captures every classifier request regardless
	// of prefix; build it as a single-entry pool with its own client, mirroring
	// the AnyRouter entrance pool.
	if compiled.classifierTarget != nil {
		table.classifierTarget = newUpstreamPool([]*url.URL{compiled.classifierTarget})
		// The global classifier target uses the generalized target client with the
		// resolved TLS config (nil ⇒ verify against the system roots; a custom CA
		// or skip-verify makes a self-signed / dev CPA target first-class). The client
		// keeps DisableCompression so gpt response reassembly reads the raw body.
		table.classifierClient = newTargetHTTPClient(compiled.classifierTargetTLS)
		// The resolved platform (explicit target_type or auto-detected) decides the
		// per-platform classifier adaptation in dispatchClassifier.
		table.classifierTargetType = compiled.classifierTargetType
	}
	return table, nil
}
func (p *proxyServer) resolveTarget(state *proxyState, route *proxyRoute, strippedPath string, rawBody []byte) resolvedTarget {
	base := resolvedTarget{
		routeName: route.name,
		baseURLs:  route.upstreams,
		client:    route.client,
		profile:   route.strategy,
	}
	// The shim-owned key (AnyRouter rotation account or CPA's static cpa.key) is
	// selected from the route's credentialProvider on the main path; a classifier
	// global target's key (set in dispatchClassifier below) takes precedence over it.
	return dispatchClassifier(state, route, base, strippedPath, rawBody)
}

// dispatchClassifier decouples classifier handling from the URL prefix. For a
// detected classifier request it selects the rewrite profile by model family
// (after applying any model-name override), routes to the configured global
// target when set (else the prefix's default upstream), and tags the strategy
// with the destination's resolved platform: the matched prefix's platform by
// default, or the global target's resolved target_type when one is configured.
// The strategy then applies each upstream adaptation by platform ⊗ profile — CPA
// session isolation for an openai-profile classifier to CPA, the AnyRouter cloak
// for an anthropic-profile classifier to AnyRouter, generic pass-through
// otherwise. Non-classifier requests and non-/v1/messages paths keep the prefix's
// default behavior untouched.
func dispatchClassifier(state *proxyState, route *proxyRoute, base resolvedTarget, strippedPath string, rawBody []byte) resolvedTarget {
	if !isClaudeMessagesPath(strippedPath) {
		return base
	}
	body, ok, err := parseAutoModeClassifier(rawBody)
	if err != nil || !ok {
		return base
	}

	classifierCfg := state.runtime.Classifier
	profile := classifierProfile(classifierRequestModel(body, classifierCfg.ModelOverride))

	target := base
	// The classifier adaptation is chosen by the destination's resolved platform,
	// not the URL prefix. With no global target the destination IS the matched
	// prefix's backend, so map the prefix to its platform.
	var platform classifierPlatform
	switch route.prefix {
	case cliproxyPrefix:
		platform = platformCPA
	case anyRouterPrefix:
		platform = platformAnyRouter
	default:
		platform = platformGeneric
	}
	if state.table != nil && state.table.classifierTarget != nil {
		target.baseURLs = state.table.classifierTarget
		target.client = state.table.classifierClient
		// Mark this request as redirected to the global target so ServeHTTP skips
		// the prefix's account pool entirely: an account key must never be applied
		// to — nor a host's failures charged against — the global target. This
		// invariant rides on the globalTarget flag, NOT on the platform: a
		// target_type:"anyrouter" global target still never draws an account.
		target.globalTarget = true
		// Label the route by the global target host instead of the matched
		// prefix, so logs read "classifier→<host> rewrote ..." rather than
		// misattributing the request to "anyrouter"/"cliproxy".
		target.routeName = "classifier→" + target.baseURLs.url(0).Host
		// Override auth with the configured classifier target key. The
		// per-attempt applyAccountAuth swaps it into Authorization: Bearer
		// (replacing the client's auth) — the same shim-owned-auth mechanism used
		// for AnyRouter accounts and the CPA key. Empty ⇒ forward the client's auth.
		target.key = classifierCfg.TargetKey
		// The global target's platform was resolved from an explicit target_type,
		// or auto-detected against the configured AnyRouter entrances / CPA upstream).
		// Use it so the per-platform fix (CPA isolation / AnyRouter cloak / generic
		// pass-through) follows the REAL target platform rather than the prefix the
		// request happened to arrive on — this generalizes the global target to any
		// upstream rather than assuming every global target is generic.
		platform = state.table.classifierTargetType
	}
	target.profile = classifierProfileStrategy{
		profile:       profile,
		modelOverride: classifierCfg.ModelOverride,
		platform:      platform,
	}
	return target
}

// attemptTarget tries the target's entrances in sticky order: the entrance
// currently in use first, then the others. Each entrance is attempted at most
// once per request, and failover only happens while nothing has been written
// back to the client; when every entrance fails, the last real error or
// response is passed through unchanged (fail closed, never synthesized).
//
// The upstreamExhausted result is true only when ok is false because at least
// one entrance was contacted and the final failure was a genuine
// transport/upstream failure (account-attributable). It is false for a
// request-build error (the shim's fault), for a client cancel, and for the
// defensive no-entrance fall-through (no entrance was ever contacted — a
// shim/config fault, not the account's), so callers can avoid penalizing the
// account in those cases.
func (rt *routingTable) matchRoute(path string) (*proxyRoute, string, bool) {
	if rt == nil {
		return nil, "", false
	}
	for i := range rt.routes {
		route := &rt.routes[i]
		if path == route.prefix {
			return route, "/", true
		}
		if strings.HasPrefix(path, route.prefix+"/") {
			stripped := strings.TrimPrefix(path, route.prefix)
			if stripped == "" {
				stripped = "/"
			}
			return route, stripped, true
		}
	}
	return nil, "", false
}

func resolveUpstreamURL(upstream *url.URL, req *http.Request, strippedPath string) *url.URL {
	target := *upstream
	basePath := strings.TrimRight(target.Path, "/")
	incomingPath := "/" + strings.TrimLeft(strippedPath, "/")
	if basePath == "" {
		target.Path = incomingPath
	} else {
		target.Path = basePath + incomingPath
	}
	target.RawPath = ""
	target.RawQuery = req.URL.RawQuery
	return &target
}
