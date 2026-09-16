package httpapi_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/HasonoCell/Orbit-DevOps/internal/identity"
	"github.com/HasonoCell/Orbit-DevOps/internal/platform/database"
	"github.com/HasonoCell/Orbit-DevOps/internal/projectauth"
	"github.com/HasonoCell/Orbit-DevOps/internal/transport/httpapi"
	"github.com/gin-gonic/gin"
	"github.com/jmoiron/sqlx"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
)

// cookieLoginFixture 仅合成独立 PG 用户，经正常身份 Interface 得到合法 Session。
// HTTP fixture 只检验浏览器传输协议，不声称验证尚未接入 app 的 OpenAPI 身份路由。
func cookieLoginFixture(t *testing.T) identity.LoginResult {
	t.Helper()
	ctx := context.Background()
	container, err := postgres.Run(ctx, "postgres:17-alpine",
		postgres.WithDatabase("browser_cookie_fixture"), postgres.WithUsername("fixture"),
		postgres.WithPassword("fixture"), postgres.BasicWaitStrategies())
	if err != nil {
		t.Fatalf("start cookie fixture: %v", err)
	}
	t.Cleanup(func() { _ = container.Terminate(context.Background()) })
	dsn, err := container.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		t.Fatal("get cookie fixture connection")
	}
	if err := database.Migrate(dsn); err != nil {
		t.Fatalf("migrate cookie fixture: %v", err)
	}
	db, err := sqlx.ConnectContext(ctx, "pgx", dsn)
	if err != nil {
		t.Fatal("connect cookie fixture")
	}
	t.Cleanup(func() { _ = db.Close() })
	module, err := identity.New(db, projectauth.NewOwnershipGuard())
	if err != nil {
		t.Fatalf("construct cookie fixture identity: %v", err)
	}
	const password = "fixture-only!Browser-September-2026"
	if _, err := module.InitializeAdmin(ctx, identity.InitializeAdminCommand{
		LoginName: "fixture-admin", DisplayName: "测试管理员", Password: password, MaintenanceRef: "fixture-browser-cookie",
	}); err != nil {
		t.Fatalf("initialize cookie fixture: %v", err)
	}
	result, err := module.LoginLocal(ctx, identity.LocalLoginCommand{
		LoginName: "fixture-admin", Password: password, SourceIP: "127.0.0.1",
	})
	if err != nil {
		t.Fatalf("login cookie fixture: %v", err)
	}
	return result
}

func TestSessionCookiePolicyUsesTrustedConfigurationAndClearsTheSameCookie(t *testing.T) {
	t.Parallel()
	login := cookieLoginFixture(t)
	for _, test := range []struct {
		name, externalURL, cookieName string
		allowHTTP, secure             bool
	}{
		{"production", "https://orbit.example", "__Host-orbit-session", false, true},
		{"explicit loopback development", "http://127.0.0.1:5173", "orbit-dev-session", true, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			security, err := httpapi.NewBrowserSecurity(httpapi.BrowserSecurityConfig{
				ExternalURL: test.externalURL, AllowLoopbackHTTP: test.allowHTTP,
			})
			if err != nil {
				t.Fatalf("construct cookie policy: %v", err)
			}
			router := gin.New()
			router.POST("/fixture/issue", func(c *gin.Context) {
				if err := security.SetSessionCookie(c, login); err != nil {
					t.Error("write valid Session Cookie")
					c.Status(http.StatusInternalServerError)
					return
				}
				c.Status(http.StatusNoContent)
			})
			router.POST("/fixture/clear", func(c *gin.Context) {
				security.ClearSessionCookie(c)
				c.Status(http.StatusNoContent)
			})
			// 请求 Host/协议不得降低配置中生产 Secure/前缀规则。
			issue := httptest.NewRecorder()
			router.ServeHTTP(issue, httptest.NewRequest(http.MethodPost, "http://attacker.example/fixture/issue", nil))
			cookies := issue.Result().Cookies()
			if issue.Code != http.StatusNoContent || len(cookies) != 1 {
				t.Fatal("valid login did not set exactly one Cookie")
			}
			cookie := cookies[0]
			if cookie.Name != test.cookieName || cookie.Value != login.Token.CookieValue() || cookie.Domain != "" ||
				cookie.Path != "/" || !cookie.HttpOnly || cookie.Secure != test.secure || cookie.SameSite != http.SameSiteLaxMode ||
				cookie.Expires.Unix() != login.ExpiresAt.Unix() {
				t.Fatal("session Cookie did not follow the approved host-only policy")
			}
			request := httptest.NewRequest(http.MethodGet, test.externalURL, nil)
			request.AddCookie(cookie)
			value, err := security.SessionCookieValue(request)
			if err != nil || value != login.Token.CookieValue() {
				t.Fatalf("read named Session Cookie: %v", err)
			}
			request.AddCookie(cookie)
			if _, err := security.SessionCookieValue(request); !errors.Is(err, identity.ErrUnauthenticated) {
				t.Fatalf("duplicate credential Cookies were ambiguous: %v", err)
			}
			clear := httptest.NewRecorder()
			router.ServeHTTP(clear, httptest.NewRequest(http.MethodPost, test.externalURL+"/fixture/clear", nil))
			cleared := clear.Result().Cookies()
			if len(cleared) != 1 || cleared[0].Name != cookie.Name || cleared[0].Path != cookie.Path ||
				cleared[0].Domain != "" || cleared[0].MaxAge >= 0 || cleared[0].Value != "" || cleared[0].Secure != cookie.Secure {
				t.Fatal("logout did not clear the same host-only Cookie")
			}
		})
	}
}
