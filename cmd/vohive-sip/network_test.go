package main

import (
	"errors"
	"github.com/iniwex5/vohive/internal/winmodem"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

type fakeNetworkModem struct {
	release chan struct{}
	done    chan struct{}
}

func (f *fakeNetworkModem) RunNetwork(winmodem.NetworkRequest) (winmodem.NetworkResult, error) {
	<-f.release
	return winmodem.NetworkResult{Log: []string{"ERROR"}}, errors.New("modem rejected")
}
func (f *fakeNetworkModem) Refresh() error {
	select {
	case <-f.done:
	default:
		close(f.done)
	}
	return nil
}
func (f *fakeNetworkModem) Snapshot() winmodem.Status { return winmodem.Status{Connected: true} }
func TestNetworkHTTPAsyncSingleFlightAndValidation(t *testing.T) {
	f := &fakeNetworkModem{release: make(chan struct{}), done: make(chan struct{})}
	s := &networkService{modem: f}
	post := func(body string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		s.ServeHTTP(w, httptest.NewRequest("POST", "/api/network", strings.NewReader(body)))
		return w
	}
	if w := post(`{"action":"manual","plmn":"46011;ATH","rat":7}`); w.Code != 400 {
		t.Fatal(w.Code)
	}
	if w := post(`{"action":"scan"} {}`); w.Code != 400 {
		t.Fatal(w.Code)
	}
	if w := post(`{"action":"scan"}`); w.Code != 202 {
		t.Fatal(w.Code)
	}
	if w := post(`{"action":"auto"}`); w.Code != 409 {
		t.Fatal(w.Code)
	}
	w := httptest.NewRecorder()
	s.ServeHTTP(w, httptest.NewRequest("GET", "/api/network", nil))
	if !strings.Contains(w.Body.String(), `"running"`) {
		t.Fatal(w.Body.String())
	}
	close(f.release)
	select {
	case <-f.done:
	case <-time.After(time.Second):
		t.Fatal("worker stuck")
	}
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		s.mu.Lock()
		state := s.job.State
		err := s.job.Error
		s.mu.Unlock()
		if state == "error" {
			if !strings.Contains(err, "modem rejected") {
				t.Fatal(err)
			}
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("final failure was not exposed")
}
