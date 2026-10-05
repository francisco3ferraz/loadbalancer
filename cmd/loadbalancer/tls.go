package main

import (
	"crypto/tls"
	"fmt"
	"sync/atomic"
)

// certStore holds the certificate served to clients. A reload replaces it
// while handshakes are reading it, so it's an atomic pointer, as in
// balancer.Swapper: connections already open keep the certificate they
// were set up with.
type certStore struct {
	current atomic.Pointer[tls.Certificate]
}

// getCertificate is the server's tls.Config.GetCertificate, called on every
// handshake.
func (s *certStore) getCertificate(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	return s.current.Load(), nil
}

// loadCert reads a certificate and its private key from PEM files. It fails
// if either is missing or malformed, or if they don't match.
func loadCert(certFile, keyFile string) (*tls.Certificate, error) {
	cert, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return nil, fmt.Errorf("load certificate %s: %w", certFile, err)
	}
	return &cert, nil
}
