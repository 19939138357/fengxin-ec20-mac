package main

import (
	"crypto/tls"
	"crypto/x509"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestCertificateMatchesPairedHostAndPrivateKey(t *testing.T) {
	now := time.Now()
	cert, key, err := certificate("192.168.0.19", now)
	if err != nil {
		t.Fatal(err)
	}
	pair, err := tls.X509KeyPair(cert, key)
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := x509.ParseCertificate(pair.Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	if err = leaf.VerifyHostname("192.168.0.19"); err != nil {
		t.Fatal(err)
	}
	if leaf.VerifyHostname("192.168.0.20") == nil {
		t.Fatal("certificate accepts unpaired host")
	}
	dir := t.TempDir()
	certPath := filepath.Join(dir, "server.pem")
	keyPath := filepath.Join(dir, "key.pem")
	if err = writePrivate(certPath, cert); err != nil {
		t.Fatal(err)
	}
	if err = writePrivate(keyPath, key); err != nil {
		t.Fatal(err)
	}
	if !reusable(certPath, keyPath, "192.168.0.19", now) {
		t.Fatal("would rotate valid paired certificate")
	}
	if reusable(certPath, keyPath, "192.168.0.20", now) || reusable(certPath, keyPath, "192.168.0.19", now.AddDate(2, 0, 0)) {
		t.Fatal("reuses wrong or expired certificate")
	}
	if err = writePrivate(certPath, []byte("replacement")); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(certPath)
	if string(got) != "replacement" {
		t.Fatal("atomic replacement failed")
	}
}
