package shim

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net"
	"net/http"
	"os"
	"strings"
	"time"
)

// buildCliproxyTLSConfig trusts the given PEM CA file if provided, otherwise skips
// verification (the upstream is a fixed loopback CLIProxyAPI with a self-signed certificate).
func buildCliproxyTLSConfig(caPath string) (*tls.Config, error) {
	caPath = strings.TrimSpace(caPath)
	if caPath == "" {
		return &tls.Config{InsecureSkipVerify: true}, nil
	}
	pem, err := os.ReadFile(caPath)
	if err != nil {
		return nil, err
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pem) {
		return nil, fmt.Errorf("no certificates found in %s", caPath)
	}
	return &tls.Config{RootCAs: pool}, nil
}

// buildTargetTLSConfig builds the TLS config for the generalized classifier
// target client. Its defaults are deliberately the safe-by-default mirror
// image of buildCliproxyTLSConfig above: here an empty caPath means "verify
// against the system roots" (return nil), NOT "skip verification". The CPA helper
// can safely default to skip because its upstream is a fixed loopback CLIProxyAPI
// with a self-signed certificate, but a general classifier target may be a public
// endpoint, so silently skipping verification there would be a footgun.
//
// Precedence (the conflict check is first on purpose):
//   - caPath set AND insecure -> error (contradictory; reject loudly rather
//     than silently letting one win).
//   - caPath set              -> trust exactly that PEM CA bundle (RootCAs).
//   - insecure                -> InsecureSkipVerify (self-signed / dev target).
//   - neither (the default)   -> nil: verify against the system roots.
func buildTargetTLSConfig(insecure bool, caPath string) (*tls.Config, error) {
	caPath = strings.TrimSpace(caPath)
	if caPath != "" && insecure {
		return nil, fmt.Errorf("target_ca_path and target_insecure_skip_verify are mutually exclusive")
	}
	if caPath != "" {
		pem, err := os.ReadFile(caPath)
		if err != nil {
			return nil, err
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("no certificates found in %s", caPath)
		}
		return &tls.Config{RootCAs: pool}, nil
	}
	if insecure {
		return &tls.Config{InsecureSkipVerify: true}, nil
	}
	return nil, nil
}

func newCliproxyHTTPClient(tlsConfig *tls.Config) *http.Client {
	return &http.Client{Transport: &http.Transport{
		// Upstream is a fixed loopback CLIProxyAPI; do not route via a proxy.
		Proxy: nil,
		DialContext: (&net.Dialer{
			Timeout:   30 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		ForceAttemptHTTP2:     true,
		DisableCompression:    true,
		TLSClientConfig:       tlsConfig,
		MaxIdleConns:          100,
		MaxIdleConnsPerHost:   20,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   30 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
	}}
}

// newTargetHTTPClient builds the HTTP client used for the generalized classifier
// target and is the single source of truth for the AnyRouter-class transport
// tuning. It injects the caller's TLS config so the target can be a public
// endpoint (tlsConfig == nil ⇒ verify against the system roots), a custom-CA
// endpoint, or a skip-verify self-signed/dev endpoint — see buildTargetTLSConfig.
//
// DisableCompression stays true: downstream classifier response reassembly reads
// the body bytes directly, so a gzip-compressed body would break gpt reassembly.
// Proxy stays ProxyFromEnvironment so a public CPA target can be reached
// through an environment proxy.
func newTargetHTTPClient(tlsConfig *tls.Config) *http.Client {
	return &http.Client{Transport: &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		// Dial/TLS timeouts bound how long a request stalls on a blackholed
		// entrance before failing over to the next one; response header and
		// body reads stay unbounded because LLM gateways can be slow to first
		// byte and misclassifying a slow entrance as dead would flap traffic.
		DialContext: (&net.Dialer{
			Timeout:   5 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		ForceAttemptHTTP2:     true,
		DisableCompression:    true,
		TLSClientConfig:       tlsConfig,
		MaxIdleConns:          100,
		MaxIdleConnsPerHost:   20,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
	}}
}

// newAnyRouterHTTPClient verifies TLS against the system roots (no custom CA, no
// skip): the AnyRouter entrances present publicly-trusted certificates. It is the
// degenerate, TLS-default case of newTargetHTTPClient — a nil TLS config makes the
// transport use the system root pool, identical to the prior hand-rolled client.
func newAnyRouterHTTPClient() *http.Client {
	return newTargetHTTPClient(nil)
}
