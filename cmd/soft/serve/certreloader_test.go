//go:build unix

package serve

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
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

func TestCertReloader(t *testing.T) {
	dir := t.TempDir()
	certPath := filepath.Join(dir, "/cert.pem")
	keyPath := filepath.Join(dir, "/key.pem")

	// Initial cert
	generateTestCert(t, certPath, keyPath, "cert-v1")

	certReloader, err := NewCertReloader(certPath, keyPath)
	if err != nil {
		t.Fatalf("failed to create reloader: %v", err)
	}

	go func() {
		sigCh := make(chan os.Signal, 1)
		signal.Notify(sigCh, syscall.SIGHUP)
		for range sigCh {
			// The test checks below that the certificate changed.
			_ = certReloader.Reload()
		}
	}()

	getCert := certReloader.GetCertificateFunc()

	cert1, err := getCert(nil)
	if err != nil {
		t.Fatal(err)
	}

	// Replace cert on disk
	generateTestCert(t, certPath, keyPath, "cert-v2")

	// Trigger reload
	if err := syscall.Kill(os.Getpid(), syscall.SIGHUP); err != nil {
		t.Fatalf("failed to send SIGHUP: %v", err)
	}

	// Allow async goroutine to reload
	time.Sleep(100 * time.Millisecond)

	cert2, err := getCert(nil)
	if err != nil {
		t.Fatal(err)
	}

	if cert1 == cert2 {
		t.Fatal("certificate was not reloaded after SIGHUP")
	}
}
