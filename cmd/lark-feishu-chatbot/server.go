package main

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"time"

	"lark-feishu-chatbot/internal/config"
)

// serverTLS loads explicit HTTPS credentials before opening the listener.
// [Main Request] 03 Bootstrap: HTTPS certificate checks happen before listening; disabled HTTPS skips loading the
// files.
func serverTLS(cfg config.Config) (*tls.Config, error) {
	if !cfg.UseHTTPS {
		return nil, nil
	}
	cert, err := tls.LoadX509KeyPair(cfg.CertFile, cfg.KeyFile)
	if err != nil || len(cert.Certificate) == 0 {
		return nil, errors.New("cannot load HTTPS certificate/key pair")
	}
	leaf, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		return nil, errors.New("cannot parse HTTPS certificate")
	}
	now := time.Now()
	if now.Before(leaf.NotBefore) || !now.Before(leaf.NotAfter) {
		return nil, errors.New("HTTPS certificate is outside its validity period")
	}
	cert.Leaf = leaf
	return &tls.Config{MinVersion: tls.VersionTLS12, Certificates: []tls.Certificate{cert}}, nil
}
