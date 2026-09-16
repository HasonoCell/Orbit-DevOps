package httpapi_test

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/HasonoCell/Orbit-DevOps/internal/transport/httpapi"
	"github.com/gin-gonic/gin"
)

func TestBrowserConfigurationFailsClosedWithoutEchoingInvalidURL(t *testing.T) {
	t.Parallel()
	for _, test := range []httpapi.BrowserSecurityConfig{
		{},
		{ExternalURL: "http://127.0.0.1:5173"},
		{ExternalURL: "http://attacker.example", AllowLoopbackHTTP: true},
		{ExternalURL: "https://secret:fixture-value@orbit.example"},
		{ExternalURL: "https://orbit.example/?code=fixture-secret"},
		{ExternalURL: "https://orbit.example:70000"},
		{ExternalURL: "https://orbit.example", TrustedOrigins: []string{"*"}},
		{ExternalURL: "https://orbit.example", TrustedOrigins: []string{"null"}},
		{ExternalURL: "https://orbit.example", TrustedOrigins: []string{"https://console.example/"}},
		{ExternalURL: "https://orbit.example", AllowLoopbackHTTP: true, TrustedOrigins: []string{"http://127.0.0.1:5173"}},
	} {
		_, err := httpapi.NewBrowserSecurity(test)
		if !errors.Is(err, httpapi.ErrInvalidBrowserSecurity) || err.Error() != "invalid browser security configuration" {
			t.Fatal("unsafe configuration was accepted or input material was echoed")
		}
	}
}

func TestOnlyExplicitWebhookRouteIsExemptAndItsBoundedBodyIsPreserved(t *testing.T) {
	t.Parallel()
	security, err := httpapi.NewBrowserSecurity(httpapi.BrowserSecurityConfig{
		ExternalURL: "https://orbit.example", WebhookMaxBodyBytes: 2 * 1024 * 1024,
	})
	if err != nil {
		t.Fatalf("construct webhook transport bound: %v", err)
	}
	router := gin.New()
	router.Use(security.Middleware())
	router.POST("/api/v1/webhooks/github/:endpointKey", func(c *gin.Context) {
		body, err := io.ReadAll(c.Request.Body)
		if err != nil || len(body) != 1536*1024 {
			t.Error("bounded transport changed the body needed for independent signature verification")
			c.Status(http.StatusBadRequest)
			return
		}
		c.Status(http.StatusAccepted)
	})
	router.POST("/api/v1/webhooks/github-lookalike/:endpointKey", func(c *gin.Context) {
		c.Status(http.StatusAccepted)
	})
	for _, test := range []struct {
		path         string
		size, status int
	}{
		{"/api/v1/webhooks/github/fixture", 1536 * 1024, http.StatusAccepted},
		{"/api/v1/webhooks/github/fixture", 2*1024*1024 + 1, http.StatusRequestEntityTooLarge},
		{"/api/v1/webhooks/github-lookalike/fixture", 0, http.StatusForbidden},
	} {
		request := httptest.NewRequest(http.MethodPost, "https://orbit.example"+test.path, strings.NewReader(strings.Repeat("x", test.size)))
		response := httptest.NewRecorder()
		router.ServeHTTP(response, request)
		if response.Code != test.status {
			t.Fatalf("route transport status = %d, want %d", response.Code, test.status)
		}
	}
}

func TestBrowserGuardRejectsReadBodiesAndAmbiguousProofHeaders(t *testing.T) {
	t.Parallel()
	security, err := httpapi.NewBrowserSecurity(httpapi.BrowserSecurityConfig{ExternalURL: "https://orbit.example"})
	if err != nil {
		t.Fatalf("construct ambiguity guard: %v", err)
	}
	router := gin.New()
	router.Use(security.Middleware())
	router.GET("/api/v1/users/me", func(c *gin.Context) { c.Status(http.StatusNoContent) })
	router.POST("/api/v1/auth/logout", func(c *gin.Context) { c.Status(http.StatusNoContent) })
	read := httptest.NewRequest(http.MethodGet, "https://orbit.example/api/v1/users/me", strings.NewReader(`{}`))
	response := httptest.NewRecorder()
	router.ServeHTTP(response, read)
	if response.Code != http.StatusBadRequest {
		t.Fatal("GET body could reach OpenAPI's unbounded authentication read")
	}
	write := httptest.NewRequest(http.MethodPost, "https://orbit.example/api/v1/auth/logout", nil)
	write.Header.Set("Origin", "https://orbit.example")
	write.Header.Set("X-Orbit-CSRF", "1")
	response = httptest.NewRecorder()
	router.ServeHTTP(response, write)
	if response.Code != http.StatusNoContent {
		t.Fatal("bodyless Cookie write incorrectly required a JSON body")
	}
	write.Header.Add("Origin", "https://attacker.example")
	response = httptest.NewRecorder()
	router.ServeHTTP(response, write)
	if response.Code != http.StatusForbidden {
		t.Fatal("duplicate Origin proof was ambiguous")
	}
}
