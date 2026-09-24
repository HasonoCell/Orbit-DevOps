package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/HasonoCell/Orbit-DevOps/internal/api"
	"github.com/HasonoCell/Orbit-DevOps/internal/identity"
	"github.com/gin-gonic/gin"
)

func TestIdentityForbiddenErrorDistinguishesReason(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name, code string
		err        error
	}{
		{name: "missing role", err: identity.ErrForbidden, code: "platform_permission_denied"},
		{name: "recent proof expired", err: identity.ErrRecentAuthenticationRequired, code: "recent_authentication_required"},
		{name: "temporary password", err: identity.ErrPasswordChangeRequired, code: "password_change_required"},
	} {
		t.Run(test.name, func(t *testing.T) {
			response := identityForbiddenError(test.err, "platform_permission_denied", "platform administrator required")
			if response.Code != test.code {
				t.Fatalf("error code = %q, want %q", response.Code, test.code)
			}
		})
	}
}

func TestStrictHandlerPreservesRecentAuthenticationReason(t *testing.T) {
	t.Parallel()
	response := httptest.NewRecorder()
	context, _ := gin.CreateTestContext(response)
	StrictHandlerOptions().HandlerErrorFunc(context, identity.ErrRecentAuthenticationRequired)
	var body api.Error
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if response.Code != http.StatusForbidden || body.Code != "recent_authentication_required" {
		t.Fatalf("status = %d, code = %q", response.Code, body.Code)
	}
}
