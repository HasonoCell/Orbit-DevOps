package httpapi

import (
	"context"
	"errors"

	"github.com/HasonoCell/Orbit-DevOps/internal/api"
	"github.com/HasonoCell/Orbit-DevOps/internal/webhook"
	"github.com/gin-gonic/gin"
)

// AcceptGitHubWebhook 把未经解析的 Body 交给验签模块；OpenAPI 不预先绑定 JSON。
func (s *Server) AcceptGitHubWebhook(ctx context.Context, request api.AcceptGitHubWebhookRequestObject) (api.AcceptGitHubWebhookResponseObject, error) {
	ginContext, ok := ctx.(*gin.Context)
	if !ok {
		return nil, errors.New("github webhook requires gin request context")
	}
	result, err := s.webhooks.Accept(ginContext.Request.Context(), request.EndpointKey, webhook.Headers{DeliveryID: request.Params.XGitHubDelivery, EventType: request.Params.XGitHubEvent, Signature: request.Params.XHubSignature256}, ginContext.Request.Body)
	if err != nil {
		switch {
		case errors.Is(err, webhook.ErrMissingHeaders):
			return api.AcceptGitHubWebhook400JSONResponse{Code: "github_headers_missing", Message: err.Error()}, nil
		case errors.Is(err, webhook.ErrInvalidSignature):
			return api.AcceptGitHubWebhook401JSONResponse{Code: "github_signature_invalid", Message: err.Error()}, nil
		case errors.Is(err, webhook.ErrEndpointNotFound):
			return api.AcceptGitHubWebhook404JSONResponse{Code: "github_endpoint_not_found", Message: "webhook endpoint not found"}, nil
		case errors.Is(err, webhook.ErrSecurityConflict):
			return api.AcceptGitHubWebhook409JSONResponse{Code: "github_delivery_security_conflict", Message: err.Error()}, nil
		case errors.Is(err, webhook.ErrBodyTooLarge):
			return api.AcceptGitHubWebhook413JSONResponse{Code: "github_payload_too_large", Message: err.Error()}, nil
		default:
			return api.AcceptGitHubWebhook503JSONResponse{Code: "webhook_store_unavailable", Message: "webhook delivery could not be durably accepted"}, nil
		}
	}
	return api.AcceptGitHubWebhook202JSONResponse{DeliveryId: result.Delivery.ID, State: api.WebhookAcceptanceState(result.Delivery.State), Replay: result.Replay}, nil
}
