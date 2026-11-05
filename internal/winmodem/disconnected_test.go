package winmodem

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestDisconnectedDesktopCannotTransmitAndKeepsArchive(t *testing.T) {
	m := NewDisconnected("usb:test", errors.New("Expected exactly one EC20; found 0"))
	defer m.Close()
	s := m.Snapshot()
	if s.Connected || s.Registered || s.LastUpdated.IsZero() || !strings.Contains(s.Error, "found 0") {
		t.Fatalf("invalid offline snapshot: %+v", s)
	}
	if _, err := m.ExecuteATSilent("ATD123456;", time.Millisecond); err == nil {
		t.Fatal("offline AT unexpectedly accepted")
	}
	if err := m.SendSMS("123456", "test"); err == nil {
		t.Fatal("offline SMS accepted")
	}
	if err := m.DialCall("123456"); err == nil {
		t.Fatal("offline call accepted")
	}
	if _, err := m.RunNetwork(NetworkRequest{Action: "radio-diagnose"}); err == nil {
		t.Fatal("offline network operation accepted")
	}
	dir := t.TempDir()
	raw := `{"version":1,"messages":{"existing":{"id":"existing","number":"123456","text":"archived","received_at":"2026-10-02T00:00:00Z","stored_at":"2026-10-02T00:00:00Z","historical":false,"parts":1}}}`
	if err := os.WriteFile(filepath.Join(dir, "received.json"), []byte(raw), 0600); err != nil {
		t.Fatal(err)
	}
	var forwarded atomic.Int32
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := m.StartSMSReceiver(ctx, dir, func(IncomingSMS) error { forwarded.Add(1); return nil }); err != nil {
		t.Fatal(err)
	}
	inbox := m.SMSInbox()
	if len(inbox) != 1 || inbox[0].ID != "existing" {
		t.Fatalf("archive unavailable: %+v", inbox)
	}
	deadline := time.Now().Add(time.Second)
	for m.SMSReceiverStatus().Running && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if m.SMSReceiverStatus().Ready || forwarded.Load() != 0 {
		t.Fatal("offline receiver advertised readiness or replayed old messages")
	}
}
