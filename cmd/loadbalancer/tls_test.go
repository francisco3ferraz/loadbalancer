package main

import (
	"bufio"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// writeCert writes a self-signed certificate for 127.0.0.1, with name as its
// common name, and its private key, and returns the certificate. Generated
// rather than committed, so it can't expire and break the tests.
func writeCert(t *testing.T, certFile, keyFile, name string) *x509.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		Subject:     pkix.Name{CommonName: name},
		IPAddresses: []net.IP{net.IPv4(127, 0, 0, 1)},
		NotBefore:   time.Now().Add(-time.Hour),
		NotAfter:    time.Now().Add(time.Hour),
		// Its own CA, so a client can trust it by adding it to RootCAs.
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, certFile, string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})))
	writeFile(t, keyFile, string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})))

	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return cert
}

// tlsFiles returns paths for a certificate and key in a temporary directory.
func tlsFiles(t *testing.T) (certFile, keyFile string) {
	t.Helper()
	dir := t.TempDir()
	return filepath.Join(dir, "cert.pem"), filepath.Join(dir, "key.pem")
}

func tlsConfigFor(backendURL, certFile, keyFile string) string {
	return configFor(backendURL) + fmt.Sprintf("tls_cert: %s\ntls_key: %s\n", certFile, keyFile)
}

// tlsClient returns a client that trusts certs. Each client has its own
// transport, so its first request makes a new connection and handshake.
func tlsClient(certs ...*x509.Certificate) *http.Client {
	pool := x509.NewCertPool()
	for _, c := range certs {
		pool.AddCert(c)
	}
	return &http.Client{Transport: &http.Transport{
		TLSClientConfig: &tls.Config{RootCAs: pool},
		// A custom TLSClientConfig turns HTTP/2 off unless asked for.
		ForceAttemptHTTP2: true,
	}}
}

// getTLS sends a GET with client and returns the response, its body read.
func getTLS(t *testing.T, client *http.Client, url string) (*http.Response, string) {
	t.Helper()
	resp, err := client.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	client.CloseIdleConnections()
	return resp, string(body)
}

func TestRunTLS(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, r.Header.Get("X-Forwarded-Proto"))
	}))
	defer backend.Close()

	certFile, keyFile := tlsFiles(t)
	cert := writeCert(t, certFile, keyFile, "lb")
	path := writeConfig(t, backend.URL)
	writeFile(t, path, tlsConfigFor(backend.URL, certFile, keyFile))
	p := start(t, path)

	resp, body := getTLS(t, tlsClient(cert), "https://"+p.addr+"/")
	if resp.StatusCode != http.StatusOK || body != "https" {
		t.Errorf("got %d with X-Forwarded-Proto %q, want 200 %q", resp.StatusCode, body, "https")
	}
	if resp.Proto != "HTTP/2.0" {
		t.Errorf("protocol = %s, want HTTP/2.0", resp.Proto)
	}

	// Go's server answers plain HTTP on a TLS port with a 400.
	if code, _ := get(t, "http://"+p.addr+"/"); code != http.StatusBadRequest {
		t.Errorf("plain HTTP to the TLS port: got %d, want %d", code, http.StatusBadRequest)
	}

	if err := p.stop(t); err != nil {
		t.Errorf("run returned %v, want nil", err)
	}
}

// echoBackend accepts any upgrade and echoes back what it receives.
func echoBackend(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, brw, err := http.NewResponseController(w).Hijack()
		if err != nil {
			t.Errorf("hijack: %v", err)
			return
		}
		defer conn.Close()
		fmt.Fprintf(brw, "HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: %s\r\n\r\n", r.Header.Get("Upgrade"))
		if brw.Flush() == nil {
			io.Copy(conn, brw)
		}
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

// TestRunTLSUpgrade checks that WebSockets work over TLS. HTTP/2 has no
// Upgrade, and Go's server doesn't offer WebSockets over HTTP/2, so clients
// open a separate HTTP/1.1 connection for them, as this one does.
func TestRunTLSUpgrade(t *testing.T) {
	certFile, keyFile := tlsFiles(t)
	cert := writeCert(t, certFile, keyFile, "lb")
	backendURL := echoBackend(t)
	path := writeConfig(t, backendURL)
	writeFile(t, path, tlsConfigFor(backendURL, certFile, keyFile))
	p := start(t, path)

	pool := x509.NewCertPool()
	pool.AddCert(cert)
	conn, err := tls.Dial("tcp", p.addr, &tls.Config{RootCAs: pool, NextProtos: []string{"http/1.1"}})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(5 * time.Second))

	io.WriteString(conn, "GET / HTTP/1.1\r\nHost: test\r\nConnection: Upgrade\r\nUpgrade: echo\r\n\r\n")
	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusSwitchingProtocols)
	}
	io.WriteString(conn, "ping\n")
	if got, err := br.ReadString('\n'); err != nil || got != "ping\n" {
		t.Errorf("echo: got %q, %v; want %q", got, err, "ping\n")
	}
}

// TestRunTLSReload checks that a reload picks up a renewed certificate
// written over the old files, and that a broken one is rejected.
func TestRunTLSReload(t *testing.T) {
	certFile, keyFile := tlsFiles(t)
	oldCert := writeCert(t, certFile, keyFile, "old")
	backendURL := slowNamedBackend(t, "backend")
	path := writeConfig(t, backendURL)
	writeFile(t, path, tlsConfigFor(backendURL, certFile, keyFile))
	p := start(t, path)

	servedName := func(client *http.Client) string {
		t.Helper()
		resp, _ := getTLS(t, client, "https://"+p.addr+"/")
		return resp.TLS.PeerCertificates[0].Subject.CommonName
	}
	if got := servedName(tlsClient(oldCert)); got != "old" {
		t.Fatalf("certificate before reload = %q, want %q", got, "old")
	}

	newCert := writeCert(t, certFile, keyFile, "new")
	p.sendReload(t)
	client := tlsClient(oldCert, newCert)
	if got := servedName(client); got != "new" {
		t.Errorf("certificate after reload = %q, want %q", got, "new")
	}

	// A key that doesn't match the certificate: the reload is rejected
	// and the last good certificate stays.
	writeCert(t, certFile, filepath.Join(t.TempDir(), "other-key.pem"), "mismatched")
	p.sendReload(t)
	if got := servedName(client); got != "new" {
		t.Errorf("certificate after rejected reload = %q, want %q", got, "new")
	}

	if err := p.stop(t); err != nil {
		t.Errorf("run returned %v, want nil", err)
	}
}

func TestRunTLSErrors(t *testing.T) {
	certFile, keyFile := tlsFiles(t)
	writeCert(t, certFile, keyFile, "lb")
	otherCert, otherKey := tlsFiles(t)
	writeCert(t, otherCert, otherKey, "other")

	tests := []struct {
		name, cert, key string
	}{
		{"missing certificate", filepath.Join(t.TempDir(), "missing.pem"), keyFile},
		{"mismatched key", certFile, otherKey},
		{"not a certificate", keyFile, keyFile},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := writeConfig(t, "http://127.0.0.1:1")
			writeFile(t, path, tlsConfigFor("http://127.0.0.1:1", tt.cert, tt.key))
			err := run(context.Background(), path, io.Discard, nil, nil)
			if err == nil || !strings.Contains(err.Error(), "load certificate") {
				t.Errorf("err = %v, want a certificate error", err)
			}
		})
	}
}

func TestRunReloadCantToggleTLS(t *testing.T) {
	certFile, keyFile := tlsFiles(t)
	writeCert(t, certFile, keyFile, "lb")
	path := writeConfig(t, slowNamedBackend(t, "old"))
	p := start(t, path)

	writeFile(t, path, tlsConfigFor(slowNamedBackend(t, "new"), certFile, keyFile))
	p.sendReload(t)

	if code, body := get(t, "http://"+p.addr+"/"); code != http.StatusOK || body != "old" {
		t.Errorf("after turning TLS on by reload: got %d %q, want plain HTTP to answer %q", code, body, "old")
	}
	if err := p.stop(t); err != nil {
		t.Errorf("run returned %v, want nil", err)
	}
}
