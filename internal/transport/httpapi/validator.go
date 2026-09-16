package httpapi

import (
	"context"
	"errors"
	"net/http"

	"github.com/HasonoCell/Orbit-DevOps/internal/api"
	"github.com/HasonoCell/Orbit-DevOps/internal/identity"
	"github.com/HasonoCell/Orbit-DevOps/internal/projectauth"
	"github.com/getkin/kin-openapi/openapi3filter"
	"github.com/gin-gonic/gin"
	ginmiddleware "github.com/oapi-codegen/gin-middleware"
)

const authenticationStatusKey = "orbit-devops.authentication-status"

// ValidatorOptions 将 OpenAPI CookieAuth 连接到真实 Session 解析；协议校验错误不回显底层错误。
func ValidatorOptions(identities *identity.Module, browser *BrowserSecurity) *ginmiddleware.Options {
	return &ginmiddleware.Options{
		Options: openapi3filter.Options{AuthenticationFunc: func(ctx context.Context, input *openapi3filter.AuthenticationInput) error {
			ginContext := ginmiddleware.GetGinContext(ctx)
			if ginContext == nil || identities == nil || browser == nil || input.SecuritySchemeName != "CookieAuth" {
				return errors.New("authentication unavailable")
			}
			cookie, err := browser.SessionCookieValue(ginContext.Request)
			if err == nil {
				var caller identity.Caller
				caller, err = identities.ResolveSession(ginContext.Request.Context(), cookie)
				if err == nil {
					ginContext.Request = ginContext.Request.WithContext(identity.WithCaller(ginContext.Request.Context(), caller))
					return nil
				}
			}
			status := http.StatusUnauthorized
			if errors.Is(err, identity.ErrUnavailable) || errors.Is(err, context.DeadlineExceeded) {
				status = http.StatusServiceUnavailable
			}
			ginContext.Set(authenticationStatusKey, status)
			return errors.New("cookie authentication failed")
		}},
		ErrorHandler: func(c *gin.Context, _ string, statusCode int) {
			if authenticationStatus, exists := c.Get(authenticationStatusKey); exists {
				statusCode = authenticationStatus.(int)
				code, message := "authentication_required", "authentication required"
				if statusCode == http.StatusServiceUnavailable {
					code, message = "identity_unavailable", "identity service unavailable"
				}
				c.AbortWithStatusJSON(statusCode, gin.H{"code": code, "message": message})
				return
			}
			if statusCode == http.StatusNotFound {
				c.AbortWithStatusJSON(statusCode, gin.H{"code": "route_not_found", "message": "route not found"})
				return
			}
			c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"code": "invalid_request", "message": "request does not match API contract"})
		},
	}
}

// StrictHandlerOptions 统一隐藏驱动/凭据/约束细节，同时保留身份依赖和授权语义。
func StrictHandlerOptions() api.StrictGinServerOptions {
	write := func(c *gin.Context, status int, code, message string) {
		c.AbortWithStatusJSON(status, gin.H{"code": code, "message": message})
	}
	return api.StrictGinServerOptions{
		RequestErrorHandlerFunc: func(c *gin.Context, _ error) {
			write(c, http.StatusBadRequest, "invalid_request", "request body is invalid")
		},
		HandlerErrorFunc: func(c *gin.Context, err error) {
			switch {
			case errors.Is(err, identity.ErrUnauthenticated):
				write(c, http.StatusUnauthorized, "authentication_required", "authentication required")
			case errors.Is(err, identity.ErrPasswordChangeRequired):
				write(c, http.StatusForbidden, "password_change_required", "password change required")
			case errors.Is(err, identity.ErrForbidden), errors.Is(err, identity.ErrRecentAuthenticationRequired), errors.Is(err, projectauth.ErrForbidden):
				write(c, http.StatusForbidden, "permission_denied", "permission denied")
			case errors.Is(err, identity.ErrUnavailable), errors.Is(err, context.DeadlineExceeded):
				write(c, http.StatusServiceUnavailable, "dependency_unavailable", "required dependency unavailable")
			default:
				write(c, http.StatusInternalServerError, "internal_error", "internal server error")
			}
		},
		ResponseErrorHandlerFunc: func(c *gin.Context, _ error) {
			write(c, http.StatusInternalServerError, "response_error", "response could not be encoded")
		},
	}
}
