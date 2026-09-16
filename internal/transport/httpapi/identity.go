package httpapi

import (
	"context"
	"errors"
	"net"

	"github.com/HasonoCell/Orbit-DevOps/internal/api"
	"github.com/HasonoCell/Orbit-DevOps/internal/identity"
	"github.com/gin-gonic/gin"
	openapi_types "github.com/oapi-codegen/runtime/types"
)

func (s *Server) ListAuthProviders(ctx context.Context, _ api.ListAuthProvidersRequestObject) (api.ListAuthProvidersResponseObject, error) {
	result := api.ListAuthProviders200JSONResponse{{Id: "local", Type: api.AuthProviderSummaryTypeLocal, DisplayName: "本地账号", Available: true}}
	providers, err := s.identities.ListOIDCProviders(httpRequestContext(ctx))
	if err != nil {
		return nil, err
	}
	for _, provider := range providers {
		result = append(result, api.AuthProviderSummary{Id: provider.ID, Type: api.AuthProviderSummaryTypeOidc,
			DisplayName: provider.DisplayName, Available: s.identities.OIDCProviderAvailable(provider)})
	}
	return result, nil
}

func (s *Server) LoginLocal(ctx context.Context, request api.LoginLocalRequestObject) (api.LoginLocalResponseObject, error) {
	result, err := s.identities.LoginLocal(httpRequestContext(ctx), identity.LocalLoginCommand{
		LoginName: request.Body.LoginName, Password: stringValue(request.Body.Password), SourceIP: requestSourceIP(ctx),
	})
	if err != nil {
		if errors.Is(err, identity.ErrRateLimited) {
			return api.LoginLocal429JSONResponse{Code: "authentication_rate_limited", Message: "too many authentication attempts"}, nil
		}
		if errors.Is(err, identity.ErrUnauthenticated) || errors.Is(err, identity.ErrInvalidCommand) {
			return api.LoginLocal401JSONResponse{Code: "invalid_credentials", Message: "login name or password is invalid"}, nil
		}
		return nil, err
	}
	if err := s.browserSecurity.SetSessionCookie(ginContext(ctx), result); err != nil {
		return nil, err
	}
	return api.LoginLocal200JSONResponse(userPrincipalResponse(result.CurrentUser)), nil
}

func (s *Server) LogoutCurrent(ctx context.Context, _ api.LogoutCurrentRequestObject) (api.LogoutCurrentResponseObject, error) {
	ginCtx := ginContext(ctx)
	cookie, _ := s.browserSecurity.SessionCookieValue(ginCtx.Request)
	if err := s.identities.LogoutCurrent(ginCtx.Request.Context(), cookie); err != nil {
		return nil, err
	}
	s.browserSecurity.ClearSessionCookie(ginCtx)
	return api.LogoutCurrent204Response{}, nil
}

func (s *Server) LogoutAll(ctx context.Context, _ api.LogoutAllRequestObject) (api.LogoutAllResponseObject, error) {
	if err := s.identities.LogoutAll(httpRequestContext(ctx), requestCaller(ctx)); err != nil {
		return nil, err
	}
	s.browserSecurity.ClearSessionCookie(ginContext(ctx))
	return api.LogoutAll204Response{}, nil
}

func (s *Server) ReauthenticateLocal(ctx context.Context, request api.ReauthenticateLocalRequestObject) (api.ReauthenticateLocalResponseObject, error) {
	result, err := s.identities.ReauthenticateLocal(httpRequestContext(ctx), identity.ReauthenticateLocalCommand{
		Caller: requestCaller(ctx), Password: stringValue(request.Body.Password), SourceIP: requestSourceIP(ctx),
	})
	if err != nil {
		if errors.Is(err, identity.ErrRateLimited) {
			return api.ReauthenticateLocal429JSONResponse{Code: "authentication_rate_limited", Message: "too many authentication attempts"}, nil
		}
		if errors.Is(err, identity.ErrUnauthenticated) {
			return api.ReauthenticateLocal401JSONResponse{Code: "invalid_credentials", Message: "password is invalid"}, nil
		}
		return nil, err
	}
	if err := s.browserSecurity.SetSessionCookie(ginContext(ctx), result); err != nil {
		return nil, err
	}
	return api.ReauthenticateLocal200JSONResponse(userPrincipalResponse(result.CurrentUser)), nil
}

func (s *Server) GetCurrentUser(ctx context.Context, _ api.GetCurrentUserRequestObject) (api.GetCurrentUserResponseObject, error) {
	current, err := s.identities.CurrentPrincipal(httpRequestContext(ctx), requestCaller(ctx))
	if err != nil {
		return nil, err
	}
	return api.GetCurrentUser200JSONResponse(principalResponse(current)), nil
}

func (s *Server) ChangeCurrentPassword(ctx context.Context, request api.ChangeCurrentPasswordRequestObject) (api.ChangeCurrentPasswordResponseObject, error) {
	err := s.identities.ChangePassword(httpRequestContext(ctx), identity.ChangePasswordCommand{
		Caller: requestCaller(ctx), CurrentPassword: stringValue(request.Body.CurrentPassword),
		NewPassword: stringValue(request.Body.NewPassword), LoginName: optionalString(request.Body.LoginName), SourceIP: requestSourceIP(ctx),
	})
	if err != nil {
		if errors.Is(err, identity.ErrInvalidPassword) || errors.Is(err, identity.ErrInvalidCommand) {
			return api.ChangeCurrentPassword400JSONResponse{Code: "invalid_password", Message: "password does not meet policy"}, nil
		}
		if errors.Is(err, identity.ErrUnauthenticated) {
			return api.ChangeCurrentPassword401JSONResponse{Code: "invalid_credentials", Message: "current password is invalid"}, nil
		}
		return nil, err
	}
	s.browserSecurity.ClearSessionCookie(ginContext(ctx))
	return api.ChangeCurrentPassword204Response{}, nil
}

func (s *Server) ListUsers(ctx context.Context, request api.ListUsersRequestObject) (api.ListUsersResponseObject, error) {
	limit, cursor := 20, ""
	if request.Params.Limit != nil {
		limit = *request.Params.Limit
	}
	if request.Params.Cursor != nil {
		cursor = *request.Params.Cursor
	}
	page, err := s.identities.ListUsers(httpRequestContext(ctx), requestCaller(ctx), limit, cursor)
	if err != nil {
		if errors.Is(err, identity.ErrInvalidCursor) {
			return api.ListUsers400JSONResponse{Code: "invalid_cursor", Message: "user cursor is invalid"}, nil
		}
		if identityForbidden(err) {
			return api.ListUsers403JSONResponse{Code: "platform_permission_denied", Message: "platform administrator required"}, nil
		}
		return nil, err
	}
	items := make([]api.User, 0, len(page.Items))
	for _, user := range page.Items {
		items = append(items, userResponse(user))
	}
	return api.ListUsers200JSONResponse{Items: items, NextCursor: page.NextCursor}, nil
}

func (s *Server) CreateLocalUser(ctx context.Context, request api.CreateLocalUserRequestObject) (api.CreateLocalUserResponseObject, error) {
	user, err := s.identities.CreateLocalUser(httpRequestContext(ctx), identity.CreateLocalUserCommand{
		Caller: requestCaller(ctx), LoginName: request.Body.LoginName, DisplayName: request.Body.DisplayName,
		TemporaryPassword: stringValue(request.Body.TemporaryPassword),
	})
	if err != nil {
		switch {
		case errors.Is(err, identity.ErrInvalidCommand), errors.Is(err, identity.ErrInvalidPassword):
			return api.CreateLocalUser400JSONResponse{Code: "invalid_user", Message: "user input is invalid"}, nil
		case identityForbidden(err):
			return api.CreateLocalUser403JSONResponse{Code: "platform_permission_denied", Message: "recent platform administrator authentication required"}, nil
		case errors.Is(err, identity.ErrLoginNameConflict):
			return api.CreateLocalUser409JSONResponse{Code: "login_name_conflict", Message: "login name is already in use"}, nil
		default:
			return nil, err
		}
	}
	return api.CreateLocalUser201JSONResponse(userResponse(user)), nil
}

func (s *Server) GetUser(ctx context.Context, request api.GetUserRequestObject) (api.GetUserResponseObject, error) {
	user, err := s.identities.GetUser(httpRequestContext(ctx), requestCaller(ctx), request.UserId)
	if err != nil {
		if identityForbidden(err) {
			return api.GetUser403JSONResponse{Code: "platform_permission_denied", Message: "platform administrator required"}, nil
		}
		if errors.Is(err, identity.ErrUserNotFound) {
			return api.GetUser404JSONResponse{Code: "user_not_found", Message: "user not found"}, nil
		}
		return nil, err
	}
	return api.GetUser200JSONResponse(userResponse(user)), nil
}

func (s *Server) ChangeUserPlatformRole(ctx context.Context, request api.ChangeUserPlatformRoleRequestObject) (api.ChangeUserPlatformRoleResponseObject, error) {
	user, err := s.identities.ChangePlatformRole(httpRequestContext(ctx), identity.ChangePlatformRoleCommand{
		Caller: requestCaller(ctx), UserID: request.UserId, Role: string(request.Body.Role),
	})
	if err != nil {
		if identityForbidden(err) {
			return api.ChangeUserPlatformRole403JSONResponse{Code: "platform_permission_denied", Message: "recent platform administrator authentication required"}, nil
		}
		if errors.Is(err, identity.ErrUserNotFound) {
			return api.ChangeUserPlatformRole404JSONResponse{Code: "user_not_found", Message: "user not found"}, nil
		}
		if errors.Is(err, identity.ErrLastAdministrator) {
			return api.ChangeUserPlatformRole409JSONResponse{Code: "last_platform_administrator", Message: "platform must retain an administrator"}, nil
		}
		return nil, err
	}
	return api.ChangeUserPlatformRole200JSONResponse(userResponse(user)), nil
}

func (s *Server) DisableUser(ctx context.Context, request api.DisableUserRequestObject) (api.DisableUserResponseObject, error) {
	user, err := s.identities.DisableUser(httpRequestContext(ctx), identity.DisableUserCommand{Caller: requestCaller(ctx), UserID: request.UserId})
	if err != nil {
		if identityForbidden(err) {
			return api.DisableUser403JSONResponse{Code: "platform_permission_denied", Message: "recent platform administrator authentication required"}, nil
		}
		if errors.Is(err, identity.ErrUserNotFound) {
			return api.DisableUser404JSONResponse{Code: "user_not_found", Message: "user not found"}, nil
		}
		if errors.Is(err, identity.ErrLastAdministrator) || errors.Is(err, identity.ErrLastProjectOwner) {
			return api.DisableUser409JSONResponse{Code: "identity_invariant", Message: "user is the last effective administrator or project owner"}, nil
		}
		return nil, err
	}
	return api.DisableUser200JSONResponse(userResponse(user)), nil
}

func (s *Server) EnableUser(ctx context.Context, request api.EnableUserRequestObject) (api.EnableUserResponseObject, error) {
	user, err := s.identities.EnableUser(httpRequestContext(ctx), identity.EnableUserCommand{Caller: requestCaller(ctx), UserID: request.UserId})
	if err != nil {
		if identityForbidden(err) {
			return api.EnableUser403JSONResponse{Code: "platform_permission_denied", Message: "recent platform administrator authentication required"}, nil
		}
		if errors.Is(err, identity.ErrUserNotFound) {
			return api.EnableUser404JSONResponse{Code: "user_not_found", Message: "user not found"}, nil
		}
		return nil, err
	}
	return api.EnableUser200JSONResponse(userResponse(user)), nil
}

func (s *Server) ResetUserLocalPassword(ctx context.Context, request api.ResetUserLocalPasswordRequestObject) (api.ResetUserLocalPasswordResponseObject, error) {
	err := s.identities.ResetLocalPassword(httpRequestContext(ctx), identity.ResetLocalPasswordCommand{
		Caller: requestCaller(ctx), UserID: request.UserId, TemporaryPassword: stringValue(request.Body.TemporaryPassword),
		LoginName: optionalString(request.Body.LoginName),
	})
	if err != nil {
		if errors.Is(err, identity.ErrInvalidCommand) || errors.Is(err, identity.ErrInvalidPassword) {
			return api.ResetUserLocalPassword400JSONResponse{Code: "invalid_password", Message: "password does not meet policy"}, nil
		}
		if identityForbidden(err) {
			return api.ResetUserLocalPassword403JSONResponse{Code: "platform_permission_denied", Message: "recent platform administrator authentication required"}, nil
		}
		if errors.Is(err, identity.ErrUserNotFound) {
			return api.ResetUserLocalPassword404JSONResponse{Code: "user_not_found", Message: "user not found"}, nil
		}
		return nil, err
	}
	return api.ResetUserLocalPassword204Response{}, nil
}

func userPrincipalResponse(current identity.CurrentUser) api.CurrentPrincipal {
	user := userResponse(current.User)
	return api.CurrentPrincipal{Kind: api.CurrentPrincipalKindUser, User: &user, MustChangePassword: current.MustChangePassword}
}

func principalResponse(current identity.CurrentPrincipal) api.CurrentPrincipal {
	if current.User != nil {
		return userPrincipalResponse(*current.User)
	}
	response := api.CurrentPrincipal{Kind: api.CurrentPrincipalKindPending, MustChangePassword: false}
	if current.ExternalIdentity != nil {
		value := externalIdentityResponse(*current.ExternalIdentity)
		response.ExternalIdentity = &value
	}
	return response
}

func externalIdentityResponse(item identity.ExternalIdentity) api.ExternalIdentity {
	var email *openapi_types.Email
	if item.Email != nil {
		value := openapi_types.Email(*item.Email)
		email = &value
	}
	return api.ExternalIdentity{Id: item.ID, ProviderId: item.ProviderID, Status: api.ExternalIdentityStatus(item.Status),
		UserId: item.UserID, DisplayName: item.DisplayName, Email: email, EmailVerified: item.EmailVerified,
		CreatedAt: item.CreatedAt, UpdatedAt: item.UpdatedAt}
}

func userResponse(user identity.User) api.User {
	return api.User{Id: user.ID, DisplayName: user.DisplayName, Status: api.UserStatus(user.Status),
		PlatformRole: api.PlatformRole(user.PlatformRole), CreatedAt: user.CreatedAt}
}

func identityForbidden(err error) bool {
	return errors.Is(err, identity.ErrForbidden) || errors.Is(err, identity.ErrRecentAuthenticationRequired) ||
		errors.Is(err, identity.ErrPasswordChangeRequired)
}

func requestSourceIP(ctx context.Context) string {
	remote := ginContext(ctx).Request.RemoteAddr
	host, _, err := net.SplitHostPort(remote)
	if err == nil {
		return host
	}
	return remote
}

func ginContext(ctx context.Context) *gin.Context {
	ginCtx, _ := ctx.(*gin.Context)
	return ginCtx
}

func optionalString(value *string) string { return stringValue(value) }
