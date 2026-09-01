package gateway

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/Siriusrry/cc-automux/internal/config"
	"github.com/Siriusrry/cc-automux/internal/provider"
)

var hopByHopHeaders = []string{
	"Connection",
	"Proxy-Connection",
	"Keep-Alive",
	"Proxy-Authenticate",
	"Proxy-Authorization",
	"Te",
	"Trailer",
	"Transfer-Encoding",
	"Upgrade",
}

func upstreamURL(item *provider.CompiledProvider, requestURL *url.URL) (*url.URL, error) {
	if item == nil || item.BaseURL == nil {
		return nil, errors.New("provider base URL is unavailable")
	}
	return targetUpstreamURL(&item.CompiledTarget, requestURL)
}

// targetUpstreamURL composes an upstream endpoint from a compiled target. The
// target's base URL is never interpreted as an endpoint; the protocol selects
// the fixed API path and the client's raw query is copied unchanged.
func targetUpstreamURL(target *provider.CompiledTarget, requestURL *url.URL) (*url.URL, error) {
	if target == nil || target.BaseURL == nil {
		return nil, errors.New("target base URL is unavailable")
	}
	suffix, err := protocolAPIPath(target.Protocol)
	if err != nil {
		return nil, err
	}
	result := *target.BaseURL
	escapedPrefix := result.EscapedPath()
	if strings.HasSuffix(escapedPrefix, "/") {
		escapedPrefix = strings.TrimSuffix(escapedPrefix, "/")
	}
	escapedPath := escapedPrefix + suffix
	decodedPath, err := url.PathUnescape(escapedPath)
	if err != nil {
		return nil, fmt.Errorf("compose upstream path: %w", err)
	}
	result.Path = decodedPath
	result.RawPath = escapedPath
	if result.EscapedPath() == result.Path {
		result.RawPath = ""
	}
	if requestURL != nil {
		result.RawQuery = requestURL.RawQuery
		result.ForceQuery = requestURL.ForceQuery
	} else {
		result.RawQuery = ""
		result.ForceQuery = false
	}
	result.Fragment = ""
	result.RawFragment = ""
	return &result, nil
}

func protocolAPIPath(protocolID string) (string, error) {
	switch protocolID {
	case config.ProtocolAnthropicMessages:
		return MessagesPath, nil
	case config.ProtocolOpenAIResponses:
		return ResponsesPath, nil
	case config.ProtocolOpenAICompatible:
		return ChatCompletionsPath, nil
	default:
		return "", fmt.Errorf("unsupported target protocol %q", protocolID)
	}
}

func prepareUpstreamHeaders(source http.Header, item *provider.CompiledProvider) (http.Header, error) {
	headers := source.Clone()
	if headers == nil {
		headers = make(http.Header)
	}
	removeHopByHop(headers)
	deleteHeaderFold(headers, "Content-Length")
	deleteHeaderFold(headers, "Host")
	if err := item.ApplyAuthHeaders(headers); err != nil {
		return nil, err
	}
	return headers, nil
}

// forceUncompressedUpstreamResponse pins the upstream response to an
// unencoded representation for targets whose response this gateway will
// rewrite. A rewritten response has to be parsed as JSON, and the gateway
// never decodes a transfer encoding, so an upstream that honored the client's
// gzip negotiation would make protocol conversion or a response patch fail on
// compressed bytes. An absent Accept-Encoding would leave any coding
// acceptable, so the value is set explicitly rather than deleted. Pure
// passthrough requests keep the client's own negotiation untouched.
func forceUncompressedUpstreamResponse(headers http.Header) {
	if headers == nil {
		return
	}
	deleteHeaderFold(headers, "Accept-Encoding")
	headers.Set("Accept-Encoding", "identity")
}

func copyResponseHeaders(destination http.Header, source http.Header) {
	headers := source.Clone()
	removeHopByHop(headers)
	for key := range destination {
		delete(destination, key)
	}
	for key, values := range headers {
		for _, value := range values {
			destination.Add(key, value)
		}
	}
}

func removeHopByHop(headers http.Header) {
	var connectionValues []string
	for name, values := range headers {
		if strings.EqualFold(name, "Connection") {
			connectionValues = append(connectionValues, values...)
		}
	}
	for _, value := range connectionValues {
		for _, token := range strings.Split(value, ",") {
			if name := strings.TrimSpace(token); name != "" {
				deleteHeaderFold(headers, name)
			}
		}
	}
	for _, name := range hopByHopHeaders {
		deleteHeaderFold(headers, name)
	}
}

func deleteHeaderFold(headers http.Header, target string) {
	for name := range headers {
		if strings.EqualFold(name, target) {
			delete(headers, name)
		}
	}
}
