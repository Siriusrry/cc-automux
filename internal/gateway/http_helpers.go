package gateway

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"

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
	result := *item.BaseURL
	escapedPrefix := result.EscapedPath()
	if strings.HasSuffix(escapedPrefix, "/") {
		escapedPrefix = strings.TrimSuffix(escapedPrefix, "/")
	}
	escapedPath := escapedPrefix + MessagesPath
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
	return &result, nil
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
