//go:build darwin && cgo

package notify

import (
	"os"
	"path/filepath"
	"testing"
)

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "vohive-keychain-test-")
	if err != nil {
		panic(err)
	}
	if err = startIsolatedKeychain(filepath.Join(dir, "mail.keychain")); err != nil {
		panic(err)
	}
	code := m.Run()
	stopIsolatedKeychain()
	os.RemoveAll(dir)
	os.Exit(code)
}
func TestMacCipherRejectsTampering(t *testing.T) {
	b, err := protect([]byte("synthetic-only-secret"))
	if err != nil {
		t.Fatal(err)
	}
	b[len(b)-1] ^= 1
	if _, err = unprotect(b); err == nil {
		t.Fatal("tampered credential accepted")
	}
}
func TestSMSOnlyDoesNotForwardCalls(t *testing.T) {
	s := configuredService(t)
	_, err := s.Configure(Settings{Enabled: true, SMSOnly: true, Host: "smtp.example.com", Port: 465, Security: "tls", Username: "me", From: "me@example.com", To: "owner@example.com"})
	if err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{"incoming_call", "incoming_sms"} {
		if _, err = s.Enqueue(Event{ID: kind, Type: kind, Body: "synthetic"}); err != nil {
			t.Fatal(err)
		}
	}
	events := s.Events(2)
	if events[0].Status != "queued" || events[1].Status != "disabled" {
		t.Fatal("SMS-only forwarding scope was not respected")
	}
}
