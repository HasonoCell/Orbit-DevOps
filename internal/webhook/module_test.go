package webhook

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"testing"
)

func TestAcceptRejectsUntrustedInputBeforeDatabaseAccess(t *testing.T) {
	t.Parallel()
	module := New(nil, Config{Endpoints: map[string]EndpointSecrets{"public": {Current: "secret"}}, MaxBodyBytes: 8}, nil, nil)
	if _, err := module.Accept(context.Background(), "missing", Headers{DeliveryID: "one", EventType: "push", Signature: "invalid"}, strings.NewReader("payload")); !errors.Is(err, ErrEndpointNotFound) {
		t.Fatalf("unknown endpoint error = %v", err)
	}
	if _, err := module.Accept(context.Background(), "public", Headers{DeliveryID: "one", EventType: "push", Signature: "invalid"}, strings.NewReader("payload")); !errors.Is(err, ErrInvalidSignature) {
		t.Fatalf("invalid signature error = %v", err)
	}
	if _, err := module.Accept(context.Background(), "public", Headers{DeliveryID: "one", EventType: "push", Signature: "invalid"}, strings.NewReader("payload-too-large")); !errors.Is(err, ErrBodyTooLarge) {
		t.Fatalf("oversized body error = %v", err)
	}
}

func TestValidSignatureAcceptsCurrentAndPreviousSecrets(t *testing.T) {
	t.Parallel()
	body := []byte(`{"ref":"refs/heads/main"}`)
	for _, secret := range []string{"current", "previous"} {
		mac := hmac.New(sha256.New, []byte(secret))
		_, _ = mac.Write(body)
		signature := "sha256=" + hex.EncodeToString(mac.Sum(nil))
		if !validSignature(body, signature, EndpointSecrets{Current: "current", Previous: "previous"}) {
			t.Fatalf("signature using %q was rejected", secret)
		}
	}
	if validSignature(body, "sha256="+strings.Repeat("0", 64), EndpointSecrets{Current: "current"}) {
		t.Fatal("invalid signature was accepted")
	}
}

func TestNormalizeKeepsOnlySupportedPushMetadata(t *testing.T) {
	t.Parallel()
	body := []byte(`{"ref":"refs/heads/main","before":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","after":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb","forced":true,"deleted":false,"repository":{"id":101,"full_name":"example/demo","owner":{"id":202}},"pusher":{"name":"must-not-persist"},"head_commit":{"message":"must-not-persist"}}`)
	delivery := normalize(Headers{DeliveryID: "delivery-1", EventType: "push"}, body)
	if delivery.State != StatePending || delivery.RepositoryID == nil || *delivery.RepositoryID != 101 || delivery.AfterCommit == nil || *delivery.AfterCommit != strings.Repeat("b", 40) {
		t.Fatalf("normalized delivery = %#v", delivery)
	}
	encoded := delivery.RepositoryFullName
	if encoded == nil || *encoded != "example/demo" {
		t.Fatalf("repository name = %v", encoded)
	}
}

func TestNormalizeQuarantinesTrustedButMalformedPush(t *testing.T) {
	t.Parallel()
	delivery := normalize(Headers{DeliveryID: "delivery-2", EventType: "push"}, []byte(`{"ref":`))
	if delivery.State != StateQuarantined || delivery.ReasonCode == nil || *delivery.ReasonCode != "invalid_push_payload" {
		t.Fatalf("malformed delivery = %#v", delivery)
	}
}
