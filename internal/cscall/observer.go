package cscall

import (
	"context"
	"time"

	"github.com/iniwex5/vohive/pkg/logger"
)

// IncomingCallEvent describes one cellular incoming call, independently of
// whether a SIP client is registered. ID + Type is a stable deduplication key.
// Type is received or ended; an empty Number means the caller withheld it.
// Answered means that the cellular answer command succeeded, not that audible
// conversation was verified. No audio is recorded by this observer.
type IncomingCallEvent struct {
	ID              string    `json:"id"`
	Type            string    `json:"type"`
	Number          string    `json:"number"`
	ReceivedAt      time.Time `json:"received_at"`
	AnsweredAt      time.Time `json:"answered_at,omitempty"`
	EndedAt         time.Time `json:"ended_at,omitempty"`
	Outcome         string    `json:"outcome"`
	DurationSeconds int64     `json:"duration_seconds"`
}

type incomingCall struct {
	id         string
	receivedAt time.Time
	answeredAt time.Time
	announced  bool
	sipStarted bool
	timer      *time.Timer
}

// SetCallObserver installs an asynchronous, ordered observer. It should persist
// events to a local durable queue, not send email directly. Persistence failures
// receive up to three attempts while the manager is active. Observers must
// deduplicate ID + Type because an error can follow a successful write.
func (m *Manager) SetCallObserver(observer func(IncomingCallEvent) error) {
	m.observerMu.Lock()
	m.callObserver = observer
	m.observerMu.Unlock()
}

// WaitCallObservers waits for queued persistence callbacks. Call after Stop and
// before shutting down the notification store. A stuck callback does not block
// SIP call handling and this method respects the supplied deadline.
func (m *Manager) WaitCallObservers(ctx context.Context) error {
	m.observerMu.Lock()
	done := m.observerDone
	m.observerMu.Unlock()
	if done == nil {
		return nil
	}
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (m *Manager) queueCallEvent(event IncomingCallEvent) {
	m.observerMu.Lock()
	defer m.observerMu.Unlock()
	if m.callObserver == nil {
		return
	}
	m.observerQueue = append(m.observerQueue, event)
	if m.observerDone == nil {
		m.observerDone = make(chan struct{})
		go m.dispatchCallEvents()
	}
}

func (m *Manager) dispatchCallEvents() {
	for {
		m.observerMu.Lock()
		if len(m.observerQueue) == 0 {
			close(m.observerDone)
			m.observerDone = nil
			m.observerMu.Unlock()
			return
		}
		event := m.observerQueue[0]
		m.observerQueue[0] = IncomingCallEvent{}
		m.observerQueue = m.observerQueue[1:]
		observer := m.callObserver
		m.observerMu.Unlock()
		if observer == nil {
			continue
		}
		for attempt := 0; attempt < 3; attempt++ {
			if err := observer(event); err == nil {
				break
			}
			if attempt == 2 || !m.waitObserverRetry(time.Duration(1+attempt*2)*time.Second) {
				logger.Error("Incoming call notification could not be persisted", "call_id", event.ID, "type", event.Type)
				break
			}
		}
	}
}

func (m *Manager) waitObserverRetry(delay time.Duration) bool {
	if m.monitorCtx == nil {
		return false
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-timer.C:
		return true
	case <-m.monitorCtx.Done():
		return false
	}
}

func (m *Manager) startIncomingAfterCallerWait(id string) {
	m.mu.Lock()
	if m.state != CallStateRinging || m.incoming == nil || m.incoming.id != id || m.incoming.sipStarted {
		m.mu.Unlock()
		return
	}
	m.incoming.sipStarted = true
	m.announceIncomingLocked()
	m.mu.Unlock()
	m.initiateSIPCall(id)
}

// The following helpers are called while m.mu is held; callback execution is
// always on a separate worker and never under this lock.
func (m *Manager) incomingEventLocked(kind, outcome string) IncomingCallEvent {
	i := m.incoming
	number := m.callerID
	if number == "Unknown" {
		number = ""
	}
	return IncomingCallEvent{ID: i.id, Type: kind, Number: number,
		ReceivedAt: i.receivedAt, AnsweredAt: i.answeredAt, Outcome: outcome}
}

func (m *Manager) announceIncomingLocked() {
	if m.incoming == nil || m.incoming.announced {
		return
	}
	m.incoming.announced = true
	m.queueCallEvent(m.incomingEventLocked("received", "ringing"))
}

func (m *Manager) finishIncomingLocked(outcome string) {
	i := m.incoming
	if i == nil {
		return
	}
	if i.timer != nil {
		i.timer.Stop()
	}
	m.announceIncomingLocked()
	if !i.answeredAt.IsZero() {
		outcome = "answered"
	}
	event := m.incomingEventLocked("ended", outcome)
	event.EndedAt = time.Now()
	if !i.answeredAt.IsZero() {
		event.DurationSeconds = int64(event.EndedAt.Sub(i.answeredAt) / time.Second)
	}
	m.queueCallEvent(event)
	m.incoming = nil
}
