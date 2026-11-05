package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"lark-feishu-chatbot/internal/config"
)

// tlsFiles creates short-lived credentials solely for local HTTPS tests.
func tlsFiles(t *testing.T, expires time.Time) (string, string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	cert := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "localhost"},
		DNSNames:     []string{"localhost"},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
		NotBefore:    expires.Add(-time.Hour),
		NotAfter:     expires,
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, cert, cert, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	encodedKey, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	certPath, keyPath := filepath.Join(dir, "cert.pem"), filepath.Join(dir, "key.pem")
	if err := os.WriteFile(certPath, pem.EncodeToMemory(&pem.Block{
		Type:  "CERTIFICATE",
		Bytes: der,
	}), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{
		Type:  "EC PRIVATE KEY",
		Bytes: encodedKey,
	}), 0600); err != nil {
		t.Fatal(err)
	}
	return certPath, keyPath
}

// TestOptionalHTTPS exercises a real local TLS handshake and rejects invalid credentials.
func TestOptionalHTTPS(t *testing.T) {
	if tlsConfig, err := serverTLS(config.Config{}); err != nil || tlsConfig != nil {
		t.Fatalf("plain HTTP configuration: %v", err)
	}
	cert, key := tlsFiles(t, time.Now().Add(30*time.Minute))
	tlsConfig, err := serverTLS(config.Config{UseHTTPS: true, CertFile: cert, KeyFile: key})
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	server.TLS = tlsConfig
	server.StartTLS()
	defer server.Close()
	resp, err := server.Client().Get(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 204 {
		t.Fatal(resp.StatusCode)
	}
	for _, cfg := range []config.Config{{UseHTTPS: true}, {UseHTTPS: true, CertFile: cert, KeyFile: cert}} {
		if _, err := serverTLS(cfg); err == nil {
			t.Fatal("invalid TLS files accepted")
		}
	}
	cert, key = tlsFiles(t, time.Now().Add(-time.Minute))
	if _, err := serverTLS(config.Config{UseHTTPS: true, CertFile: cert, KeyFile: key}); err == nil {
		t.Fatal("expired certificate accepted")
	}
}
