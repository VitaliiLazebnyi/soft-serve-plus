package serve

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func generateTestCert(t *testing.T, certPath, keyPath, cn string) {
	t.Helper()

	privateKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}

	template := x509.Certificate{
		SerialNumber: nil,
		Subject: pkix.Name{
			CommonName: cn,
		},
		NotBefore: time.Now(),
		NotAfter:  time.Now().Add(time.Hour),
	}

	certBytes, err := x509.CreateCertificate(rand.Reader, &template, &template, &privateKey.PublicKey, privateKey)
	if err != nil {
		t.Fatal(err)
	}

	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certBytes})
	if err := os.WriteFile(certPath, certPEM, 0o600); err != nil {
		t.Fatal(err)
	}

	keyPEM := pem.EncodeToMemory(&pem.Block{
		Type:  "RSA PRIVATE KEY",
		Bytes: x509.MarshalPKCS1PrivateKey(privateKey),
	})
	if err := os.WriteFile(keyPath, keyPEM, 0o600); err != nil {
		t.Fatal(err)
	}
}

// certCN returns the common name of the leaf certificate in cert.
func certCN(t *testing.T, cert *tls.Certificate) string {
	t.Helper()
	if cert == nil || len(cert.Certificate) == 0 {
		t.Fatal("no certificate")
	}
	leaf, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	return leaf.Subject.CommonName
}

// Reload replaces the certificate GetCertificate returns with the one on
// disk. "soft serve" calls it on SIGHUP; TestServeReloadsCertificateOnSIGHUP
// covers that wiring.
func TestCertReloader(t *testing.T) {
	dir := t.TempDir()
	certPath := filepath.Join(dir, "cert.pem")
	keyPath := filepath.Join(dir, "key.pem")

	generateTestCert(t, certPath, keyPath, "cert-v1")

	certReloader, err := NewCertReloader(certPath, keyPath)
	if err != nil {
		t.Fatalf("failed to create reloader: %v", err)
	}
	getCert := certReloader.GetCertificateFunc()

	cert1, err := getCert(nil)
	if err != nil {
		t.Fatal(err)
	}
	if cn := certCN(t, cert1); cn != "cert-v1" {
		t.Fatalf("initial certificate is %q, want cert-v1", cn)
	}

	// Replace the certificate on disk: nothing changes until Reload.
	generateTestCert(t, certPath, keyPath, "cert-v2")
	cert, err := getCert(nil)
	if err != nil {
		t.Fatal(err)
	}
	if cn := certCN(t, cert); cn != "cert-v1" {
		t.Fatalf("certificate before Reload is %q, want cert-v1", cn)
	}

	if err := certReloader.Reload(); err != nil {
		t.Fatalf("Reload: %v", err)
	}
	cert2, err := getCert(nil)
	if err != nil {
		t.Fatal(err)
	}
	if cn := certCN(t, cert2); cn != "cert-v2" {
		t.Fatalf("certificate after Reload is %q, want cert-v2", cn)
	}
}
