package httpapi

import (
	"context"
	"errors"

	"github.com/HasonoCell/Orbit-DevOps/internal/access"
	"github.com/HasonoCell/Orbit-DevOps/internal/api"
	"github.com/HasonoCell/Orbit-DevOps/internal/observability"
	"github.com/HasonoCell/Orbit-DevOps/internal/projectauth"
)

func (s *Server) GetAccessHostOptions(ctx context.Context, request api.GetAccessHostOptionsRequestObject) (api.GetAccessHostOptionsResponseObject, error) {
	observability.SetRequestProjectID(ctx, request.ProjectId)
	options, err := s.access.GetHostOptions(httpRequestContext(ctx), request.ProjectId, requestCaller(ctx))
	if err != nil {
		switch {
		case errors.Is(err, projectauth.ErrNotMember):
			return api.GetAccessHostOptions404JSONResponse{Code: "project_not_found", Message: "project not found"}, nil
		case errors.Is(err, projectauth.ErrForbidden):
			return api.GetAccessHostOptions403JSONResponse{Code: "project_permission_denied", Message: "current project role cannot manage access hosts"}, nil
		default:
			return nil, err
		}
	}
	items := make([]api.AccessIssuerPolicy, 0, len(options.IssuerPolicies))
	for _, policy := range options.IssuerPolicies {
		items = append(items, api.AccessIssuerPolicy{Key: policy.Key, Kind: policy.Kind, Name: policy.Name})
	}
	return api.GetAccessHostOptions200JSONResponse{ClusterRef: options.ClusterRef, Namespace: options.Namespace, IssuerPolicies: items}, nil
}

func (s *Server) ListAccessSecretBindingOptions(ctx context.Context, request api.ListAccessSecretBindingOptionsRequestObject) (api.ListAccessSecretBindingOptionsResponseObject, error) {
	observability.SetRequestProjectID(ctx, request.ProjectId)
	options, err := s.access.ListSecretBindingOptions(httpRequestContext(ctx), request.ProjectId, requestCaller(ctx),
		request.Params.Hostname, accessPage(request.Params.Limit, request.Params.Offset))
	if err != nil {
		switch {
		case errors.Is(err, access.ErrInvalidHost), errors.Is(err, access.ErrInvalidPage):
			return api.ListAccessSecretBindingOptions400JSONResponse{Code: "invalid_binding_options", Message: "hostname or page is invalid"}, nil
		case errors.Is(err, projectauth.ErrNotMember):
			return api.ListAccessSecretBindingOptions404JSONResponse{Code: "project_not_found", Message: "project not found"}, nil
		case errors.Is(err, projectauth.ErrForbidden):
			return api.ListAccessSecretBindingOptions403JSONResponse{Code: "project_permission_denied", Message: "current project role cannot manage access hosts"}, nil
		default:
			return nil, err
		}
	}
	result := make(api.ListAccessSecretBindingOptions200JSONResponse, 0, len(options))
	for _, option := range options {
		result = append(result, api.AccessSecretBindingOption{Id: option.ID, SecretName: option.SecretName,
			ClusterRef: option.ClusterRef, Namespace: option.Namespace, Hostname: option.Hostname})
	}
	return result, nil
}
