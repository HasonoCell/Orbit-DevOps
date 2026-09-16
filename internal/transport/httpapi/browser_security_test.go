package httpapi_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/HasonoCell/Orbit-DevOps/internal/transport/httpapi"
	"github.com/gin-gonic/gin"
)

func TestBrowserWriteGuardRequiresExplicitOriginHeaderAndJSONEvenForLogin(t *testing.T) {
	t.Parallel()
	security, err := httpapi.NewBrowserSecurity(httpapi.BrowserSecurityConfig{ExternalURL: "https://orbit.example"})
	if err != nil {
		t.Fatalf("construct browser guard: %v", err)
	}
	router := gin.New()
	router.Use(security.Middleware())
	router.POST("/api/v1/auth/login", func(c *gin.Context) { c.Status(http.StatusNoContent) })
	for _, test := range []struct {
		name, origin, csrf, contentType, body string
		status                                int
	}{
		{"trusted JSON", "https://orbit.example", "1", "application/json", `{}`, http.StatusNoContent},
		{"missing origin", "", "1", "application/json", `{}`, http.StatusForbidden},
		{"untrusted origin", "https://attacker.example", "1", "application/json", `{}`, http.StatusForbidden},
		{"null origin", "null", "1", "application/json", `{}`, http.StatusForbidden},
		{"missing header", "https://orbit.example", "", "application/json", `{}`, http.StatusForbidden},
		{"wrong header", "https://orbit.example", "true", "application/json", `{}`, http.StatusForbidden},
		{"form content", "https://orbit.example", "1", "application/x-www-form-urlencoded", `password=fixture`, http.StatusUnsupportedMediaType},
		{"oversized auth body", "https://orbit.example", "1", "application/json", strings.Repeat(" ", 16*1024+1), http.StatusRequestEntityTooLarge},
	} {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodPost, "https://orbit.example/api/v1/auth/login", strings.NewReader(test.body))
			request.Header.Set("Origin", test.origin)
			request.Header.Set("X-Orbit-CSRF", test.csrf)
			request.Header.Set("Content-Type", test.contentType)
			response := httptest.NewRecorder()
			router.ServeHTTP(response, request)
			if response.Code != test.status {
				t.Fatalf("status = %d, want %d", response.Code, test.status)
			}
			if response.Header().Get("Cache-Control") != "no-store" {
				t.Fatal("authentication response was cacheable")
			}
		})
	}
}

func TestCredentialedCORSAllowsOnlyConfiguredOriginsMethodsAndHeaders(t *testing.T) {
	t.Parallel()
	security, err := httpapi.NewBrowserSecurity(httpapi.BrowserSecurityConfig{
		ExternalURL: "https://orbit.example", TrustedOrigins: []string{"https://console.example"},
	})
	if err != nil {
		t.Fatalf("construct CORS guard: %v", err)
	}
	router := gin.New()
	router.Use(security.Middleware())
	router.OPTIONS("/api/v1/auth/login", func(c *gin.Context) { c.Status(http.StatusNotImplemented) })
	for _, test := range []struct {
		name, origin, method, headers string
		status                        int
	}{
		{"trusted explicit preflight", "https://console.example", "POST", "Content-Type, X-Orbit-CSRF, Idempotency-Key", http.StatusNoContent},
		{"untrusted preflight", "https://attacker.example", "POST", "X-Orbit-CSRF", http.StatusForbidden},
		{"null preflight", "null", "POST", "X-Orbit-CSRF", http.StatusForbidden},
		{"unclassified method", "https://console.example", "CONNECT", "X-Orbit-CSRF", http.StatusForbidden},
		{"unexpected identity header", "https://console.example", "POST", "X-Actor-ID", http.StatusForbidden},
	} {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodOptions, "https://orbit.example/api/v1/auth/login", nil)
			request.Header.Set("Origin", test.origin)
			request.Header.Set("Access-Control-Request-Method", test.method)
			request.Header.Set("Access-Control-Request-Headers", test.headers)
			response := httptest.NewRecorder()
			router.ServeHTTP(response, request)
			if response.Code != test.status {
				t.Fatalf("preflight status = %d, want %d", response.Code, test.status)
			}
			if test.status == http.StatusNoContent {
				if response.Header().Get("Access-Control-Allow-Origin") != test.origin ||
					response.Header().Get("Access-Control-Allow-Credentials") != "true" {
					t.Fatal("trusted credentialed CORS was not explicitly scoped")
				}
			} else if response.Header().Get("Access-Control-Allow-Origin") != "" {
				t.Fatal("rejected preflight received a credentialed CORS grant")
			}
		})
	}
}
