package gateway

import (
	"net/http/httptest"
	"testing"
)

func TestBearerAuthorizationIsStrict(t *testing.T) {
	for _, value := range []string{
		"",
		"Basic gateway-key",
		"Bearer",
		"Bearer ",
		"Bearer\tgateway-key",
		"Bearer  gateway-key",
		" Bearer gateway-key",
		"Bearer gateway-key ",
	} {
		request := httptest.NewRequest("POST", MessagesPath, nil)
		if value != "" {
			request.Header.Set("Authorization", value)
		}
		if authorized(request, "gateway-key") {
			t.Fatalf("authorized malformed value %q", value)
		}
	}
	request := httptest.NewRequest("POST", MessagesPath, nil)
	request.Header.Set("Authorization", "bEaReR gateway-key")
	if !authorized(request, "gateway-key") {
		t.Fatal("case-insensitive Bearer scheme was rejected")
	}
	request.Header.Add("Authorization", "Bearer gateway-key")
	if authorized(request, "gateway-key") {
		t.Fatal("multiple Authorization values were accepted")
	}
}
