package httpapi

import (
	"bytes"
	"errors"
	"io"
	"mime"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/HasonoCell/Orbit-DevOps/internal/api"
	"github.com/HasonoCell/Orbit-DevOps/internal/identity"
	"github.com/gin-gonic/gin"
)

var ErrInvalidBrowserSecurity = errors.New("invalid browser security configuration")

// BrowserSecurityConfig 明确浏览器入口，不从 Host/Header 动态决定可信 Origin。
// Loopback HTTP 是显式开发例外，不能将非本地 HTTP 配置成生产入口。
type BrowserSecurityConfig struct {
	ExternalURL       string
	TrustedOrigins    []string
	AllowLoopbackHTTP bool
	// 复制既有 Webhook 的限制，不能被通用 JSON 默认上限无意截断。
	WebhookMaxBodyBytes int64
}

// BrowserSecurity 拥有浏览器传输协议；身份和角色事实仍由 identity/projectauth 决定。
type BrowserSecurity struct {
	origins             map[string]struct{}
	frontendOrigin      string
	secure              bool
	webhookMaxBodyBytes int64
}

func NewBrowserSecurity(config BrowserSecurityConfig) (*BrowserSecurity, error) {
	origin, secure, err := browserOrigin(config.ExternalURL, config.AllowLoopbackHTTP, true)
	if err != nil {
		return nil, err
	}
	webhookLimit := config.WebhookMaxBodyBytes
	if webhookLimit == 0 {
		webhookLimit = 1024 * 1024
	}
	if webhookLimit < 0 || webhookLimit > 10*1024*1024 {
		return nil, ErrInvalidBrowserSecurity
	}
	security := &BrowserSecurity{origins: map[string]struct{}{origin: {}}, frontendOrigin: origin, secure: secure, webhookMaxBodyBytes: webhookLimit}
	for _, configured := range config.TrustedOrigins {
		origin, _, err := browserOrigin(configured, config.AllowLoopbackHTTP && !secure, false)
		if err != nil {
			return nil, err
		}
		security.origins[origin] = struct{}{}
	}
	return security, nil
}

func (s *BrowserSecurity) oidcCookieName() string {
	if s.secure {
		return "__Host-orbit-oidc"
	}
	return "orbit-dev-oidc"
}

func (s *BrowserSecurity) SetOIDCBrowserCookie(c *gin.Context, token identity.SessionToken, expiresAt time.Time) error {
	if token.CookieValue() == "" || expiresAt.IsZero() {
		return identity.ErrOIDCTransaction
	}
	http.SetCookie(c.Writer, &http.Cookie{Name: s.oidcCookieName(), Value: token.CookieValue(), Path: "/",
		HttpOnly: true, Secure: s.secure, SameSite: http.SameSiteLaxMode, Expires: expiresAt})
	return nil
}

func (s *BrowserSecurity) OIDCBrowserCookieValue(request *http.Request) (string, error) {
	values := make([]string, 0, 1)
	for _, cookie := range request.Cookies() {
		if cookie.Name == s.oidcCookieName() {
			values = append(values, cookie.Value)
		}
	}
	if len(values) != 1 || values[0] == "" {
		return "", identity.ErrOIDCTransaction
	}
	return values[0], nil
}

func (s *BrowserSecurity) ClearOIDCBrowserCookie(c *gin.Context) {
	http.SetCookie(c.Writer, &http.Cookie{Name: s.oidcCookieName(), Value: "", Path: "/", HttpOnly: true,
		Secure: s.secure, SameSite: http.SameSiteLaxMode, Expires: time.Unix(1, 0), MaxAge: -1})
}

func (s *BrowserSecurity) OIDCCallbackRedirect() string { return s.frontendOrigin + "/auth/callback" }

// CookieName 区分显式 loopback 开发与生产，不因请求 Host/协议改变。
func (s *BrowserSecurity) CookieName() string {
	if s.secure {
		return "__Host-orbit-session"
	}
	return "orbit-dev-session"
}

// SetSessionCookie 只接收 identity 返回的合法签发结果，不把 Token 写进响应 JSON。
func (s *BrowserSecurity) SetSessionCookie(c *gin.Context, result identity.LoginResult) error {
	if result.Token.CookieValue() == "" || result.ExpiresAt.IsZero() {
		return identity.ErrUnauthenticated
	}
	c.Header("Cache-Control", "no-store")
	http.SetCookie(c.Writer, &http.Cookie{Name: s.CookieName(), Value: result.Token.CookieValue(),
		Path: "/", HttpOnly: true, Secure: s.secure, SameSite: http.SameSiteLaxMode, Expires: result.ExpiresAt})
	return nil
}

// ClearSessionCookie 与签发使用相同 host-only/Path/安全属性，避免只清理另一个 Cookie。
func (s *BrowserSecurity) ClearSessionCookie(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	http.SetCookie(c.Writer, &http.Cookie{Name: s.CookieName(), Value: "", Path: "/", HttpOnly: true,
		Secure: s.secure, SameSite: http.SameSiteLaxMode, Expires: time.Unix(1, 0), MaxAge: -1})
}

// SessionCookieValue 拒绝重复同名凭据；其他 Cookie 不具有平台认证能力。
func (s *BrowserSecurity) SessionCookieValue(request *http.Request) (string, error) {
	value, count := "", 0
	for _, cookie := range request.Cookies() {
		if cookie.Name == s.CookieName() {
			value, count = cookie.Value, count+1
		}
	}
	if count != 1 || value == "" {
		return "", identity.ErrUnauthenticated
	}
	return value, nil
}

// Middleware 包含 login CSRF；非浏览器测试/CLI 也必须提供相同证明，没有跳过 flag。
// 唯一写例外是契约定义的 GitHub Webhook，它另由签名与 endpoint 完整验证。
func (s *BrowserSecurity) Middleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		request := c.Request
		identityResponse := strings.HasPrefix(request.URL.Path, "/api/v1/auth/") ||
			request.URL.Path == "/api/v1/users" || strings.HasPrefix(request.URL.Path, "/api/v1/users/")
		if identityResponse {
			c.Header("Cache-Control", "no-store")
			c.Header("Referrer-Policy", "no-referrer")
		}
		readOnly := request.Method == http.MethodGet || request.Method == http.MethodHead
		webhook := request.Method == http.MethodPost && c.FullPath() == "/api/v1/webhooks/github/:endpointKey"
		if request.Method == http.MethodOptions {
			s.preflight(c)
			return
		}
		if !readOnly && !webhook {
			origins, headers := request.Header.Values("Origin"), request.Header.Values("X-Orbit-CSRF")
			if len(origins) != 1 || len(headers) != 1 || headers[0] != "1" {
				browserError(c, http.StatusForbidden, "csrf_rejected", "request origin and CSRF header required")
				return
			}
			if _, trusted := s.origins[origins[0]]; !trusted {
				browserError(c, http.StatusForbidden, "csrf_rejected", "request origin not allowed")
				return
			}
		}
		// Origin 缺失的只读导航允许；带凭据 CORS 只授予配置中的精确来源。
		if origins := request.Header.Values("Origin"); len(origins) > 0 && !webhook {
			if len(origins) != 1 {
				browserError(c, http.StatusForbidden, "cors_rejected", "request origin not allowed")
				return
			}
			if _, trusted := s.origins[origins[0]]; !trusted {
				browserError(c, http.StatusForbidden, "cors_rejected", "request origin not allowed")
				return
			}
			c.Header("Access-Control-Allow-Origin", origins[0])
			c.Header("Access-Control-Allow-Credentials", "true")
			c.Writer.Header().Add("Vary", "Origin")
		}
		if request.Body != nil && request.Body != http.NoBody {
			if readOnly {
				browserError(c, http.StatusBadRequest, "unexpected_body", "read request must not contain a body")
				return
			}
			if !webhook {
				contentTypes := request.Header.Values("Content-Type")
				mediaType, _, err := mime.ParseMediaType(request.Header.Get("Content-Type"))
				if len(contentTypes) != 1 || err != nil || mediaType != "application/json" {
					browserError(c, http.StatusUnsupportedMediaType, "json_required", "JSON request body required")
					return
				}
			}
			limit := int64(1024 * 1024)
			if webhook {
				limit = s.webhookMaxBodyBytes
			}
			if identityResponse {
				limit = 16 * 1024
			}
			// OpenAPI 安全校验自身也会读取 Body，必须在它之前完成有界读取。
			body, err := io.ReadAll(io.LimitReader(request.Body, limit+1))
			_ = request.Body.Close()
			if int64(len(body)) > limit {
				browserError(c, http.StatusRequestEntityTooLarge, "body_too_large", "request body too large")
				return
			}
			if err != nil {
				browserError(c, http.StatusBadRequest, "invalid_body", "request body could not be read")
				return
			}
			request.Body = io.NopCloser(bytes.NewReader(body))
			request.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(body)), nil }
			request.ContentLength = int64(len(body))
		}
		c.Next()
	}
}

// preflight 不认证用户，也不执行命令；只协商固定方法/Header，不反射任意请求 Header。
func (s *BrowserSecurity) preflight(c *gin.Context) {
	origins := c.Request.Header.Values("Origin")
	methods := c.Request.Header.Values("Access-Control-Request-Method")
	if len(origins) != 1 || len(methods) != 1 {
		browserError(c, http.StatusForbidden, "cors_rejected", "invalid CORS preflight")
		return
	}
	if _, trusted := s.origins[origins[0]]; !trusted || !corsMethodAllowed(methods[0]) {
		browserError(c, http.StatusForbidden, "cors_rejected", "CORS preflight not allowed")
		return
	}
	headers := c.Request.Header.Values("Access-Control-Request-Headers")
	if len(headers) > 1 {
		browserError(c, http.StatusForbidden, "cors_rejected", "CORS headers not allowed")
		return
	}
	if len(headers) == 1 && headers[0] != "" {
		for _, header := range strings.Split(headers[0], ",") {
			switch strings.ToLower(strings.TrimSpace(header)) {
			case "accept", "content-type", "x-orbit-csrf", "idempotency-key", "traceparent", "tracestate":
			default:
				browserError(c, http.StatusForbidden, "cors_rejected", "CORS headers not allowed")
				return
			}
		}
	}
	c.Header("Access-Control-Allow-Origin", origins[0])
	c.Header("Access-Control-Allow-Credentials", "true")
	c.Header("Access-Control-Allow-Methods", "GET, HEAD, POST, PUT, DELETE, OPTIONS")
	c.Header("Access-Control-Allow-Headers", "Accept, Content-Type, X-Orbit-CSRF, Idempotency-Key, Traceparent, Tracestate")
	c.Writer.Header().Add("Vary", "Origin")
	c.Writer.Header().Add("Vary", "Access-Control-Request-Method")
	c.Writer.Header().Add("Vary", "Access-Control-Request-Headers")
	c.AbortWithStatus(http.StatusNoContent)
}

func corsMethodAllowed(method string) bool {
	switch method {
	case http.MethodGet, http.MethodHead, http.MethodPost, http.MethodPut, http.MethodDelete, http.MethodOptions:
		return true
	default:
		return false
	}
}

func browserError(c *gin.Context, status int, code, message string) {
	c.AbortWithStatusJSON(status, api.Error{Code: code, Message: message})
}

// browserOrigin 只接受可配置的根 URL，拒绝凭据、查询串、fragment、wildcard 与非本地 HTTP。
// 错误不回显原始配置，防止误填的 secret 被打印。
func browserOrigin(value string, allowLoopback, rootSlash bool) (string, bool, error) {
	u, err := url.Parse(value)
	if err != nil || u.User != nil || u.Hostname() == "" || u.RawQuery != "" || u.ForceQuery ||
		u.Fragment != "" || u.Opaque != "" || (u.Path != "" && (!rootSlash || u.Path != "/")) ||
		strings.ContainsAny(u.Host, "*\\\r\n\t ") {
		return "", false, ErrInvalidBrowserSecurity
	}
	if port := u.Port(); port != "" {
		number, err := strconv.ParseUint(port, 10, 16)
		if err != nil || number == 0 {
			return "", false, ErrInvalidBrowserSecurity
		}
	} else if strings.HasSuffix(u.Host, ":") {
		return "", false, ErrInvalidBrowserSecurity
	}
	secure := u.Scheme == "https"
	if !secure {
		ip := net.ParseIP(u.Hostname())
		loopback := strings.EqualFold(u.Hostname(), "localhost") || ip != nil && ip.IsLoopback()
		if u.Scheme != "http" || !allowLoopback || !loopback {
			return "", false, ErrInvalidBrowserSecurity
		}
	}
	return u.Scheme + "://" + strings.ToLower(u.Host), secure, nil
}
