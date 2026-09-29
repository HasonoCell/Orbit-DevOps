package httpapi

import (
	"context"
	"errors"

	"github.com/HasonoCell/Orbit-DevOps/internal/api"
	"github.com/HasonoCell/Orbit-DevOps/internal/identity"
)

func (s *Server) StartOIDCLogin(ctx context.Context, request api.StartOIDCLoginRequestObject) (api.StartOIDCLoginResponseObject, error) {
	started, err := s.identities.StartOIDC(httpRequestContext(ctx), identity.OIDCStartCommand{ProviderID: request.ProviderId, Mode: "login"})
	if err != nil {
		if errors.Is(err, identity.ErrOIDCUnavailable) {
			return api.StartOIDCLogin503JSONResponse{Code: "oidc_unavailable", Message: "OIDC provider unavailable"}, nil
		}
		return nil, err
	}
	if err := s.browserSecurity.SetOIDCBrowserCookie(ginContext(ctx), started.BrowserToken, started.ExpiresAt); err != nil {
		return nil, err
	}
	return api.StartOIDCLogin200JSONResponse{AuthorizationUrl: started.AuthorizationURL, ExpiresAt: started.ExpiresAt}, nil
}

func (s *Server) StartOIDCReauthentication(ctx context.Context, request api.StartOIDCReauthenticationRequestObject) (api.StartOIDCReauthenticationResponseObject, error) {
	started, err := s.identities.StartOIDC(httpRequestContext(ctx), identity.OIDCStartCommand{
		ProviderID: request.ProviderId, Mode: "reauth", Caller: requestCaller(ctx),
	})
	if err != nil {
		if errors.Is(err, identity.ErrOIDCUnavailable) {
			return api.StartOIDCReauthentication503JSONResponse{Code: "oidc_unavailable", Message: "OIDC provider unavailable"}, nil
		}
		return nil, err
	}
	if err := s.browserSecurity.SetOIDCBrowserCookie(ginContext(ctx), started.BrowserToken, started.ExpiresAt); err != nil {
		return nil, err
	}
	return api.StartOIDCReauthentication200JSONResponse{AuthorizationUrl: started.AuthorizationURL, ExpiresAt: started.ExpiresAt}, nil
}

func (s *Server) StartExternalIdentityBinding(ctx context.Context, request api.StartExternalIdentityBindingRequestObject) (api.StartExternalIdentityBindingResponseObject, error) {
	started, err := s.identities.StartOIDC(httpRequestContext(ctx), identity.OIDCStartCommand{
		ProviderID: request.ProviderId, Mode: "bind", Caller: requestCaller(ctx),
	})
	if err != nil {
		if identityForbidden(err) {
			return api.StartExternalIdentityBinding403JSONResponse(identityForbiddenError(err, "permission_denied", "identity binding is forbidden")), nil
		}
		return nil, err
	}
	if err := s.browserSecurity.SetOIDCBrowserCookie(ginContext(ctx), started.BrowserToken, started.ExpiresAt); err != nil {
		return nil, err
	}
	return api.StartExternalIdentityBinding200JSONResponse{AuthorizationUrl: started.AuthorizationURL, ExpiresAt: started.ExpiresAt}, nil
}

func (s *Server) CompleteOIDC(ctx context.Context, request api.CompleteOIDCRequestObject) (api.CompleteOIDCResponseObject, error) {
	ginCtx := ginContext(ctx)
	browser, err := s.browserSecurity.OIDCBrowserCookieValue(ginCtx.Request)
	if err != nil {
		return api.CompleteOIDC400JSONResponse{Code: "invalid_oidc_transaction", Message: "OIDC transaction is invalid or expired"}, nil
	}
	completion, err := s.identities.CompleteOIDC(httpRequestContext(ctx), request.Params.State, browser, request.Params.Code)
	s.browserSecurity.ClearOIDCBrowserCookie(ginCtx)
	if err != nil {
		switch {
		case errors.Is(err, identity.ErrOIDCTransaction), errors.Is(err, identity.ErrOIDCProof):
			return api.CompleteOIDC400JSONResponse{Code: "invalid_oidc_proof", Message: "OIDC proof is invalid or expired"}, nil
		case errors.Is(err, identity.ErrAdmissionRejected), errors.Is(err, identity.ErrIdentityAlreadyBound):
			return api.CompleteOIDC403JSONResponse{Code: "oidc_identity_rejected", Message: "external identity cannot be used"}, nil
		case errors.Is(err, identity.ErrOIDCUnavailable):
			return api.CompleteOIDC503JSONResponse{Code: "oidc_unavailable", Message: "OIDC provider unavailable"}, nil
		default:
			return nil, err
		}
	}
	if completion.RequiresRelogin {
		s.browserSecurity.ClearSessionCookie(ginCtx)
	} else if completion.Token.CookieValue() != "" {
		result := identity.LoginResult{Token: completion.Token, ExpiresAt: completion.ExpiresAt}
		if completion.CurrentUser != nil {
			result.CurrentUser = *completion.CurrentUser
		}
		if err := s.browserSecurity.SetSessionCookie(ginCtx, result); err != nil {
			return nil, err
		}
	}
	location := s.browserSecurity.OIDCCallbackRedirect()
	return api.CompleteOIDC303Response{Headers: api.CompleteOIDC303ResponseHeaders{Location: &location}}, nil
}

func (s *Server) ListCurrentSessions(ctx context.Context, request api.ListCurrentSessionsRequestObject) (api.ListCurrentSessionsResponseObject, error) {
	limit, cursor := projectListParameters(request.Params.Limit, request.Params.Cursor)
	page, err := s.identities.ListSessions(httpRequestContext(ctx), requestCaller(ctx), limit, cursor)
	if err != nil {
		if errors.Is(err, identity.ErrInvalidCursor) {
			return api.ListCurrentSessions400JSONResponse{Code: "invalid_cursor", Message: "session cursor is invalid"}, nil
		}
		return nil, err
	}
	items := make([]api.SessionSummary, 0, len(page.Items))
	for _, item := range page.Items {
		items = append(items, api.SessionSummary{Id: item.ID, Method: api.SessionSummaryMethod(item.Method), ProviderId: item.ProviderID,
			CreatedAt: item.CreatedAt, LastSeenAt: item.LastSeenAt, ExpiresAt: item.ExpiresAt, Current: item.Current})
	}
	return api.ListCurrentSessions200JSONResponse{Items: items, NextCursor: page.NextCursor}, nil
}

func (s *Server) ListCurrentExternalIdentities(ctx context.Context, request api.ListCurrentExternalIdentitiesRequestObject) (api.ListCurrentExternalIdentitiesResponseObject, error) {
	limit, cursor := projectListParameters(request.Params.Limit, request.Params.Cursor)
	page, err := s.identities.ListExternalIdentities(httpRequestContext(ctx), requestCaller(ctx), limit, cursor)
	if err != nil {
		if errors.Is(err, identity.ErrInvalidCursor) {
			return api.ListCurrentExternalIdentities400JSONResponse{Code: "invalid_cursor", Message: "external identity cursor is invalid"}, nil
		}
		return nil, err
	}
	items := make([]api.ExternalIdentity, 0, len(page.Items))
	for _, item := range page.Items {
		items = append(items, externalIdentityResponse(item))
	}
	return api.ListCurrentExternalIdentities200JSONResponse{Items: items, NextCursor: page.NextCursor}, nil
}

func (s *Server) UnbindExternalIdentity(ctx context.Context, request api.UnbindExternalIdentityRequestObject) (api.UnbindExternalIdentityResponseObject, error) {
	err := s.identities.UnbindExternalIdentity(httpRequestContext(ctx), requestCaller(ctx), request.IdentityId)
	if err != nil {
		if identityForbidden(err) {
			return api.UnbindExternalIdentity403JSONResponse(identityForbiddenError(err, "permission_denied", "identity unbinding is forbidden")), nil
		}
		if errors.Is(err, identity.ErrUserNotFound) {
			return api.UnbindExternalIdentity404JSONResponse{Code: "external_identity_not_found", Message: "external identity not found"}, nil
		}
		if errors.Is(err, identity.ErrLastLoginMethod) {
			return api.UnbindExternalIdentity409JSONResponse{Code: "last_login_method", Message: "user must retain a login method"}, nil
		}
		return nil, err
	}
	s.browserSecurity.ClearSessionCookie(ginContext(ctx))
	return api.UnbindExternalIdentity204Response{}, nil
}

func (s *Server) ListAdmissions(ctx context.Context, request api.ListAdmissionsRequestObject) (api.ListAdmissionsResponseObject, error) {
	limit, cursor := projectListParameters(request.Params.Limit, request.Params.Cursor)
	status := ""
	if request.Params.Status != nil {
		status = string(*request.Params.Status)
	}
	page, err := s.identities.ListAdmissionsByStatus(httpRequestContext(ctx), requestCaller(ctx), limit, cursor, status)
	if err != nil {
		if errors.Is(err, identity.ErrInvalidCursor) {
			return api.ListAdmissions400JSONResponse{Code: "invalid_cursor", Message: "admission cursor is invalid"}, nil
		}
		if identityForbidden(err) {
			return api.ListAdmissions403JSONResponse(identityForbiddenError(err, "platform_permission_denied", "platform administrator required")), nil
		}
		return nil, err
	}
	items := make([]api.ExternalIdentity, 0, len(page.Items))
	for _, item := range page.Items {
		items = append(items, externalIdentityResponse(item))
	}
	return api.ListAdmissions200JSONResponse{Items: items, NextCursor: page.NextCursor}, nil
}

func (s *Server) GetAdmission(ctx context.Context, request api.GetAdmissionRequestObject) (api.GetAdmissionResponseObject, error) {
	item, err := s.identities.GetAdmission(httpRequestContext(ctx), requestCaller(ctx), request.IdentityId)
	if err != nil {
		switch {
		case identityForbidden(err):
			return api.GetAdmission403JSONResponse(identityForbiddenError(err, "platform_permission_denied", "platform administrator required")), nil
		case errors.Is(err, identity.ErrUserNotFound):
			return api.GetAdmission404JSONResponse{Code: "admission_not_found", Message: "admission not found"}, nil
		default:
			return nil, err
		}
	}
	return api.GetAdmission200JSONResponse(externalIdentityResponse(item)), nil
}

func (s *Server) ApproveAdmission(ctx context.Context, request api.ApproveAdmissionRequestObject) (api.ApproveAdmissionResponseObject, error) {
	user, err := s.identities.ApproveAdmission(httpRequestContext(ctx), requestCaller(ctx), request.IdentityId)
	if err != nil {
		if identityForbidden(err) {
			return api.ApproveAdmission403JSONResponse(identityForbiddenError(err, "platform_permission_denied", "platform administrator required")), nil
		}
		if errors.Is(err, identity.ErrUserNotFound) {
			return api.ApproveAdmission404JSONResponse{Code: "admission_not_found", Message: "admission not found"}, nil
		}
		if errors.Is(err, identity.ErrAdmissionState) {
			return api.ApproveAdmission409JSONResponse{Code: "admission_state_conflict", Message: "admission state does not allow approval"}, nil
		}
		return nil, err
	}
	return api.ApproveAdmission200JSONResponse(userResponse(user)), nil
}

func (s *Server) RejectAdmission(ctx context.Context, request api.RejectAdmissionRequestObject) (api.RejectAdmissionResponseObject, error) {
	item, err := s.identities.RejectAdmission(httpRequestContext(ctx), requestCaller(ctx), request.IdentityId)
	if response, handled := rejectAdmissionError(err); handled {
		return response, nil
	} else if err != nil {
		return nil, err
	}
	return api.RejectAdmission200JSONResponse(externalIdentityResponse(item)), nil
}

func (s *Server) ReopenAdmission(ctx context.Context, request api.ReopenAdmissionRequestObject) (api.ReopenAdmissionResponseObject, error) {
	item, err := s.identities.ReopenAdmission(httpRequestContext(ctx), requestCaller(ctx), request.IdentityId)
	if err != nil {
		if identityForbidden(err) {
			return api.ReopenAdmission403JSONResponse(identityForbiddenError(err, "platform_permission_denied", "platform administrator required")), nil
		}
		if errors.Is(err, identity.ErrUserNotFound) {
			return api.ReopenAdmission404JSONResponse{Code: "admission_not_found", Message: "admission not found"}, nil
		}
		if errors.Is(err, identity.ErrAdmissionState) {
			return api.ReopenAdmission409JSONResponse{Code: "admission_state_conflict", Message: "admission state does not allow reopening"}, nil
		}
		return nil, err
	}
	return api.ReopenAdmission200JSONResponse(externalIdentityResponse(item)), nil
}

func rejectAdmissionError(err error) (api.RejectAdmissionResponseObject, bool) {
	if err == nil {
		return nil, false
	}
	if identityForbidden(err) {
		return api.RejectAdmission403JSONResponse(identityForbiddenError(err, "platform_permission_denied", "platform administrator required")), true
	}
	if errors.Is(err, identity.ErrUserNotFound) {
		return api.RejectAdmission404JSONResponse{Code: "admission_not_found", Message: "admission not found"}, true
	}
	if errors.Is(err, identity.ErrAdmissionState) {
		return api.RejectAdmission409JSONResponse{Code: "admission_state_conflict", Message: "admission state does not allow rejection"}, true
	}
	return nil, false
}
