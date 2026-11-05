package notify

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func configuredService(t *testing.T) *Service {
	t.Helper()
	s, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.Configure(Settings{Enabled: true, Host: "smtp.example.com", Port: 465, Security: "tls", Username: "me@example.com", Password: "test-secret-never-log", From: "me@example.com", To: "owner@example.com"})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestSettingsDPAPIRoundtripAndPublicRedaction(t *testing.T) {
	s := configuredService(t)
	data, err := os.ReadFile(s.path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "test-secret-never-log") {
		t.Fatal("plaintext secret persisted")
	}
	public, _ := json.Marshal(s.Settings())
	if strings.Contains(string(public), "test-secret") || strings.Contains(string(public), "protected_password") {
		t.Fatal("public secret leak")
	}
	reopened, err := New(filepath.Dir(s.path))
	if err != nil {
		t.Fatal(err)
	}
	if reopened.password != "test-secret-never-log" || !reopened.Settings().PasswordConfigured {
		t.Fatal("secret not restored")
	}
	_, err = reopened.Configure(Settings{Enabled: true, Host: "smtp.example.com", Port: 465, Security: "tls", Username: "me@example.com", From: "me@example.com", To: "owner@example.com"})
	if err != nil {
		t.Fatal(err)
	}
	if reopened.password != "test-secret-never-log" {
		t.Fatal("blank password erased secret")
	}
	_, err = reopened.Configure(Settings{Enabled: false, Security: "tls", Port: 465, ClearPassword: true})
	if err != nil {
		t.Fatal(err)
	}
	if reopened.Settings().PasswordConfigured || len(reopened.state.ProtectedPassword) != 0 {
		t.Fatal("secret was not cleared")
	}
}

func TestSettingsValidationAndTestEmailGate(t *testing.T) {
	s, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.EnqueueTest(); err == nil {
		t.Fatal("disabled test accepted")
	}
	bad := []Settings{
		{Enabled: true, Security: "plain", Port: 25},
		{Enabled: true, Security: "tls", Port: 465, Host: "smtp.example.com", Username: "me", Password: "x", From: "me@example.com", To: "a@example.com,b@example.com"},
		{Enabled: true, Security: "tls", Port: 465, Host: "smtp.example.com\r\nInjected: x", Username: "me", Password: "x", From: "me@example.com", To: "a@example.com"},
		{Enabled: true, Security: "tls", Port: 465, Host: "smtp.example.com", Username: "me", From: "me@example.com", To: "a@example.com"},
	}
	for _, c := range bad {
		if _, err = s.Configure(c); err == nil {
			t.Fatal("invalid setting accepted")
		}
	}
}

func TestEnqueueDisabledNoReplayAndDurableDedup(t *testing.T) {
	dir := t.TempDir()
	s, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	e := Event{ID: "sms-001", Type: "incoming_sms", Title: "收到短信", Body: "你好", OccurredAt: time.Now()}
	if added, err := s.Enqueue(e); err != nil || !added {
		t.Fatalf("enqueue: %v %v", added, err)
	}
	_, err = s.Configure(Settings{Enabled: true, Host: "smtp.example.com", Port: 465, Security: "tls", Username: "me", Password: "secret", From: "me@example.com", To: "owner@example.com"})
	if err != nil {
		t.Fatal(err)
	}
	if s.Events(1)[0].Status != "disabled" {
		t.Fatal("historical event replayed")
	}
	s, err = New(dir)
	if err != nil {
		t.Fatal(err)
	}
	if added, err := s.Enqueue(e); err != nil || added {
		t.Fatal("duplicate event not deduplicated after restart")
	}
	if len(s.Events(20)) != 1 {
		t.Fatal("duplicate history")
	}
}

func TestSendQueueRetryAcceptanceAndCrashRecovery(t *testing.T) {
	s := configuredService(t)
	_, err := s.Enqueue(Event{ID: "call-1", Type: "incoming_call", Title: "来电", Body: "未接听"})
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	s.send = func(_ context.Context, _ Settings, _ Event) deliveryResult {
		count++
		return deliveryResult{Status: "failed", Detail: "temporary", Retry: true}
	}
	if _, err = s.process(context.Background()); err != nil {
		t.Fatal(err)
	}
	if s.Events(1)[0].Status != "retry" || s.Events(1)[0].Attempts != 1 {
		t.Fatal("transient error not scheduled")
	}
	if worked, _ := s.process(context.Background()); worked || count != 1 {
		t.Fatal("retry backoff ignored")
	}
	for i := 1; i < 5; i++ {
		s.state.Events[0].NextAttempt = time.Time{}
		if _, err = s.process(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if s.Events(1)[0].Status != "failed" || count != 5 {
		t.Fatal("retry bound ignored")
	}
	_, err = s.Enqueue(Event{ID: "call-2", Type: "incoming_call", Title: "来电"})
	if err != nil {
		t.Fatal(err)
	}
	s.send = func(_ context.Context, _ Settings, e Event) deliveryResult {
		stored, err := New(filepath.Dir(s.path))
		if err != nil {
			t.Fatal(err)
		}
		if stored.Events(1)[0].Status != "uncertain" {
			t.Fatal("in-flight crash would replay")
		}
		return deliveryResult{Status: "sent"}
	}
	if _, err = s.process(context.Background()); err != nil {
		t.Fatal(err)
	}
	if s.Events(1)[0].Status != "sent" || s.Events(1)[0].SentAt.IsZero() {
		t.Fatal("acceptance not persisted")
	}
}

func TestStorageFailureNeverSendsAndDestinationChangeCancelsQueue(t *testing.T) {
	s := configuredService(t)
	_, err := s.Enqueue(Event{ID: "sms", Type: "incoming_sms", Title: "短信"})
	if err != nil {
		t.Fatal(err)
	}
	c := s.state.Settings
	_, err = s.Configure(Settings{Enabled: true, Host: c.Host, Port: c.Port, Security: c.Security, Username: c.Username, From: c.From, To: "new@example.com"})
	if err != nil {
		t.Fatal(err)
	}
	if s.Events(1)[0].Status != "cancelled" {
		t.Fatal("old destination queue not cancelled")
	}
	_, err = s.Enqueue(Event{ID: "sms2", Type: "incoming_sms", Title: "短信"})
	if err != nil {
		t.Fatal(err)
	}
	s.path = filepath.Join(filepath.Dir(s.path), "missing", "file.json")
	s.send = func(context.Context, Settings, Event) deliveryResult {
		t.Fatal("sent without durable sending state")
		return deliveryResult{}
	}
	if _, err = s.process(context.Background()); err == nil {
		t.Fatal("storage error hidden")
	}
	if _, err = s.Enqueue(Event{ID: "sms3", Type: "incoming_sms", Title: "短信"}); err == nil {
		t.Fatal("enqueue storage failure hidden")
	}
}

func TestUncertainIsNotRetried(t *testing.T) {
	s := configuredService(t)
	_, err := s.Enqueue(Event{ID: "one", Type: "incoming_sms", Title: "SMS"})
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	s.send = func(context.Context, Settings, Event) deliveryResult {
		calls++
		return deliveryResult{Status: "uncertain", Detail: "unknown"}
	}
	if _, err = s.process(context.Background()); err != nil {
		t.Fatal(err)
	}
	if worked, _ := s.process(context.Background()); worked || calls != 1 {
		t.Fatal("uncertain email repeated")
	}
	if _, err = New(filepath.Dir(s.path)); err != nil {
		t.Fatal(err)
	}
}

func TestCorruptStorageRejected(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "notifications.json"), []byte("{"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := New(dir); err == nil || errors.Is(err, os.ErrNotExist) {
		t.Fatal("corrupt durable history ignored")
	}
}

func TestConfigureCancelsActiveDeliveryWithoutRetry(t *testing.T) {
	for _, kind := range []string{"disable", "change-recipient"} {
		t.Run(kind, func(t *testing.T) {
			s := configuredService(t)
			if _, err := s.Enqueue(Event{ID: "active", Type: "incoming_call", Title: "来电"}); err != nil {
				t.Fatal(err)
			}
			started := make(chan struct{})
			s.send = func(ctx context.Context, _ Settings, _ Event) deliveryResult {
				close(started)
				<-ctx.Done()
				return deliveryResult{Status: "failed", Retry: true}
			}
			done := make(chan error, 1)
			go func() { _, err := s.process(context.Background()); done <- err }()
			<-started
			p := s.Settings()
			c := Settings{Enabled: true, Host: p.Host, Port: p.Port, Security: p.Security, Username: p.Username, From: p.From, To: p.To}
			if kind == "disable" {
				c.Enabled = false
			} else {
				c.To = "new@example.com"
			}
			if _, err := s.Configure(c); err != nil {
				t.Fatal(err)
			}
			select {
			case err := <-done:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(time.Second):
				t.Fatal("active SMTP delivery not cancelled")
			}
			if e := s.Events(1)[0]; e.Status != "cancelled" || !e.NextAttempt.IsZero() {
				t.Fatalf("cancelled delivery retried: %+v", e)
			}
		})
	}
}
