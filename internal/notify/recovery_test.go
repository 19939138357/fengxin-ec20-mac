package notify

import (
	"bytes"
	"context"
	"path/filepath"
	"testing"
)

func TestDesktopCredentialFailurePreservesQueueAndCiphertext(t *testing.T) {
	s := configuredService(t)
	s.state.ProtectedPassword[len(s.state.ProtectedPassword)-1] ^= 1
	if err := s.persist(s.state); err != nil {
		t.Fatal(err)
	}
	reopened, err := NewDesktop(filepath.Dir(s.path))
	if err != nil {
		t.Fatal(err)
	}
	if !reopened.Settings().PasswordLocked || !reopened.Settings().PasswordConfigured {
		t.Fatal("inaccessible credential was hidden")
	}
	saved := append([]byte(nil), reopened.state.ProtectedPassword...)
	if _, err = reopened.Enqueue(Event{ID: "sms-locked", Type: "incoming_sms", Title: "synthetic"}); err != nil {
		t.Fatal(err)
	}
	reopened.send = func(context.Context, Settings, Event) deliveryResult {
		t.Fatal("locked credential reached SMTP")
		return deliveryResult{}
	}
	if worked, err := reopened.process(context.Background()); worked || err != nil {
		t.Fatalf("locked worker: %v %v", worked, err)
	}
	if _, err = reopened.EnqueueTest(); err == nil {
		t.Fatal("test accepted while locked")
	}
	if _, err = reopened.Unlock(); err == nil {
		t.Fatal("corrupted secret unlocked")
	}
	if _, err = reopened.Configure(Settings{Enabled: false, Host: "smtp.example.com", Security: "tls", Port: 465}); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(saved, reopened.state.ProtectedPassword) || !reopened.Settings().PasswordLocked {
		t.Fatal("pausing erased the saved credential")
	}
	if reopened.Events(1)[0].Status != "cancelled" {
		t.Fatal("pausing failed to cancel the unsent event")
	}
	if _, err = reopened.Configure(Settings{Enabled: false, Security: "tls", Port: 465, ClearPassword: true}); err != nil {
		t.Fatal(err)
	}
	if reopened.Settings().PasswordConfigured || reopened.Settings().PasswordLocked || len(reopened.state.ProtectedPassword) > 0 {
		t.Fatal("explicit clearing retained the locked credential")
	}
}

func TestDesktopUnlockRestoresDeliveryWithoutReplacingSecret(t *testing.T) {
	s := configuredService(t)
	stored := append([]byte(nil), s.state.ProtectedPassword...)
	s.password = ""
	s.credentialError = "synthetic access denial"
	if _, err := s.Enqueue(Event{ID: "sms-unlock", Type: "incoming_sms", Title: "synthetic"}); err != nil {
		t.Fatal(err)
	}
	public, err := s.Unlock()
	if err != nil || public.PasswordLocked || !public.PasswordConfigured {
		t.Fatalf("unlock: %v %v", public.PasswordLocked, err)
	}
	if !bytes.Equal(stored, s.state.ProtectedPassword) {
		t.Fatal("unlock replaced ciphertext")
	}
	sent := false
	s.send = func(_ context.Context, c Settings, _ Event) deliveryResult {
		sent = true
		if c.Password != "test-secret-never-log" {
			t.Fatal("secret not restored")
		}
		return deliveryResult{Status: "sent"}
	}
	if worked, err := s.process(context.Background()); !worked || err != nil || !sent {
		t.Fatalf("resumed queue: %v %v", worked, err)
	}
}
