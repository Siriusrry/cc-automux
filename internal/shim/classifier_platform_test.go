package shim

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func mustParseURL(t *testing.T, raw string) *url.URL {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("url.Parse(%q): %v", raw, err)
	}
	return u
}

// compileClassifier compiles a minimal-but-valid runtime config carrying the
// given classifier block, so config compilation tests can assert the compiled platform + TLS
// outputs without restating the rest of the config each time. The AnyRouter
// entrances and CPA upstream here are the fixed reference set the auto matrix
// matches against.
func compileClassifier(t *testing.T, c classifierRuntimeConfig) (*compiledRuntimeConfig, error) {
	t.Helper()
	return compileRuntimeConfig(&runtimeConfig{
		ListenAddr: "127.0.0.1:8765",
		AnyRouter:  anyRouterRuntimeConfig{Entrances: []string{"https://anyrouter.top", "https://a-ocnfniawgw.cn-shanghai.fcapp.run"}},
		CPA:        cpaRuntimeConfig{Upstream: "https://127.0.0.1:8317"},
		Classifier: c,
	})
}

func platformName(p classifierPlatform) string {
	switch p {
	case platformGeneric:
		return "generic"
	case platformAnyRouter:
		return "anyrouter"
	case platformCPA:
		return "cpa"
	default:
		return "unknown"
	}
}

// TestCanonicalUpstreamKeyNormalization pins exactly which URL differences the
// auto matcher folds (scheme/host case, default port, trailing slashes) and
// which it keeps distinct (http vs https, localhost vs 127.0.0.1, distinct
// ports, distinct paths). These are the equivalences the platform auto-detect
// relies on.
func TestCanonicalUpstreamKeyNormalization(t *testing.T) {
	equal := []struct{ a, b string }{
		{"https://h", "https://h:443"},                     // https default port
		{"http://h", "http://h:80"},                        // http default port
		{"https://h", "https://h/"},                        // trailing slash on empty path
		{"https://h", "https://H"},                         // host case-folding
		{"https://h/v1", "https://h/v1/"},                  // trailing slash on a real path
		{"https://h:8317", "https://h:8317/"},              // explicit port + trailing slash
		{"https://Any.Example", "https://any.example:443"}, // combined
	}
	for _, p := range equal {
		if ka, kb := canonicalUpstreamKey(mustParseURL(t, p.a)), canonicalUpstreamKey(mustParseURL(t, p.b)); ka != kb {
			t.Errorf("canonicalUpstreamKey: %q (%s) != %q (%s), want equal", p.a, ka, p.b, kb)
		}
	}

	distinct := []struct{ a, b string }{
		{"http://h", "https://h"},                  // scheme is NOT folded
		{"https://localhost", "https://127.0.0.1"}, // host alias is NOT folded
		{"https://h:8317", "https://h:8318"},       // distinct explicit ports
		{"https://h", "https://h:8317"},            // default vs explicit port
		{"https://h/v1", "https://h/v2"},           // distinct paths
		{"https://a.example", "https://b.example"}, // distinct hosts
	}
	for _, p := range distinct {
		if ka, kb := canonicalUpstreamKey(mustParseURL(t, p.a)), canonicalUpstreamKey(mustParseURL(t, p.b)); ka == kb {
			t.Errorf("canonicalUpstreamKey: %q and %q both = %q, want distinct", p.a, p.b, ka)
		}
	}

	if got := canonicalUpstreamKey(nil); got != "" {
		t.Errorf("canonicalUpstreamKey(nil) = %q, want \"\"", got)
	}
}

// TestResolveClassifierPlatformAuto exercises the auto matcher against the fixed
// reference upstreams: matches must hold across default-port / trailing-slash /
// host-case variants, and the deliberate non-equivalences (http≠https,
// localhost≠127.0.0.1) must fall through to generic.
func TestResolveClassifierPlatformAuto(t *testing.T) {
	anyRouter := []*url.URL{
		mustParseURL(t, "https://anyrouter.top"),
		mustParseURL(t, "https://a-ocnfniawgw.cn-shanghai.fcapp.run"),
	}
	cpa := mustParseURL(t, "https://127.0.0.1:8317")

	cases := []struct {
		name   string
		target string
		want   classifierPlatform
	}{
		{"first anyrouter entrance", "https://anyrouter.top", platformAnyRouter},
		{"second anyrouter entrance", "https://a-ocnfniawgw.cn-shanghai.fcapp.run", platformAnyRouter},
		{"anyrouter trailing slash", "https://anyrouter.top/", platformAnyRouter},
		{"anyrouter host case", "https://ANYROUTER.top", platformAnyRouter},
		{"anyrouter explicit default port", "https://anyrouter.top:443", platformAnyRouter},
		{"cpa upstream", "https://127.0.0.1:8317", platformCPA},
		{"cpa trailing slash", "https://127.0.0.1:8317/", platformCPA},
		{"cpa wrong scheme http", "http://127.0.0.1:8317", platformGeneric},
		{"cpa localhost alias", "https://localhost:8317", platformGeneric},
		{"cpa wrong port", "https://127.0.0.1:8318", platformGeneric},
		{"unrelated host", "https://other.example", platformGeneric},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := resolveClassifierPlatform(mustParseURL(t, tc.target), anyRouter, cpa)
			if got != tc.want {
				t.Fatalf("resolveClassifierPlatform(%q) = %s, want %s", tc.target, platformName(got), platformName(tc.want))
			}
		})
	}

	if got := resolveClassifierPlatform(nil, anyRouter, cpa); got != platformGeneric {
		t.Fatalf("resolveClassifierPlatform(nil) = %s, want generic", platformName(got))
	}
}

// TestResolveConfiguredClassifierPlatform covers the config-string → platform
// mapping: explicit values override auto detection, ""/auto fall through to the
// URL matcher, and an unknown string is a configuration error.
func TestResolveConfiguredClassifierPlatform(t *testing.T) {
	anyRouter := []*url.URL{mustParseURL(t, "https://anyrouter.top")}
	cpa := mustParseURL(t, "https://127.0.0.1:8317")
	generic := mustParseURL(t, "https://other.example")

	// Explicit type overrides the URL: a generic URL forced to anyrouter/cpa, and
	// an anyrouter URL forced to generic.
	cases := []struct {
		name       string
		targetType string
		target     *url.URL
		want       classifierPlatform
	}{
		{"empty resolves auto match", "", mustParseURL(t, "https://anyrouter.top"), platformAnyRouter},
		{"auto resolves match", "auto", mustParseURL(t, "https://anyrouter.top"), platformAnyRouter},
		{"empty auto no match -> generic", "", generic, platformGeneric},
		{"explicit anyrouter overrides generic url", "anyrouter", generic, platformAnyRouter},
		{"explicit cpa overrides generic url (public cpa)", "cpa", generic, platformCPA},
		{"explicit generic overrides anyrouter url", "generic", mustParseURL(t, "https://anyrouter.top"), platformGeneric},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := resolveConfiguredClassifierPlatform(tc.targetType, tc.target, anyRouter, cpa)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.want {
				t.Fatalf("got %s, want %s", platformName(got), platformName(tc.want))
			}
		})
	}

	if _, err := resolveConfiguredClassifierPlatform("bogus", generic, anyRouter, cpa); err == nil {
		t.Fatal("unknown target_type should be a configuration error")
	} else if !strings.Contains(err.Error(), "classifier.target_type:") {
		t.Fatalf("error %q missing classifier.target_type: prefix", err)
	}
}

// TestCompileClassifierTargetPlatformAndTLS asserts the compiled outputs for a
// configured global target across explicit types, auto detection, and the three
// TLS shapes (system roots / RootCAs / skip-verify).
func TestCompileClassifierTargetPlatformAndTLS(t *testing.T) {
	caPath := writeTestCACert(t)

	cases := []struct {
		name         string
		classifier   classifierRuntimeConfig
		wantPlatform classifierPlatform
		checkTLS     func(t *testing.T, cfg *compiledRuntimeConfig)
	}{
		{
			name:         "auto matches anyrouter entrance",
			classifier:   classifierRuntimeConfig{TargetBaseURL: "https://anyrouter.top"},
			wantPlatform: platformAnyRouter,
			checkTLS:     wantNilTLS,
		},
		{
			name:         "empty type behaves as auto (cpa upstream)",
			classifier:   classifierRuntimeConfig{TargetBaseURL: "https://127.0.0.1:8317", TargetType: ""},
			wantPlatform: platformCPA,
			checkTLS:     wantNilTLS,
		},
		{
			name:         "auto no match -> generic",
			classifier:   classifierRuntimeConfig{TargetBaseURL: "https://public.example", TargetType: "auto"},
			wantPlatform: platformGeneric,
			checkTLS:     wantNilTLS,
		},
		{
			name:         "explicit generic, system roots",
			classifier:   classifierRuntimeConfig{TargetBaseURL: "https://public.example", TargetType: "generic"},
			wantPlatform: platformGeneric,
			checkTLS:     wantNilTLS,
		},
		{
			name:         "explicit anyrouter override on a generic url",
			classifier:   classifierRuntimeConfig{TargetBaseURL: "https://public.example", TargetType: "anyrouter"},
			wantPlatform: platformAnyRouter,
			checkTLS:     wantNilTLS,
		},
		{
			name:         "explicit cpa (public) with skip-verify",
			classifier:   classifierRuntimeConfig{TargetBaseURL: "https://public-cpa.example", TargetType: "cpa", TargetInsecureSkipVerify: true},
			wantPlatform: platformCPA,
			checkTLS:     wantSkipVerifyTLS,
		},
		{
			name:         "explicit cpa with pinned CA",
			classifier:   classifierRuntimeConfig{TargetBaseURL: "https://public-cpa.example", TargetType: "cpa", TargetCAPath: caPath},
			wantPlatform: platformCPA,
			checkTLS:     wantRootCAsTLS,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			compiled, err := compileClassifier(t, tc.classifier)
			if err != nil {
				t.Fatalf("compileRuntimeConfig returned error: %v", err)
			}
			if compiled.classifierTarget == nil {
				t.Fatal("classifierTarget is nil, want parsed target")
			}
			if compiled.classifierTargetType != tc.wantPlatform {
				t.Fatalf("classifierTargetType = %s, want %s", platformName(compiled.classifierTargetType), platformName(tc.wantPlatform))
			}
			tc.checkTLS(t, compiled)
		})
	}
}

func wantNilTLS(t *testing.T, cfg *compiledRuntimeConfig) {
	t.Helper()
	if cfg.classifierTargetTLS != nil {
		t.Fatalf("classifierTargetTLS = %+v, want nil (system-root verification)", cfg.classifierTargetTLS)
	}
}

func wantSkipVerifyTLS(t *testing.T, cfg *compiledRuntimeConfig) {
	t.Helper()
	if cfg.classifierTargetTLS == nil || !cfg.classifierTargetTLS.InsecureSkipVerify {
		t.Fatalf("classifierTargetTLS = %+v, want InsecureSkipVerify", cfg.classifierTargetTLS)
	}
	if cfg.classifierTargetTLS.RootCAs != nil {
		t.Fatal("classifierTargetTLS.RootCAs should be nil when skipping verification")
	}
}

func wantRootCAsTLS(t *testing.T, cfg *compiledRuntimeConfig) {
	t.Helper()
	if cfg.classifierTargetTLS == nil || cfg.classifierTargetTLS.RootCAs == nil {
		t.Fatalf("classifierTargetTLS = %+v, want populated RootCAs", cfg.classifierTargetTLS)
	}
	if cfg.classifierTargetTLS.InsecureSkipVerify {
		t.Fatal("classifierTargetTLS.InsecureSkipVerify should be false when a CA is pinned")
	}
}

// TestCompileClassifierTargetInvalidTypeRejected proves an unknown target_type
// fails the POST-equivalent paths (validateRuntimeConfig + compileRuntimeConfig)
// without producing a compiled config, while every valid value compiles.
func TestCompileClassifierTargetInvalidTypeRejected(t *testing.T) {
	bad := &runtimeConfig{
		ListenAddr: "127.0.0.1:8765",
		AnyRouter:  anyRouterRuntimeConfig{Entrances: []string{"https://anyrouter.top"}},
		CPA:        cpaRuntimeConfig{Upstream: "https://127.0.0.1:8317"},
		Classifier: classifierRuntimeConfig{TargetBaseURL: "https://public.example", TargetType: "bogus"},
	}
	compiled, err := compileRuntimeConfig(bad)
	if err == nil {
		t.Fatal("invalid target_type should fail compile")
	}
	if compiled != nil {
		t.Fatal("compileRuntimeConfig should not return a half-built config on error")
	}
	if !strings.Contains(err.Error(), "classifier.target_type:") {
		t.Fatalf("error %q missing classifier.target_type: prefix", err)
	}
	// POST goes through validateRuntimeConfig, which must reject identically.
	if err := validateRuntimeConfig(bad); err == nil {
		t.Fatal("validateRuntimeConfig should reject invalid target_type")
	}

	for _, valid := range []string{"", "auto", "anyrouter", "cpa", "generic"} {
		ok := *bad
		ok.Classifier.TargetType = valid
		if err := validateRuntimeConfig(&ok); err != nil {
			t.Fatalf("validateRuntimeConfig(target_type=%q) = %v, want accepted", valid, err)
		}
	}
}

// TestCompileClassifierTargetTLSErrorsWrapped proves the TLS builder context-free TLS
// errors are surfaced with a single "classifier:" prefix (no double prefix) and
// that compile fails without a half-built config.
func TestCompileClassifierTargetTLSErrorsWrapped(t *testing.T) {
	t.Run("ca and insecure conflict", func(t *testing.T) {
		caPath := writeTestCACert(t)
		compiled, err := compileClassifier(t, classifierRuntimeConfig{
			TargetBaseURL:            "https://public.example",
			TargetCAPath:             caPath,
			TargetInsecureSkipVerify: true,
		})
		if err == nil {
			t.Fatal("ca_path + insecure should fail compile")
		}
		if compiled != nil {
			t.Fatal("compile should not return a half-built config on TLS error")
		}
		if !strings.Contains(err.Error(), "classifier:") || !strings.Contains(err.Error(), "mutually exclusive") {
			t.Fatalf("error = %q, want classifier:-prefixed mutually-exclusive error", err)
		}
		if strings.Contains(err.Error(), "classifier: classifier:") {
			t.Fatalf("error = %q has a doubled prefix", err)
		}
	})

	t.Run("bad CA pem", func(t *testing.T) {
		badCA := filepath.Join(t.TempDir(), "bad.pem")
		if err := os.WriteFile(badCA, []byte("not a pem"), 0o600); err != nil {
			t.Fatalf("write bad ca: %v", err)
		}
		_, err := compileClassifier(t, classifierRuntimeConfig{
			TargetBaseURL: "https://public.example",
			TargetCAPath:  badCA,
		})
		if err == nil {
			t.Fatal("bad CA pem should fail compile")
		}
		if !strings.Contains(err.Error(), "classifier:") || !strings.Contains(err.Error(), "no certificates found") {
			t.Fatalf("error = %q, want classifier:-prefixed no-certificates error", err)
		}
	})
}

// TestCompileClassifierLazyWithoutTarget pins lazy-target validation rule: when no global target is set,
// the three classifier-target fields are lazy — an invalid target_type or a ca+insecure conflict
// must NOT fail compile, and the compiled platform/TLS stay at their inert
// zero/nil values.
func TestCompileClassifierLazyWithoutTarget(t *testing.T) {
	caPath := writeTestCACert(t)
	compiled, err := compileClassifier(t, classifierRuntimeConfig{
		TargetBaseURL:            "", // no global target
		TargetType:               "bogus",
		TargetCAPath:             caPath,
		TargetInsecureSkipVerify: true, // conflicts with CA, but must be ignored
	})
	if err != nil {
		t.Fatalf("lazy fields without a target must not fail compile, got: %v", err)
	}
	if compiled.classifierTarget != nil {
		t.Fatalf("classifierTarget = %v, want nil with no target", compiled.classifierTarget)
	}
	if compiled.classifierTargetType != platformGeneric {
		t.Fatalf("classifierTargetType = %s, want generic (inert) with no target", platformName(compiled.classifierTargetType))
	}
	if compiled.classifierTargetTLS != nil {
		t.Fatalf("classifierTargetTLS = %+v, want nil with no target", compiled.classifierTargetTLS)
	}
}

// TestNormalizeClassifierTargetFields pins stable-shape rule: an empty target_type is trimmed but
// never materialized to "auto", and target_type / target_ca_path are trimmed.
func TestNormalizeClassifierTargetFields(t *testing.T) {
	out := normalizeRuntimeConfig(&runtimeConfig{
		Classifier: classifierRuntimeConfig{
			TargetType:   "   ",
			TargetCAPath: "  /tmp/ca.pem  ",
		},
	})
	if out.Classifier.TargetType != "" {
		t.Fatalf("TargetType = %q, want \"\" (empty must NOT become \"auto\")", out.Classifier.TargetType)
	}
	if out.Classifier.TargetCAPath != "/tmp/ca.pem" {
		t.Fatalf("TargetCAPath = %q, want trimmed", out.Classifier.TargetCAPath)
	}

	// A real value is trimmed but otherwise preserved.
	out = normalizeRuntimeConfig(&runtimeConfig{Classifier: classifierRuntimeConfig{TargetType: "  cpa  "}})
	if out.Classifier.TargetType != "cpa" {
		t.Fatalf("TargetType = %q, want \"cpa\"", out.Classifier.TargetType)
	}
}

// TestReadRuntimeConfigBackCompatMissingClassifierTargetFields proves an older config file that
// predates the three classifier-target fields still decodes under DisallowUnknownFields (the
// fields are now in the struct, so omitted keys are fine) and that the missing
// target_type decodes to "" ≡ auto.
func TestReadRuntimeConfigBackCompatMissingClassifierTargetFields(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	old := `{
		"listen_addr": "127.0.0.1:8765",
		"anyrouter": {"entrances": ["https://anyrouter.top"], "accounts": []},
		"cpa": {"upstream": "https://127.0.0.1:8317", "key": "", "ca_path": ""},
		"classifier": {"target_base_url": "", "target_key": "", "model_override": ""}
	}`
	if err := os.WriteFile(path, []byte(old), 0o600); err != nil {
		t.Fatalf("write old config: %v", err)
	}
	cfg, err := readRuntimeConfig(path)
	if err != nil {
		t.Fatalf("old config without classifier-target fields must decode, got: %v", err)
	}
	if cfg.Classifier.TargetType != "" || cfg.Classifier.TargetCAPath != "" || cfg.Classifier.TargetInsecureSkipVerify {
		t.Fatalf("classifier-target fields should default to zero, got %+v", cfg.Classifier)
	}
	// Empty target_type ≡ auto: with a target pointing at an anyrouter entrance it
	// resolves to platformAnyRouter.
	cfg.Classifier.TargetBaseURL = "https://anyrouter.top"
	compiled, err := compileRuntimeConfig(cfg)
	if err != nil {
		t.Fatalf("compile after back-compat read: %v", err)
	}
	if compiled.classifierTargetType != platformAnyRouter {
		t.Fatalf("empty target_type did not behave as auto: got %s", platformName(compiled.classifierTargetType))
	}
}

// TestClassifierTargetFieldsRoundTripShapeConsistent pins that the GET /admin/config
// view and the persisted on-disk file carry the three classifier-target fields with the same
// shape (present, not omitted) — no "" vs omitted drift. It mirrors the accounts
// empty-slice shape test.
func TestClassifierTargetFieldsRoundTripShapeConsistent(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.json")
	server := newTestAdminServer(t, baseAdminConfig(), configPath)

	getRaw := func() string {
		t.Helper()
		rec := httptest.NewRecorder()
		server.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/admin/config", nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("GET /admin/config status = %d, want 200", rec.Code)
		}
		return rec.Body.String()
	}

	// GET (compact JSON) must carry all three keys, with empty target_type as ""
	// (not omitted, not "auto").
	before := getRaw()
	for _, want := range []string{`"target_type":""`, `"target_ca_path":""`, `"target_insecure_skip_verify":false`} {
		if !strings.Contains(before, want) {
			t.Fatalf("GET /admin/config missing %s: %s", want, before)
		}
	}

	// Round-trip the live view back through POST so disk is written from it.
	if rec := postAdminConfig(t, server, before); rec.Code != http.StatusOK {
		t.Fatalf("POST round-trip status = %d (%s)", rec.Code, rec.Body.String())
	}

	// The persisted file (indented JSON) must carry the same keys/shape.
	diskBytes, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("read persisted config: %v", err)
	}
	disk := string(diskBytes)
	for _, want := range []string{`"target_type": ""`, `"target_ca_path": ""`, `"target_insecure_skip_verify": false`} {
		if !strings.Contains(disk, want) {
			t.Fatalf("persisted config missing %s: %s", want, disk)
		}
	}

	// GET after the round-trip must remain consistent.
	if after := getRaw(); !strings.Contains(after, `"target_type":""`) {
		t.Fatalf("GET after round-trip lost the empty target_type shape: %s", after)
	}
}

// TestClassifierTargetPopulatedRoundTrip proves populated classifier-target values survive a
// GET→POST→disk round-trip unchanged (decode-level, format-independent).
func TestClassifierTargetPopulatedRoundTrip(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.json")
	cfg := baseAdminConfig()
	cfg.Classifier = classifierRuntimeConfig{
		TargetBaseURL:            "https://public-cpa.example",
		TargetKey:                "sk-target",
		TargetType:               "cpa",
		TargetInsecureSkipVerify: true,
	}
	server := newTestAdminServer(t, cfg, configPath)

	rec := httptest.NewRecorder()
	server.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/admin/config", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET status = %d", rec.Code)
	}
	if post := postAdminConfig(t, server, rec.Body.String()); post.Code != http.StatusOK {
		t.Fatalf("POST round-trip status = %d (%s)", post.Code, post.Body.String())
	}
	persisted, err := readRuntimeConfig(configPath)
	if err != nil {
		t.Fatalf("read persisted: %v", err)
	}
	got := persisted.Classifier
	if got.TargetBaseURL != "https://public-cpa.example" || got.TargetType != "cpa" || !got.TargetInsecureSkipVerify || got.TargetKey != "sk-target" {
		t.Fatalf("round-trip lost classifier-target values: %+v", got)
	}
}
