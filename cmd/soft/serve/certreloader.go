package serve

import (
	"crypto/tls"
	"sync"
)

// CertReloader holds the TLS certificate and key the HTTP server presents and
// reloads them from disk when asked. "soft serve" calls Reload on SIGHUP.
type CertReloader struct {
	certMu   sync.RWMutex
	cert     *tls.Certificate
	certPath string
	keyPath  string
}

// NewCertReloader creates a new CertReloader and loads the certificate and
// key.
func NewCertReloader(certPath, keyPath string) (*CertReloader, error) {
	reloader := &CertReloader{
		certPath: certPath,
		keyPath:  keyPath,
	}

	cert, err := tls.LoadX509KeyPair(certPath, keyPath)
	if err != nil {
		return nil, err
	}
	reloader.cert = &cert

	return reloader, nil
}

// Reload attempts to reload the certificate and key.
func (cr *CertReloader) Reload() error {
	newCert, err := tls.LoadX509KeyPair(cr.certPath, cr.keyPath)
	if err != nil {
		return err
	}

	cr.certMu.Lock()
	defer cr.certMu.Unlock()
	cr.cert = &newCert
	return nil
}

// GetCertificateFunc returns a function that can be used with tls.Config.GetCertificate.
func (cr *CertReloader) GetCertificateFunc() func(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	return func(clientHello *tls.ClientHelloInfo) (*tls.Certificate, error) {
		cr.certMu.RLock()
		defer cr.certMu.RUnlock()
		return cr.cert, nil
	}
}
