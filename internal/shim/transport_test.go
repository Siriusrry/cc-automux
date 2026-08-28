package shim

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// writeTestCACert generates a throwaway self-signed CA certificate, writes it as
// PEM into a temp file, and returns the path. It exercises the real PEM-loading
// path of buildTargetTLSConfig without depending on any committed fixture.
func writeTestCACert(t *testing.T) string {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "cc-auto-mode-shim test CA"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create certificate: %v", err)
	}
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	path := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(path, pemBytes, 0o600); err != nil {
		t.Fatalf("write ca file: %v", err)
	}
	return path
}

func TestBuildTargetTLSConfigDefaultVerifiesSystemRoots(t *testing.T) {
	cfg, err := buildTargetTLSConfig(false, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// nil config = the transport's default = verify against the system roots.
	// This is the deliberately-safe default that differs from buildCliproxyTLSConfig.
	if cfg != nil {
		t.Fatalf("expected nil tls.Config (system-root verification), got %+v", cfg)
	}
}

func TestBuildTargetTLSConfigWithCABuildsRootCAs(t *testing.T) {
	caPath := writeTestCACert(t)
	cfg, err := buildTargetTLSConfig(false, caPath)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg == nil {
		t.Fatal("expected non-nil tls.Config")
	}
	if cfg.RootCAs == nil {
		t.Error("RootCAs = nil, want a populated pool from the CA file")
	}
	if cfg.InsecureSkipVerify {
		t.Error("InsecureSkipVerify = true, want false when a CA is pinned")
	}
}

func TestBuildTargetTLSConfigInsecureSkipsVerify(t *testing.T) {
	cfg, err := buildTargetTLSConfig(true, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg == nil {
		t.Fatal("expected non-nil tls.Config")
	}
	if !cfg.InsecureSkipVerify {
		t.Error("InsecureSkipVerify = false, want true")
	}
	if cfg.RootCAs != nil {
		t.Error("RootCAs != nil, want nil when skipping verification")
	}
}

func TestBuildTargetTLSConfigMissingCAFileErrors(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "does-not-exist.pem")
	cfg, err := buildTargetTLSConfig(false, missing)
	if err == nil {
		t.Fatal("expected an error for a nonexistent CA file")
	}
	if cfg != nil {
		t.Errorf("expected nil config on error, got %+v", cfg)
	}
}

func TestBuildTargetTLSConfigBadPEMErrors(t *testing.T) {
	bad := filepath.Join(t.TempDir(), "bad.pem")
	if err := os.WriteFile(bad, []byte("not a pem certificate"), 0o600); err != nil {
		t.Fatalf("write bad pem: %v", err)
	}
	cfg, err := buildTargetTLSConfig(false, bad)
	if err == nil {
		t.Fatal("expected an error for a file with no valid certificates")
	}
	if cfg != nil {
		t.Errorf("expected nil config on error, got %+v", cfg)
	}
}

func TestBuildTargetTLSConfigCAAndInsecureConflict(t *testing.T) {
	// Use a path that does NOT exist: if the conflict check runs first (as it must,
	// TLS conflict rule), we get the "mutually exclusive" error without ever touching the file.
	caPath := filepath.Join(t.TempDir(), "unread.pem")
	cfg, err := buildTargetTLSConfig(true, caPath)
	if err == nil {
		t.Fatal("expected an error when both CA and insecure are set")
	}
	if cfg != nil {
		t.Errorf("expected nil config on conflict, got %+v", cfg)
	}
	if !strings.Contains(err.Error(), "mutually exclusive") {
		t.Errorf("error %q does not look like the TLS conflict rule conflict error; "+
			"the conflict check may not run before the file read", err)
	}
}

func TestBuildTargetTLSConfigCAPathTrimmed(t *testing.T) {
	// Whitespace-only caPath trims to empty: with insecure=false this is the
	// safe default (nil), and with insecure=true it is NOT a conflict (the trim
	// happens before the conflict check), so insecure wins.
	cfg, err := buildTargetTLSConfig(false, "   \t  ")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg != nil {
		t.Errorf("whitespace-only caPath should trim to empty (nil config), got %+v", cfg)
	}

	cfg, err = buildTargetTLSConfig(true, "   ")
	if err != nil {
		t.Fatalf("unexpected error: %v (whitespace caPath should not conflict with insecure)", err)
	}
	if cfg == nil || !cfg.InsecureSkipVerify {
		t.Errorf("expected InsecureSkipVerify config when caPath trims to empty, got %+v", cfg)
	}
}

func TestNewTargetHTTPClientInjectsTLSConfig(t *testing.T) {
	cfg := &tls.Config{InsecureSkipVerify: true}
	tr, ok := newTargetHTTPClient(cfg).Transport.(*http.Transport)
	if !ok {
		t.Fatal("transport is not *http.Transport")
	}
	if tr.TLSClientConfig != cfg {
		t.Errorf("TLSClientConfig = %p, want the injected config %p", tr.TLSClientConfig, cfg)
	}
	// Reassembly reads body bytes directly, so the target client must never
	// negotiate compression.
	if !tr.DisableCompression {
		t.Error("DisableCompression = false, want true")
	}
	// environment-proxy rule: a public CPA target may sit behind an environment proxy.
	if tr.Proxy == nil {
		t.Error("Proxy = nil, want non-nil (ProxyFromEnvironment)")
	}
}

func TestNewTargetHTTPClientNilTLSConfig(t *testing.T) {
	tr, ok := newTargetHTTPClient(nil).Transport.(*http.Transport)
	if !ok {
		t.Fatal("transport is not *http.Transport")
	}
	if tr.TLSClientConfig != nil {
		t.Errorf("TLSClientConfig = %+v, want nil (system-root verification)", tr.TLSClientConfig)
	}
	if !tr.DisableCompression {
		t.Error("DisableCompression = false, want true")
	}
	if tr.Proxy == nil {
		t.Error("Proxy = nil, want non-nil (ProxyFromEnvironment)")
	}
}

// TestNewAnyRouterHTTPClientTransportFields pins the AnyRouter transport tuning.
// newAnyRouterHTTPClient now delegates to newTargetHTTPClient(nil); this guard
// fails loudly if that delegation ever drifts the AnyRouter transport away from
// the original hand-rolled client.
//
// Scope: it pins the introspectable *http.Transport fields — ForceAttemptHTTP2,
// DisableCompression, MaxIdleConns, MaxIdleConnsPerHost, IdleConnTimeout,
// TLSHandshakeTimeout, ExpectContinueTimeout, and TLSClientConfig==nil — and
// asserts Proxy and DialContext are non-nil. The Dialer's Timeout (5s) and
// KeepAlive (30s) live inside the DialContext closure, which a *http.Transport
// cannot introspect, so those dial timeouts are NOT covered by this guard.
func TestNewAnyRouterHTTPClientTransportFields(t *testing.T) {
	tr, ok := newAnyRouterHTTPClient().Transport.(*http.Transport)
	if !ok {
		t.Fatal("transport is not *http.Transport")
	}
	if !tr.ForceAttemptHTTP2 {
		t.Error("ForceAttemptHTTP2 = false, want true")
	}
	if !tr.DisableCompression {
		t.Error("DisableCompression = false, want true")
	}
	if tr.MaxIdleConns != 100 {
		t.Errorf("MaxIdleConns = %d, want 100", tr.MaxIdleConns)
	}
	if tr.MaxIdleConnsPerHost != 20 {
		t.Errorf("MaxIdleConnsPerHost = %d, want 20", tr.MaxIdleConnsPerHost)
	}
	if tr.IdleConnTimeout != 90*time.Second {
		t.Errorf("IdleConnTimeout = %v, want 90s", tr.IdleConnTimeout)
	}
	if tr.TLSHandshakeTimeout != 10*time.Second {
		t.Errorf("TLSHandshakeTimeout = %v, want 10s", tr.TLSHandshakeTimeout)
	}
	if tr.ExpectContinueTimeout != 1*time.Second {
		t.Errorf("ExpectContinueTimeout = %v, want 1s", tr.ExpectContinueTimeout)
	}
	// Proxy and DialContext are funcs (not comparable with ==); assert non-nil.
	if tr.Proxy == nil {
		t.Error("Proxy = nil, want non-nil (ProxyFromEnvironment)")
	}
	if tr.DialContext == nil {
		t.Error("DialContext = nil, want non-nil")
	}
	// AnyRouter verifies against the system roots: no custom TLS config.
	if tr.TLSClientConfig != nil {
		t.Errorf("TLSClientConfig = %+v, want nil (system-root verification)", tr.TLSClientConfig)
	}
}
