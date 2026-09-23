package kube

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

var ErrTLSSecretInvalid = errors.New("TLS Secret is unavailable or invalid")

// VerifyTLSSecret 仅在平台授权登记时检查已有证书和私钥是否配对；材料只留在进程内存。
func (a *Adapter) VerifyTLSSecret(ctx context.Context, namespace, name, hostname string) error {
	if namespace != a.config.Namespace || name == "" || hostname == "" {
		return ErrTLSSecretInvalid
	}
	secret, err := a.client.CoreV1().Secrets(namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil || secret.Type != corev1.SecretTypeTLS {
		return ErrTLSSecretInvalid
	}
	pair, err := tls.X509KeyPair(secret.Data[corev1.TLSCertKey], secret.Data[corev1.TLSPrivateKeyKey])
	if err != nil || len(pair.Certificate) == 0 {
		return ErrTLSSecretInvalid
	}
	certificate, err := x509.ParseCertificate(pair.Certificate[0])
	if err != nil || certificate.VerifyHostname(hostname) != nil {
		return ErrTLSSecretInvalid
	}
	now := time.Now().UTC()
	if now.Before(certificate.NotBefore) || !now.Before(certificate.NotAfter) {
		return ErrTLSSecretInvalid
	}
	return nil
}
