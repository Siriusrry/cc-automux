package shim

import (
	"fmt"
	"net/url"
	"strings"
)

// classifierPlatform is the resolved upstream platform of a configured global
// classifier target. The fix a classifier request receives is chosen by
// this resolved platform ⊗ the request's model family, rather than by the URL
// prefix the request arrived on. Only these three concrete values exist: the
// config's target_type ("auto"/"") is resolved to one of them at compile time,
// so there is deliberately no "auto" enum value here.
type classifierPlatform int

const (
	// platformGeneric is the zero value and the safest default: no platform-
	// specific classifier adaptation (no AnyRouter identity cloak, no CPA session
	// isolation). An auto target matching neither a configured AnyRouter entrance
	// nor the CPA upstream resolves here, as does an explicit target_type:"generic".
	platformGeneric classifierPlatform = iota
	platformAnyRouter
	platformCPA
)

// resolveConfiguredClassifierPlatform maps the configured classifier.target_type
// to a concrete platform. An explicit anyrouter/cpa/generic maps directly; the
// default ""/auto is resolved by matching the target URL against the configured
// AnyRouter entrances and CPA upstream (resolveClassifierPlatform). An unknown
// type string is a configuration error. The caller invokes this only when a
// global classifier target is actually configured; the field remains lazy
// (unvalidated) otherwise.
func resolveConfiguredClassifierPlatform(targetType string, target *url.URL, anyRouterUpstreams []*url.URL, cliproxyUpstream *url.URL) (classifierPlatform, error) {
	switch targetType {
	case "", "auto":
		return resolveClassifierPlatform(target, anyRouterUpstreams, cliproxyUpstream), nil
	case "anyrouter":
		return platformAnyRouter, nil
	case "cpa":
		return platformCPA, nil
	case "generic":
		return platformGeneric, nil
	default:
		return platformGeneric, fmt.Errorf("classifier.target_type: unknown value %q (want auto, anyrouter, cpa, or generic)", targetType)
	}
}

// resolveClassifierPlatform identifies an auto target's platform by exact
// canonical-URL match against the configured upstreams: a match with any
// AnyRouter entrance yields platformAnyRouter, a match with the CPA upstream
// yields platformCPA, and anything else yields platformGeneric. AnyRouter is
// checked first so an (impossible-in-practice) overlap is deterministic.
//
// Note: a PUBLIC CPA whose URL is not the configured loopback CPA upstream is
// not recognized by auto (it resolves to generic); selecting it needs an
// explicit target_type:"cpa". That is by design — auto only knows the upstreams
// it is configured with.
func resolveClassifierPlatform(target *url.URL, anyRouterUpstreams []*url.URL, cliproxyUpstream *url.URL) classifierPlatform {
	if target == nil {
		return platformGeneric
	}
	key := canonicalUpstreamKey(target)
	for _, entrance := range anyRouterUpstreams {
		if canonicalUpstreamKey(entrance) == key {
			return platformAnyRouter
		}
	}
	if cliproxyUpstream != nil && canonicalUpstreamKey(cliproxyUpstream) == key {
		return platformCPA
	}
	return platformGeneric
}

// canonicalUpstreamKey reduces a URL to a comparable "scheme://host:port/path"
// string for exact upstream-identity matching. It lowercases the scheme and
// host, fills in the scheme's default port when none is given (http=80,
// https=443) so https://h and https://h:443 compare equal, and strips trailing
// slashes from the path so https://h and https://h/ compare equal. It does NOT
// do fuzzy equivalence: http vs https, localhost vs 127.0.0.1, and distinct
// ports stay distinct (host case-folding is the only host-equivalence applied,
// since DNS hostnames are case-insensitive).
func canonicalUpstreamKey(u *url.URL) string {
	if u == nil {
		return ""
	}
	scheme := strings.ToLower(u.Scheme)
	host := strings.ToLower(u.Hostname())
	port := u.Port()
	if port == "" {
		switch scheme {
		case "http":
			port = "80"
		case "https":
			port = "443"
		}
	}
	return scheme + "://" + host + ":" + port + strings.TrimRight(u.Path, "/")
}
