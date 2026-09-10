package integration_test

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

type webhookAcceptanceDocument struct {
	DeliveryID string `json:"deliveryId"`
	State      string `json:"state"`
	Replay     bool   `json:"replay"`
}

func TestGitHubWebhookAcceptanceVerifiesDeduplicatesAndQuarantines(t *testing.T) {
	environment := newTestEnvironment(t)
	push := `{"ref":"refs/heads/main","before":"` + strings.Repeat("a", 40) + `","after":"` + strings.Repeat("b", 40) + `","forced":false,"deleted":false,"repository":{"id":101,"full_name":"example/demo","owner":{"id":202}}}`
	first := postGitHubWebhook(t, environment, "delivery-1", "push", push, "integration-webhook-secret")
	defer first.Body.Close()
	if first.StatusCode != http.StatusAccepted {
		t.Fatalf("first webhook status = %d", first.StatusCode)
	}
	var accepted webhookAcceptanceDocument
	if err := json.NewDecoder(first.Body).Decode(&accepted); err != nil {
		t.Fatal(err)
	}
	if accepted.State != "pending" || accepted.Replay {
		t.Fatalf("first webhook = %#v", accepted)
	}

	replay := postGitHubWebhook(t, environment, "delivery-1", "push", push, "integration-webhook-secret")
	defer replay.Body.Close()
	if replay.StatusCode != http.StatusAccepted {
		t.Fatalf("replay status = %d", replay.StatusCode)
	}
	var replayed webhookAcceptanceDocument
	if err := json.NewDecoder(replay.Body).Decode(&replayed); err != nil {
		t.Fatal(err)
	}
	if !replayed.Replay || replayed.DeliveryID != accepted.DeliveryID {
		t.Fatalf("replay = %#v", replayed)
	}

	conflict := postGitHubWebhook(t, environment, "delivery-1", "push", push+" ", "integration-webhook-secret")
	defer conflict.Body.Close()
	if conflict.StatusCode != http.StatusConflict {
		t.Fatalf("conflict status = %d", conflict.StatusCode)
	}

	malformed := postGitHubWebhook(t, environment, "delivery-2", "push", `{"ref":`, "integration-webhook-secret")
	defer malformed.Body.Close()
	if malformed.StatusCode != http.StatusAccepted {
		t.Fatalf("malformed status = %d", malformed.StatusCode)
	}
	var quarantined webhookAcceptanceDocument
	if err := json.NewDecoder(malformed.Body).Decode(&quarantined); err != nil {
		t.Fatal(err)
	}
	if quarantined.State != "quarantined" {
		t.Fatalf("malformed webhook = %#v", quarantined)
	}

	invalid := postGitHubWebhook(t, environment, "delivery-3", "push", push, "wrong-secret")
	defer invalid.Body.Close()
	if invalid.StatusCode != http.StatusUnauthorized {
		t.Fatalf("invalid signature status = %d", invalid.StatusCode)
	}
}

func postGitHubWebhook(t *testing.T, environment *testEnvironment, deliveryID, eventType, body, secret string) *http.Response {
	t.Helper()
	request, err := http.NewRequestWithContext(context.Background(), http.MethodPost, environment.server.URL+"/api/v1/webhooks/github/integration", bytes.NewBufferString(body))
	if err != nil {
		t.Fatal(err)
	}
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write([]byte(body))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-GitHub-Delivery", deliveryID)
	request.Header.Set("X-GitHub-Event", eventType)
	request.Header.Set("X-Hub-Signature-256", "sha256="+hex.EncodeToString(mac.Sum(nil)))
	response, err := environment.server.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	return response
}
