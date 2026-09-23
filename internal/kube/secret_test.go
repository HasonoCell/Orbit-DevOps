package kube_test

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"testing"
	"time"

	"github.com/HasonoCell/Orbit-DevOps/internal/kube"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

// TestVerifyTLSSecretBinding 在 Kubernetes 读取边界验证证书与私钥配对以及 hostname。
func TestVerifyTLSSecretBinding(t *testing.T) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	der, err := x509.CreateCertificate(rand.Reader, &x509.Certificate{SerialNumber: big.NewInt(1),
		Subject: pkix.Name{CommonName: "pay.example.test"}, DNSNames: []string{"pay.example.test"},
		NotBefore: now.Add(-time.Hour), NotAfter: now.Add(time.Hour)},
		&x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "pay.example.test"},
			DNSNames: []string{"pay.example.test"}, NotBefore: now.Add(-time.Hour), NotAfter: now.Add(time.Hour)},
		&key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	privateKey, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "pay-tls", Namespace: "task-ns"},
		Type: corev1.SecretTypeTLS, Data: map[string][]byte{
			corev1.TLSCertKey:       pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
			corev1.TLSPrivateKeyKey: pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: privateKey}),
		}}
	adapter, err := kube.New(fake.NewClientset(secret), kube.Config{ClusterRef: "kind-task", Namespace: "task-ns", FieldManager: "test"})
	if err != nil {
		t.Fatal(err)
	}
	if err := adapter.VerifyTLSSecret(context.Background(), "task-ns", "pay-tls", "pay.example.test"); err != nil {
		t.Fatalf("registered certificate rejected: %v", err)
	}
	if err := adapter.VerifyTLSSecret(context.Background(), "other-ns", "pay-tls", "pay.example.test"); err == nil {
		t.Fatal("cross-namespace Secret was accepted")
	}
	if err := adapter.VerifyTLSSecret(context.Background(), "task-ns", "pay-tls", "other.example.test"); err == nil {
		t.Fatal("certificate for different hostname was accepted")
	}
}
