package shim

import (
	"crypto/tls"
	"fmt"
	"net"
	"net/url"
	"os"
	"strconv"
	"strings"
)

const (
	anyRouterPrefix = "/any"
	cliproxyPrefix  = "/cpa"

	defaultListenAddr = "127.0.0.1:8765"
	// Both AnyRouter entrances front the same backend; the list is ordered —
	// the first entry is the entrance used at startup.
	defaultAnyRouterUpstreamURLs = "https://anyrouter.top,https://a-ocnfniawgw.cn-shanghai.fcapp.run"
	defaultCliproxyUpstreamURL   = "https://127.0.0.1:8317"
)

func validateListenAddr(addr string) error {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("invalid listen address: %w", err)
	}
	if host != "127.0.0.1" {
		return fmt.Errorf("listen address must listen on 127.0.0.1, got %q", host)
	}
	// SplitHostPort accepts an empty or non-numeric port ("127.0.0.1:" binds a
	// random port; "127.0.0.1:abc" persists and bricks startup). Require an
	// explicit numeric port in range, since POST /admin/config now reaches here.
	n, err := strconv.Atoi(port)
	if err != nil {
		return fmt.Errorf("listen address port must be numeric, got %q", port)
	}
	// strconv.Atoi accepts a leading '+' and leading zeros ("+8080", "08081"),
	// which would persist a non-canonical port literally. Require the canonical
	// decimal form.
	if strconv.Itoa(n) != port {
		return fmt.Errorf("listen address port must be canonical decimal, got %q", port)
	}
	if n < 1 || n > 65535 {
		return fmt.Errorf("listen address port must be in 1-65535, got %d", n)
	}
	return nil
}

func getenvDefault(name, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(name)); value != "" {
		return value
	}
	return fallback
}

func parseUpstream(raw string) (*url.URL, error) {
	parsed, err := url.Parse(raw)
	if err != nil {
		return nil, err
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return nil, fmt.Errorf("scheme must be http or https")
	}
	if parsed.Host == "" {
		return nil, fmt.Errorf("host is required")
	}
	return parsed, nil
}

// parseUpstreamEntries validates an ordered list of upstream URLs. Blank
// entries are ignored; duplicates are a configuration error; at least one valid
// URL is required.
func parseUpstreamEntries(entries []string) ([]*url.URL, error) {
	var out []*url.URL
	seen := map[string]struct{}{}
	for _, part := range entries {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		parsed, err := parseUpstream(part)
		if err != nil {
			return nil, fmt.Errorf("entry %q: %w", part, err)
		}
		key := parsed.String()
		if _, dup := seen[key]; dup {
			return nil, fmt.Errorf("duplicate upstream %q", part)
		}
		seen[key] = struct{}{}
		out = append(out, parsed)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("at least one upstream URL is required")
	}
	return out, nil
}

type runtimeConfig struct {
	ListenAddr string `json:"listen_addr"`
	// Enabled is the global feature switch. When false the shim degrades to a
	// plain prefix-routed reverse proxy (passthrough), bypassing account rotation,
	// classifier rewrite/fix, response reassembly, and shim-owned auth. It is a
	// *bool on purpose: an older config file that predates this field simply omits
	// the key, and a missing key decodes to the zero value false — plain
	// encoding/json behavior, unrelated to DisallowUnknownFields (which only rejects
	// *unknown* keys). A bare bool would therefore make such a config decode as
	// "disabled", silently turning the gateway into passthrough on upgrade; the *bool
	// distinguishes unset (nil ⇒ default on) from an explicit false. nil/null ⇒ on.
	Enabled *bool `json:"enabled"`
	// LogMaxBytes is the per-file cap (in bytes) for the stdout/stderr logs;
	// oldest complete lines are dropped past it (cappedLogWriter). It stores
	// bytes to match CC_AUTO_SHIM_LOG_MAX_BYTES and cappedLogWriter (the admin UI
	// shows MB). Unlike Enabled — a *bool because false is a legal value — this is
	// a plain int64 with 0 as the "unset" sentinel: 0 is never a valid cap, so it
	// cannot collide with a meaningful setting the way a bare bool false would. An
	// absent key (a config predating this field decodes to 0) and an explicit 0
	// both fall back to defaultMaxLogBytes in normalizeRuntimeConfig; a negative
	// value is rejected by compileRuntimeConfig. No omitempty (mirrors the other
	// fields): the normalized config always carries a positive value, so GET
	// /admin/config and the on-disk file keep the same shape.
	LogMaxBytes int64                   `json:"log_max_bytes"`
	AnyRouter   anyRouterRuntimeConfig  `json:"anyrouter"`
	CPA         cpaRuntimeConfig        `json:"cpa"`
	Classifier  classifierRuntimeConfig `json:"classifier"`
}

// isEnabled reports whether the global feature switch is on. An absent (nil)
// switch is treated as on so a config file written before the field existed keeps
// today's full-gateway behavior.
func (c *runtimeConfig) isEnabled() bool {
	return c.Enabled == nil || *c.Enabled
}

type anyRouterRuntimeConfig struct {
	Entrances []string       `json:"entrances"`
	Accounts  []accountEntry `json:"accounts"`
}

type accountEntry struct {
	Label string `json:"label"`
	Key   string `json:"key"`
}

type cpaRuntimeConfig struct {
	Upstream string `json:"upstream"`
	Key      string `json:"key"`
	CAPath   string `json:"ca_path"`
}

type classifierRuntimeConfig struct {
	TargetBaseURL string `json:"target_base_url"`
	TargetKey     string `json:"target_key"`
	ModelOverride string `json:"model_override"`
	// These generalized target fields keep platform and model adaptation orthogonal.
	// They are validated and compiled only when TargetBaseURL is set;
	// otherwise they persist as written but compile to nothing.
	//
	// TargetType selects how the target's platform is determined: "" and "auto"
	// resolve it by matching the target URL against the configured AnyRouter
	// entrances / CPA upstream (auto), while "anyrouter"/"cpa"/"generic" force it.
	// An empty value is left empty (never materialized to "auto") so the GET view
	// and the on-disk file keep the same shape.
	TargetType string `json:"target_type"`
	// TargetCAPath trusts a self-signed CA PEM bundle for the target's TLS.
	TargetCAPath string `json:"target_ca_path"`
	// TargetInsecureSkipVerify skips TLS verification for the target. It is a
	// plain bool: the absent/zero value is false ⇒ verify, the safe default. It is
	// mutually exclusive with TargetCAPath (enforced in
	// buildTargetTLSConfig).
	TargetInsecureSkipVerify bool `json:"target_insecure_skip_verify"`
}

type appConfig struct {
	configPath string
	runtime    *runtimeConfig
	table      *routingTable
}

type compiledRuntimeConfig struct {
	runtime            *runtimeConfig
	anyRouterUpstreams []*url.URL
	cliproxyUpstream   *url.URL
	cliproxyTLSConfig  *tls.Config
	classifierTarget   *url.URL
	// classifierTargetType is the resolved platform of classifierTarget, consumed
	// by routing to pick the classifier fix by platform⊗model family.
	// It stays platformGeneric (the zero value) when no global target is set.
	classifierTargetType classifierPlatform
	// classifierTargetTLS is the TLS config for the global classifier target
	// client (nil ⇒ verify against the system roots). It is nil when no global
	// target is set.
	classifierTargetTLS *tls.Config
}

func validateRuntimeConfig(cfg *runtimeConfig) error {
	_, err := compileRuntimeConfig(cfg)
	return err
}

func compileRuntimeConfig(cfg *runtimeConfig) (*compiledRuntimeConfig, error) {
	normalized := normalizeRuntimeConfig(cfg)
	if err := validateListenAddr(normalized.ListenAddr); err != nil {
		return nil, fmt.Errorf("listen_addr: %w", err)
	}
	// normalizeRuntimeConfig already mapped 0/absent to defaultMaxLogBytes, so the
	// only value that reaches <= 0 here is a negative cap, which is illegal
	// (mirrors logging.go's configuredMaxLogBytes <= 0 guard).
	if normalized.LogMaxBytes <= 0 {
		return nil, fmt.Errorf("log_max_bytes: must be a positive integer (bytes), got %d", normalized.LogMaxBytes)
	}
	anyRouterUpstreams, err := parseUpstreamEntries(normalized.AnyRouter.Entrances)
	if err != nil {
		return nil, fmt.Errorf("anyrouter.entrances: %w", err)
	}
	if err := validateAccountLabels(normalized.AnyRouter.Accounts); err != nil {
		return nil, fmt.Errorf("anyrouter.accounts: %w", err)
	}
	cliproxyUpstream, err := parseUpstream(normalized.CPA.Upstream)
	if err != nil {
		return nil, fmt.Errorf("cpa.upstream: %w", err)
	}
	cliproxyTLSConfig, err := buildCliproxyTLSConfig(normalized.CPA.CAPath)
	if err != nil {
		return nil, fmt.Errorf("cpa.ca_path: %w", err)
	}

	var classifierTarget *url.URL
	var classifierTargetType classifierPlatform
	var classifierTargetTLS *tls.Config
	if normalized.Classifier.TargetBaseURL != "" {
		classifierTarget, err = parseUpstream(normalized.Classifier.TargetBaseURL)
		if err != nil {
			return nil, fmt.Errorf("classifier.target_base_url: %w", err)
		}
		// Validate target_type and resolve it to a concrete platform. The "auto"
		// resolver needs the already-parsed AnyRouter entrances / CPA upstream
		// above. This whole block is skipped when no global target is set,
		// so an invalid target_type or a ca+insecure conflict in a config that has
		// no target_base_url is intentionally not an error.
		classifierTargetType, err = resolveConfiguredClassifierPlatform(
			normalized.Classifier.TargetType, classifierTarget, anyRouterUpstreams, cliproxyUpstream)
		if err != nil {
			return nil, err
		}
		// buildTargetTLSConfig returns context-free errors; add the field
		// context here. The conflict / bad-CA errors already name the offending
		// field, so a single "classifier:" prefix avoids a double prefix.
		classifierTargetTLS, err = buildTargetTLSConfig(
			normalized.Classifier.TargetInsecureSkipVerify, normalized.Classifier.TargetCAPath)
		if err != nil {
			return nil, fmt.Errorf("classifier: %w", err)
		}
	}

	return &compiledRuntimeConfig{
		runtime:              normalized,
		anyRouterUpstreams:   anyRouterUpstreams,
		cliproxyUpstream:     cliproxyUpstream,
		cliproxyTLSConfig:    cliproxyTLSConfig,
		classifierTarget:     classifierTarget,
		classifierTargetType: classifierTargetType,
		classifierTargetTLS:  classifierTargetTLS,
	}, nil
}

func normalizeRuntimeConfig(cfg *runtimeConfig) *runtimeConfig {
	if cfg == nil {
		cfg = &runtimeConfig{}
	}
	out := cloneRuntimeConfig(cfg)
	if out.Enabled == nil {
		// Default the global switch to on when absent/null (back-compat with configs
		// written before the field existed), mirroring the slice nil→[] defaults below.
		enabled := true
		out.Enabled = &enabled
	}
	out.ListenAddr = strings.TrimSpace(out.ListenAddr)
	if out.ListenAddr == "" {
		out.ListenAddr = defaultListenAddr
	}

	// Default the log cap to defaultMaxLogBytes when absent/zero (mirrors the
	// listen_addr/Enabled defaults; the on-disk file is written from the
	// normalized config, so the default persists). 0 is the unset sentinel —
	// back-compat: a config written before this field omits the key and decodes
	// to 0. A negative value is left as-is for compileRuntimeConfig to reject.
	if out.LogMaxBytes == 0 {
		out.LogMaxBytes = defaultMaxLogBytes
	}

	out.AnyRouter.Entrances = trimStringSlice(out.AnyRouter.Entrances)
	if len(out.AnyRouter.Entrances) == 0 {
		out.AnyRouter.Entrances = trimStringSlice(strings.Split(defaultAnyRouterUpstreamURLs, ","))
	}
	if out.AnyRouter.Accounts == nil {
		out.AnyRouter.Accounts = []accountEntry{}
	}
	for i := range out.AnyRouter.Accounts {
		out.AnyRouter.Accounts[i].Label = strings.TrimSpace(out.AnyRouter.Accounts[i].Label)
		out.AnyRouter.Accounts[i].Key = strings.TrimSpace(out.AnyRouter.Accounts[i].Key)
	}
	fillBlankAccountLabels(out.AnyRouter.Accounts)

	out.CPA.Upstream = strings.TrimSpace(out.CPA.Upstream)
	if out.CPA.Upstream == "" {
		out.CPA.Upstream = defaultCliproxyUpstreamURL
	}
	out.CPA.Key = strings.TrimSpace(out.CPA.Key)
	out.CPA.CAPath = strings.TrimSpace(out.CPA.CAPath)

	out.Classifier.TargetBaseURL = strings.TrimSpace(out.Classifier.TargetBaseURL)
	out.Classifier.TargetKey = strings.TrimSpace(out.Classifier.TargetKey)
	out.Classifier.ModelOverride = strings.TrimSpace(out.Classifier.ModelOverride)
	// Trim but do NOT default target_type: an empty value stays empty (it means
	// "auto", resolved at compile time). Materializing "" to "auto" here would
	// drift the GET view against the on-disk file. target_insecure_skip_verify
	// is a plain bool with no normalization.
	out.Classifier.TargetType = strings.TrimSpace(out.Classifier.TargetType)
	out.Classifier.TargetCAPath = strings.TrimSpace(out.Classifier.TargetCAPath)
	return out
}

// fillBlankAccountLabels assigns the next free "acct-N" label to every account
// that has a key but no label, mirroring the admin UI's nextAcctLabel(). The
// counter starts past the highest existing acct-N suffix so a generated label can
// never collide with an explicit one (compileRuntimeConfig then rejects any
// remaining duplicate). Labels are assumed already trimmed by the caller. An
// account with no key is left untouched: it carries no rotation weight
// (newAccountPool skips an empty-key entry), so there is nothing to name.
func fillBlankAccountLabels(accounts []accountEntry) {
	max := 0
	for _, a := range accounts {
		if n, ok := parseAcctLabel(a.Label); ok && n > max {
			max = n
		}
	}
	for i := range accounts {
		if accounts[i].Label == "" && accounts[i].Key != "" {
			max++
			accounts[i].Label = "acct-" + strconv.Itoa(max)
		}
	}
}

// parseAcctLabel reports the numeric suffix N of an "acct-N" label, where N is a
// non-empty run of ASCII digits with no sign — the exact shape the admin UI's
// /^acct-(\d+)$/ matches. A suffix too large for an int (Atoi overflow) reports
// not-a-match, so it simply does not advance the autofill counter.
func parseAcctLabel(label string) (int, bool) {
	const prefix = "acct-"
	digits, ok := strings.CutPrefix(label, prefix)
	if !ok || digits == "" {
		return 0, false
	}
	for _, c := range digits {
		if c < '0' || c > '9' {
			return 0, false
		}
	}
	n, err := strconv.Atoi(digits)
	if err != nil {
		return 0, false
	}
	return n, true
}

// validateAccountLabels rejects duplicate non-empty account labels. Labels key
// the GET /admin/status health snapshot (accountStatus.Label) and the admin UI's
// per-account health pills (matched by label), so two accounts sharing a label
// would alias each other's health; uniqueness keeps that mapping one-to-one.
// Empty labels are not compared: normalizeRuntimeConfig has already given every
// keyed account an acct-N label, and an account with neither label nor key is
// dropped from the rotation pool, so a residual empty label names nothing.
func validateAccountLabels(accounts []accountEntry) error {
	seen := make(map[string]struct{}, len(accounts))
	for _, a := range accounts {
		if a.Label == "" {
			continue
		}
		if _, dup := seen[a.Label]; dup {
			return fmt.Errorf("duplicate account label %q", a.Label)
		}
		seen[a.Label] = struct{}{}
	}
	return nil
}

func cloneRuntimeConfig(cfg *runtimeConfig) *runtimeConfig {
	if cfg == nil {
		return nil
	}
	out := *cfg
	out.AnyRouter.Entrances = append([]string(nil), cfg.AnyRouter.Entrances...)
	// Use a non-nil empty base so a normalized empty account list (set to
	// []accountEntry{} by normalizeRuntimeConfig) survives the clone as [] rather
	// than collapsing to nil. Otherwise the live runtime served by GET
	// /admin/config would marshal "accounts":null while the on-disk file (written
	// from the normalized compiled config) marshals "accounts":[] — an
	// observable GET-vs-disk shape mismatch.
	out.AnyRouter.Accounts = append([]accountEntry{}, cfg.AnyRouter.Accounts...)
	// Deep-copy the Enabled pointer so a hot-swapped config never aliases the
	// caller's *bool (an atomic state swap must not share mutable storage with the
	// previous live config).
	if cfg.Enabled != nil {
		enabled := *cfg.Enabled
		out.Enabled = &enabled
	}
	return &out
}

func trimStringSlice(values []string) []string {
	out := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value != "" {
			out = append(out, value)
		}
	}
	return out
}
