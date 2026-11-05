package desktopcall

import (
	"errors"
	"github.com/iniwex5/vohive/internal/winmodem"
	"sync"
	"testing"
	"time"
)

type fakeModem struct {
	mu         sync.Mutex
	registered bool
	calls      []string
	clcc       string
	ring       func()
	clip       func(string)
	end        func()
}

func (f *fakeModem) Snapshot() winmodem.Status {
	f.mu.Lock()
	defer f.mu.Unlock()
	return winmodem.Status{Connected: true, Registered: f.registered}
}
func (f *fakeModem) record(c string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, c)
	return nil
}
func (f *fakeModem) DialCall(n string) error { return f.record("dial:" + n) }
func (f *fakeModem) AnswerCall() error       { return f.record("answer") }
func (f *fakeModem) HangupCall() error       { return f.record("hangup") }
func (f *fakeModem) ExecuteATSilent(c string, _ time.Duration) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, c)
	return f.clcc, nil
}
func (f *fakeModem) SetRingCallback(cb func())       { f.ring = cb }
func (f *fakeModem) SetClipCallback(cb func(string)) { f.clip = cb }
func (f *fakeModem) SetHangupCallback(cb func())     { f.end = cb }

type fakeAudio struct {
	done          chan error
	muted, closed bool
}

func (a *fakeAudio) Close()             { a.closed = true }
func (a *fakeAudio) Mute(v bool) error  { a.muted = v; return nil }
func (a *fakeAudio) Done() <-chan error { return a.done }
func setup(t *testing.T) (*Manager, *fakeModem, *fakeAudio) {
	t.Helper()
	f := &fakeModem{registered: true}
	m := New(f, "modem", t.TempDir())
	a := &fakeAudio{done: make(chan error, 1)}
	m.startAudio = func(AudioConfig) (audioProcess, error) { return a, nil }
	m.audio.Input = "headset-mic"
	m.audio.Output = "headset-out"
	t.Cleanup(m.Stop)
	return m, f, a
}
func TestDesktopPreflightNeverDialsOfflineOrWithoutAudio(t *testing.T) {
	m, f, _ := setup(t)
	f.registered = false
	if m.Action("dial", "123456", false) == nil {
		t.Fatal("offline dial accepted")
	}
	f.registered = true
	m.startAudio = func(AudioConfig) (audioProcess, error) { return nil, errors.New("mic missing") }
	if m.Action("dial", "123456", false) == nil {
		t.Fatal("missing microphone dial accepted")
	}
	if m.Action("dial", "123;ATH", false) == nil {
		t.Fatal("invalid number accepted")
	}
	if len(f.calls) != 0 {
		t.Fatalf("modem was changed during failed preflight: %v", f.calls)
	}
}
func TestDesktopCallStateRequiresCLCCAndCleansAudio(t *testing.T) {
	m, f, a := setup(t)
	if err := m.Action("dial", "123456", false); err != nil {
		t.Fatal(err)
	}
	if m.Snapshot().State != "dialing" {
		t.Fatal("AT acceptance was treated as connected")
	}
	f.clcc = "+CLCC: 1,0,0,0,0"
	m.op.Lock()
	m.poll()
	m.op.Unlock()
	if m.Snapshot().State != "connected" {
		t.Fatal("CLCC active not reflected")
	}
	if err := m.Action("mute", "", true); err != nil || !a.muted {
		t.Fatal("mute not delivered")
	}
	if err := m.Action("hangup", "", false); err != nil {
		t.Fatal(err)
	}
	if m.Snapshot().State != "idle" || !a.closed {
		t.Fatal("audio/call cleanup incomplete")
	}
}
func TestDesktopIncomingCallbackDoesNotAnswerAutomatically(t *testing.T) {
	m, f, _ := setup(t)
	f.ring()
	f.clip("123456")
	deadline := time.Now().Add(time.Second)
	for m.Snapshot().Number != "123456" && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if m.Snapshot().State != "ringing" {
		t.Fatal("incoming state missing")
	}
	f.mu.Lock()
	calls := len(f.calls)
	f.mu.Unlock()
	if calls != 0 {
		t.Fatal("incoming was auto answered")
	}
	if err := m.Action("answer", "", false); err != nil {
		t.Fatal(err)
	}
}
