package httpapi

import (
	"context"
	"errors"

	"github.com/HasonoCell/Orbit-DevOps/internal/access"
	"github.com/HasonoCell/Orbit-DevOps/internal/api"
	"github.com/HasonoCell/Orbit-DevOps/internal/idempotency"
	"github.com/HasonoCell/Orbit-DevOps/internal/identity"
)

func accessSecretBindingResponse(binding access.SecretBinding) api.AccessSecretBinding {
	return api.AccessSecretBinding{Id: binding.ID, ProjectId: binding.ProjectID,
		ClusterRef: binding.ClusterRef, Namespace: binding.Namespace, Hostname: binding.Hostname,
		SecretName: binding.SecretName, State: api.AccessSecretBindingState(binding.State),
		CreatedAt: binding.CreatedAt, UpdatedAt: binding.UpdatedAt}
}

func (s *Server) ListAccessSecretBindings(ctx context.Context, _ api.ListAccessSecretBindingsRequestObject) (api.ListAccessSecretBindingsResponseObject, error) {
	bindings, err := s.access.ListSecretBindings(httpRequestContext(ctx), requestCaller(ctx))
	if err != nil {
		if errors.Is(err, identity.ErrForbidden) || errors.Is(err, identity.ErrRecentAuthenticationRequired) {
			return api.ListAccessSecretBindings403JSONResponse{Code: "platform_permission_denied", Message: "platform administrator authorization required"}, nil
		}
		return nil, err
	}
	result := make(api.ListAccessSecretBindings200JSONResponse, 0, len(bindings))
	for _, binding := range bindings {
		result = append(result, accessSecretBindingResponse(binding))
	}
	return result, nil
}

func (s *Server) RegisterAccessSecretBinding(ctx context.Context, request api.RegisterAccessSecretBindingRequestObject) (api.RegisterAccessSecretBindingResponseObject, error) {
	binding, err := s.access.RegisterSecret(httpRequestContext(ctx), access.RegisterSecretCommand{
		ProjectID: request.Body.ProjectId, Hostname: request.Body.Hostname, SecretName: request.Body.SecretName,
		Caller: requestCaller(ctx), IdempotencyKey: request.Params.IdempotencyKey})
	if err != nil {
		switch {
		case errors.Is(err, access.ErrInvalidSecret), errors.Is(err, access.ErrBindingNotFound):
			return api.RegisterAccessSecretBinding400JSONResponse{Code: "invalid_tls_secret_binding", Message: "TLS Secret cannot be registered"}, nil
		case errors.Is(err, identity.ErrForbidden), errors.Is(err, identity.ErrRecentAuthenticationRequired):
			return api.RegisterAccessSecretBinding403JSONResponse{Code: "platform_permission_denied", Message: "recent platform administrator authentication required"}, nil
		case errors.Is(err, access.ErrConflict), errors.Is(err, idempotency.ErrConflict):
			return api.RegisterAccessSecretBinding409JSONResponse{Code: "tls_secret_binding_conflict", Message: "TLS Secret binding or idempotency key conflicts"}, nil
		default:
			return nil, err
		}
	}
	return api.RegisterAccessSecretBinding201JSONResponse(accessSecretBindingResponse(binding)), nil
}

func (s *Server) RevokeAccessSecretBinding(ctx context.Context, request api.RevokeAccessSecretBindingRequestObject) (api.RevokeAccessSecretBindingResponseObject, error) {
	binding, err := s.access.RevokeSecret(httpRequestContext(ctx), access.RevokeSecretCommand{
		BindingID: request.BindingId, Caller: requestCaller(ctx), IdempotencyKey: request.Params.IdempotencyKey})
	if err != nil {
		switch {
		case errors.Is(err, identity.ErrForbidden), errors.Is(err, identity.ErrRecentAuthenticationRequired):
			return api.RevokeAccessSecretBinding403JSONResponse{Code: "platform_permission_denied", Message: "recent platform administrator authentication required"}, nil
		case errors.Is(err, access.ErrBindingNotFound):
			return api.RevokeAccessSecretBinding404JSONResponse{Code: "tls_secret_binding_not_found", Message: "TLS Secret binding not found"}, nil
		case errors.Is(err, idempotency.ErrConflict):
			return api.RevokeAccessSecretBinding409JSONResponse{Code: "idempotency_conflict", Message: "idempotency key conflicts"}, nil
		default:
			return nil, err
		}
	}
	return api.RevokeAccessSecretBinding200JSONResponse(accessSecretBindingResponse(binding)), nil
}
