package cscall

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/iniwex5/vohive/internal/sipgw"
)

func notificationManager(t *testing.T) (*Manager, chan IncomingCallEvent) {
	t.Helper()
	reg, err := sipgw.NewRegistrar(sipgw.DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	// The registrar has no registered clients and is not bound to a socket.
	m := NewManagerWithController("test", "synthetic", &testController{}, reg)
	events := make(chan IncomingCallEvent, 16)
	m.SetCallObserver(func(e IncomingCallEvent) error { events <- e; return nil })
	t.Cleanup(func() {
		m.Stop()
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if err := m.WaitCallObservers(ctx); err != nil {
			t.Error(err)
		}
		reg.Stop()
	})
	return m, events
}

func nextCallEvent(t *testing.T, events <-chan IncomingCallEvent) IncomingCallEvent {
	t.Helper()
	select {
	case e := <-events:
		return e
	case <-time.After(2 * time.Second):
		t.Fatal("no incoming call notification")
		return IncomingCallEvent{}
	}
}

func noMoreCallEvents(t *testing.T, m *Manager, events <-chan IncomingCallEvent) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := m.WaitCallObservers(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case e := <-events:
		t.Fatalf("unexpected duplicate event: %+v", e)
	default:
	}
}

func TestIncomingNotificationsDeduplicateRINGAndCLIPWithoutSIPClient(t *testing.T) {
	m, events := notificationManager(t)
	for i := 0; i < 5; i++ {
		m.handleControllerEvent(Event{Type: EventIncoming, CallID: "at", Number: "Unknown"})
	}
	select {
	case e := <-events:
		t.Fatalf("did not wait for caller ID: %+v", e)
	default:
	}
	clip := Event{Type: EventIncoming, CallID: "at", Number: "+123456", CallerIDOnly: true}
	m.handleControllerEvent(clip)
	received := nextCallEvent(t, events)
	if received.Type != "received" || received.Number != "+123456" || received.Outcome != "ringing" || received.ID == "" || received.ReceivedAt.IsZero() {
		t.Fatalf("unexpected received event: %+v", received)
	}
	for i := 0; i < 5; i++ {
		m.handleControllerEvent(clip)
		m.onIncoming("at", "Unknown")
	}
	m.handleControllerEvent(Event{Type: EventHangup, CallID: "at"})
	ended := nextCallEvent(t, events)
	if ended.ID != received.ID || ended.Type != "ended" || ended.Outcome != "missed" || ended.Number != received.Number || !ended.AnsweredAt.IsZero() || ended.EndedAt.Before(received.ReceivedAt) {
		t.Fatalf("unexpected ended event: %+v", ended)
	}
	m.handleControllerEvent(Event{Type: EventHangup, CallID: "at"})
	// A delayed CLIP after hangup must not create a phantom second phone call.
	m.handleControllerEvent(clip)
	noMoreCallEvents(t, m, events)
}

func TestUnknownCallerNotifiesAfterOneSecondAndLateCLIPUpdatesOutcome(t *testing.T) {
	m, events := notificationManager(t)
	started := time.Now()
	m.onIncoming("at", "Unknown")
	m.onIncoming("at", "Unknown")
	received := nextCallEvent(t, events)
	if time.Since(started) < 900*time.Millisecond || received.Number != "" || received.Type != "received" {
		t.Fatalf("unknown caller wait or number invalid: %+v", received)
	}
	// The fallback already initiated SIP; late caller ID must not initiate again.
	_, startAgain := m.beginIncomingCallEvent("at", "+123457", true)
	if startAgain {
		t.Fatal("late CLIP started a second SIP call")
	}
	m.onHangup()
	ended := nextCallEvent(t, events)
	if ended.ID != received.ID || ended.Number != "+123457" || ended.Outcome != "missed" {
		t.Fatalf("late caller ID lost: %+v", ended)
	}
	noMoreCallEvents(t, m, events)
}

func TestQuickHangupStillProducesOrderedNotificationsAndSeparateNextCall(t *testing.T) {
	m, events := notificationManager(t)
	m.onIncoming("at", "Unknown")
	m.onHangup()
	first := nextCallEvent(t, events)
	ended := nextCallEvent(t, events)
	if first.Type != "received" || ended.Type != "ended" || first.ID != ended.ID || first.Number != "" {
		t.Fatal("quick call lost notification order")
	}
	m.onIncoming("at", "123456")
	second := nextCallEvent(t, events)
	if second.ID == first.ID {
		t.Fatal("two calls reused an ID")
	}
	// Late cleanup from the previous SIP attempt must not terminate this call.
	m.endCallForID(first.ID, false, false, "failed")
	if !m.HasCall(second.ID) {
		t.Fatal("old SIP attempt terminated a new call")
	}
	m.onHangup()
	_ = nextCallEvent(t, events)
	noMoreCallEvents(t, m, events)
}

func TestIncomingNotificationRejectedAndConnectedOutcomes(t *testing.T) {
	m, events := notificationManager(t)
	m.beginIncomingCall("at", "123456")
	received := nextCallEvent(t, events)
	if !m.HandleClientCancel(received.ID) {
		t.Fatal("cancel failed")
	}
	if e := nextCallEvent(t, events); e.Outcome != "rejected" || !e.AnsweredAt.IsZero() {
		t.Fatalf("cancel result: %+v", e)
	}
	m.beginIncomingCall("at", "123456")
	received = nextCallEvent(t, events)
	m.handleControllerEvent(Event{Type: EventConnected, CallID: "at"})
	m.mu.Lock()
	answered := !m.incoming.answeredAt.IsZero()
	m.incoming.answeredAt = time.Now().Add(-2500 * time.Millisecond)
	m.mu.Unlock()
	if !answered {
		t.Fatal("connected event did not record answer time")
	}
	m.onHangup()
	e := nextCallEvent(t, events)
	if e.ID != received.ID || e.Outcome != "answered" || e.DurationSeconds != 2 {
		t.Fatalf("connected result: %+v", e)
	}
	noMoreCallEvents(t, m, events)
}

func TestSlowCallObserverDoesNotBlockCallHandling(t *testing.T) {
	m, _ := notificationManager(t)
	entered := make(chan struct{})
	release := make(chan struct{})
	events := make(chan IncomingCallEvent, 4)
	m.SetCallObserver(func(e IncomingCallEvent) error {
		if e.Type == "received" {
			close(entered)
			<-release
		}
		events <- e
		return nil
	})
	m.beginIncomingCall("at", "123456")
	<-entered
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := m.WaitCallObservers(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("observer wait did not respect deadline")
	}
	done := make(chan struct{})
	go func() { m.onHangup(); close(done) }()
	select {
	case <-done:
	case <-time.After(200 * time.Millisecond):
		close(release)
		t.Fatal("persistence callback blocked call hangup")
	}
	close(release)
	if e := nextCallEvent(t, events); e.Type != "received" {
		t.Fatal("received event order")
	}
	if e := nextCallEvent(t, events); e.Type != "ended" {
		t.Fatal("ended event order")
	}
	noMoreCallEvents(t, m, events)
}

func TestCallObserverRetriesPersistenceWithSameEventID(t *testing.T) {
	m, _ := notificationManager(t)
	events := make(chan IncomingCallEvent, 8)
	var attempts atomic.Int32
	m.SetCallObserver(func(e IncomingCallEvent) error {
		events <- e
		if attempts.Add(1) == 1 {
			return errors.New("synthetic storage unavailable")
		}
		return nil
	})
	m.beginIncomingCall("at", "123456")
	first := nextCallEvent(t, events)
	retry := nextCallEvent(t, events)
	if first.ID != retry.ID || first.Type != retry.Type || first.ReceivedAt != retry.ReceivedAt {
		t.Fatal("persistence retry changed event identity")
	}
	m.onHangup()
	if e := nextCallEvent(t, events); e.Type != "ended" {
		t.Fatal("retry lost call outcome")
	}
	noMoreCallEvents(t, m, events)
}
