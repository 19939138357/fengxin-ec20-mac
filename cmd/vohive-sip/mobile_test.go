package main

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"image/png"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/iniwex5/vohive/internal/notify"
	"github.com/iniwex5/vohive/internal/winmodem"
)

const testMobileToken = "only-a-test-token-with-more-than-32-characters"

type fakeMobileModem struct {
	registered bool
	sends      atomic.Int32
	send       func(string, string) error
}

func (m *fakeMobileModem) Snapshot() winmodem.Status {
	return winmodem.Status{Registered: m.registered, Port: "test-only", SIM: "READY"}
}
func (m *fakeMobileModem) SMSInbox() []winmodem.IncomingSMS { return nil }
func (m *fakeMobileModem) SMSReceiverStatus() winmodem.SMSReceiveStatus {
	return winmodem.SMSReceiveStatus{Ready: true}
}
func (m *fakeMobileModem) SendSMS(number, text string) error {
	m.sends.Add(1)
	if m.send != nil {
		return m.send(number, text)
	}
	return nil
}

type fakeMobileRegistrar struct{}

func (fakeMobileRegistrar) CountRegistered() int { return 1 }

type fakeMobileEvents []notify.Event

func (f fakeMobileEvents) Events(int) []notify.Event { return []notify.Event(f) }

func mobileTestRequest(handler http.Handler, method, path, auth, body, contentType string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	if auth != "" {
		r.Header.Set("Authorization", auth)
	}
	if contentType != "" {
		r.Header.Set("Content-Type", contentType)
	}
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	return w
}

func requireMobileError(t *testing.T, w *httptest.ResponseRecorder, status int) {
	t.Helper()
	if w.Code != status {
		t.Fatalf("HTTP status %d, expected %d", w.Code, status)
	}
	var body map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil || len(body) != 1 || body["error"] == nil {
		t.Fatal("response is not a JSON error")
	}
	if w.Header().Get("Cache-Control") != "no-store" || w.Header().Get("Access-Control-Allow-Origin") != "" {
		t.Fatal("cache/CORS policy changed")
	}
}

func TestMobileAllRoutesRequireBearer(t *testing.T) {
	m := &fakeMobileModem{registered: true}
	h := newMobileHandler(testMobileToken, m, fakeMobileRegistrar{}, fakeMobileEvents{})
	for _, path := range []string{"/v1/status", "/v1/inbox", "/v1/calls", "/v1/sms", "/api/mobile-pairing", "/api/shutdown", "/unknown"} {
		for _, auth := range []string{"", "Basic dXNlcjpwYXNz", "Bearer wrong", "Bearer " + testMobileToken + " "} {
			requireMobileError(t, mobileTestRequest(h, http.MethodGet, path, auth, "", ""), http.StatusUnauthorized)
		}
	}
	if m.sends.Load() != 0 {
		t.Fatal("unauthenticated request reached modem")
	}
	requireMobileError(t, mobileTestRequest(h, http.MethodOptions, "/v1/status", "Bearer "+testMobileToken, "", ""), http.StatusMethodNotAllowed)
	requireMobileError(t, mobileTestRequest(h, http.MethodGet, "/api/mobile-pairing", "Bearer "+testMobileToken, "", ""), http.StatusNotFound)
}

func TestMobileStatusInboxAndCallsContract(t *testing.T) {
	when := time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC)
	events := fakeMobileEvents{{ID: "sms-hidden", Type: "incoming_sms", Body: "not a call"}, {ID: "call-1-ended", Type: "incoming_call", Number: "12345", Title: "来电结果", Body: "已接听", OccurredAt: when, Status: "disabled", Recipient: "must-not-leak@example.com"}}
	h := newMobileHandler(testMobileToken, &fakeMobileModem{}, fakeMobileRegistrar{}, events)
	auth := "Bearer " + testMobileToken
	w := mobileTestRequest(h, http.MethodGet, "/v1/status", auth, "", "")
	var status struct {
		Version    string          `json:"version"`
		Modem      winmodem.Status `json:"modem"`
		Registered int             `json:"sip_registered"`
	}
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &status) != nil || status.Version != version || status.Registered != 1 {
		t.Fatal("status contract changed")
	}
	w = mobileTestRequest(h, http.MethodGet, "/v1/inbox", auth, "", "")
	var inbox map[string]json.RawMessage
	if json.Unmarshal(w.Body.Bytes(), &inbox) != nil || string(inbox["messages"]) != "[]" || len(inbox["receiver"]) == 0 {
		t.Fatal("empty inbox contract changed")
	}
	w = mobileTestRequest(h, http.MethodGet, "/v1/calls", auth, "", "")
	var calls []map[string]any
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &calls) != nil || len(calls) != 1 || len(calls[0]) != 6 || calls[0]["id"] != "call-1-ended" || calls[0]["occurred_at"] != when.Format(time.RFC3339) {
		t.Fatal("calls contract changed or unrelated notification fields exposed")
	}
	h = newMobileHandler(testMobileToken, &fakeMobileModem{}, fakeMobileRegistrar{}, fakeMobileEvents{})
	if body := mobileTestRequest(h, http.MethodGet, "/v1/calls", auth, "", "").Body.String(); strings.TrimSpace(body) != "[]" {
		t.Fatal("empty calls must be []")
	}
}

func TestMobileSMSRejectsInvalidBodyBeforeModem(t *testing.T) {
	m := &fakeMobileModem{registered: true}
	h := newMobileHandler(testMobileToken, m, fakeMobileRegistrar{}, fakeMobileEvents{})
	cases := []struct {
		body, contentType string
		status            int
	}{
		{`{"number":"12345","text":"你好"}`, "text/plain", 415},
		{`{`, "application/json", 400},
		{`{"number":"12345","text":"你好","unexpected":true}`, "application/json", 400},
		{`{"number":"12345","text":"你好"} {}`, "application/json", 400},
		{`{"number":"bad","text":"你好"}`, "application/json", 422},
		{`{"number":"12345","text":""}`, "application/json", 422},
		{`{"number":"12345","text":"` + strings.Repeat("x", 2049) + `"}`, "application/json", 413},
		{strings.Repeat(" ", mobileMaxBodyBytes+1), "application/json", 413},
	}
	for _, tc := range cases {
		requireMobileError(t, mobileTestRequest(h, http.MethodPost, "/v1/sms", "Bearer "+testMobileToken, tc.body, tc.contentType), tc.status)
	}
	if m.sends.Load() != 0 {
		t.Fatal("invalid SMS reached modem")
	}
	r := httptest.NewRequest(http.MethodPost, "/v1/sms", strings.NewReader(strings.Repeat(" ", mobileMaxBodyBytes+1)))
	r.ContentLength = -1
	r.Header.Set("Authorization", "Bearer "+testMobileToken)
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	requireMobileError(t, w, 413)
}

func TestMobileSMSSubmissionExactlyOnceAndOfflineGate(t *testing.T) {
	body := `{"number":"12345","text":"测试内容"}`
	for _, tc := range []struct {
		name       string
		registered bool
		failure    error
		status     int
		calls      int32
	}{
		{"offline", false, nil, 409, 0}, {"accepted", true, nil, 200, 1},
		{"modem-rejected", true, errors.New("modem rejected command"), 409, 1},
		{"too-many-parts", true, errors.New("single SMS only; shorten message"), 422, 1},
		{"ambiguous-timeout", true, errors.New("AT timeout; result uncertain; no automatic retry"), 504, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := &fakeMobileModem{registered: tc.registered, send: func(number, text string) error {
				if number != "12345" || text != "测试内容" {
					t.Fatal("SMS content changed")
				}
				return tc.failure
			}}
			h := newMobileHandler(testMobileToken, m, fakeMobileRegistrar{}, fakeMobileEvents{})
			w := mobileTestRequest(h, http.MethodPost, "/v1/sms", "Bearer "+testMobileToken, body, "application/json; charset=utf-8")
			if tc.status != 200 {
				requireMobileError(t, w, tc.status)
			} else {
				var response map[string]string
				if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &response) != nil || response["state"] != "submitted" {
					t.Fatal("submission response invalid")
				}
			}
			if m.sends.Load() != tc.calls {
				t.Fatal("SMS attempted more than once")
			}
		})
	}
}

func TestMobileConcurrentSMSIsNotQueuedOrRetried(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	m := &fakeMobileModem{registered: true, send: func(string, string) error { close(started); <-release; return nil }}
	h := newMobileHandler(testMobileToken, m, fakeMobileRegistrar{}, fakeMobileEvents{})
	body := `{"number":"12345","text":"one"}`
	done := make(chan struct{})
	go func() {
		mobileTestRequest(h, http.MethodPost, "/v1/sms", "Bearer "+testMobileToken, body, "application/json")
		close(done)
	}()
	<-started
	requireMobileError(t, mobileTestRequest(h, http.MethodPost, "/v1/sms", "Bearer "+testMobileToken, body, "application/json"), 409)
	close(release)
	<-done
	if m.sends.Load() != 1 {
		t.Fatal("duplicate queued behind first submission")
	}
}

func mobileTestConfig(t *testing.T) (config, string, []byte) {
	t.Helper()
	dir := t.TempDir()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "VoHive test"}, DNSNames: []string{"localhost"}, IPAddresses: []net.IP{net.ParseIP("127.0.0.1"), net.ParseIP("192.168.0.19")}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, BasicConstraintsValid: true}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(dir, "cert.pem"), pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0600); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(dir, "key.pem"), pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}), 0600); err != nil {
		t.Fatal(err)
	}
	return config{MobileListen: "127.0.0.1:7577", MobileAdvertiseHost: "192.168.0.19", MobileToken: testMobileToken, MobileCertFile: "cert.pem", MobileKeyFile: "key.pem", SIPListen: "0.0.0.0:5068", SIPAdvertiseIP: "192.168.0.19", SIPUsername: "1001", SIPPassword: "test-only-sip-secret"}, dir, der
}

func TestMobileConfigurationValidationAndPairingFingerprint(t *testing.T) {
	if service, err := newMobileService(config{}, t.TempDir()); service != nil || err != nil {
		t.Fatal("disabled mobile changed old configuration")
	}
	cfg, dir, der := mobileTestConfig(t)
	s, err := newMobileService(cfg, dir)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(der)
	if s.pairing.APIURL != "https://192.168.0.19:7577" || s.pairing.TLSSHA256 != hex.EncodeToString(digest[:]) || s.pairing.SIP.Port != 5068 || s.pairing.SIP.Transport != "udp" || s.tlsConfig.MinVersion != tls.VersionTLS12 {
		t.Fatal("pairing metadata does not match loaded TLS/SIP configuration")
	}
	for _, change := range []func(*config){
		func(c *config) { c.MobileToken = "short" }, func(c *config) { c.MobileToken = testMobileToken + "\n" },
		func(c *config) { c.MobileListen = "127.0.0.1:0" }, func(c *config) { c.MobileAdvertiseHost = "192.168.0.20" },
		func(c *config) { c.MobileAdvertiseHost = "https://192.168.0.19" }, func(c *config) { c.MobileCertFile = "missing.pem" },
		func(c *config) { c.SIPAdvertiseIP = "127.0.0.1" }, func(c *config) { c.SIPListen = "127.0.0.1:5068" },
	} {
		bad := cfg
		change(&bad)
		if _, err := newMobileService(bad, dir); err == nil {
			t.Fatal("invalid mobile configuration accepted")
		}
	}
}

func TestMobilePairingDisabledAndPNGContract(t *testing.T) {
	w := httptest.NewRecorder()
	if !handleMobilePairing(w, httptest.NewRequest(http.MethodGet, "/api/mobile-pairing", nil), nil) {
		t.Fatal("pairing route missed")
	}
	requireMobileError(t, w, 503)
	cfg, dir, _ := mobileTestConfig(t)
	s, err := newMobileService(cfg, dir)
	if err != nil {
		t.Fatal(err)
	}
	s.ready.Store(true)
	w = httptest.NewRecorder()
	handleMobilePairing(w, httptest.NewRequest(http.MethodGet, "/api/mobile-pairing", nil), s)
	var parsed mobilePairing
	if json.Unmarshal(w.Body.Bytes(), &parsed) != nil || parsed.Version != 1 || parsed.Token != testMobileToken {
		t.Fatal("pairing payload invalid")
	}
	w = httptest.NewRecorder()
	handleMobilePairing(w, httptest.NewRequest(http.MethodGet, "/api/mobile-pairing/qr", nil), s)
	image, err := png.Decode(bytes.NewReader(w.Body.Bytes()))
	if err != nil || image.Bounds().Dx() != 384 || w.Header().Get("Cache-Control") != "no-store" || w.Header().Get("Content-Type") != "image/png" {
		t.Fatal("QR endpoint not a no-store PNG")
	}
	w = httptest.NewRecorder()
	handleMobilePairing(w, httptest.NewRequest(http.MethodPost, "/api/mobile-pairing", nil), s)
	requireMobileError(t, w, 405)
}

func TestMobileRejectsExpiredFutureAndClientOnlyCertificates(t *testing.T) {
	for _, kind := range []string{"expired", "future", "client-only"} {
		t.Run(kind, func(t *testing.T) {
			cfg, dir, der := mobileTestConfig(t)
			pair, err := tls.LoadX509KeyPair(filepath.Join(dir, "cert.pem"), filepath.Join(dir, "key.pem"))
			if err != nil {
				t.Fatal(err)
			}
			certificate, err := x509.ParseCertificate(der)
			if err != nil {
				t.Fatal(err)
			}
			switch kind {
			case "expired":
				certificate.NotAfter = time.Now().Add(-time.Minute)
			case "future":
				certificate.NotBefore = time.Now().Add(time.Minute)
			case "client-only":
				certificate.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}
			}
			key := pair.PrivateKey.(*ecdsa.PrivateKey)
			changed, err := x509.CreateCertificate(rand.Reader, certificate, certificate, &key.PublicKey, key)
			if err != nil {
				t.Fatal(err)
			}
			if err = os.WriteFile(filepath.Join(dir, "cert.pem"), pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: changed}), 0600); err != nil {
				t.Fatal(err)
			}
			if _, err = newMobileService(cfg, dir); err == nil {
				t.Fatal("ineligible TLS certificate accepted")
			}
		})
	}
}

func TestMobileTLSListenerAuthenticatesAndStops(t *testing.T) {
	cfg, dir, der := mobileTestConfig(t)
	reservation, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	cfg.MobileListen = reservation.Addr().String()
	s, err := newMobileService(cfg, dir)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err = s.Start(ctx, &fakeMobileModem{}, fakeMobileRegistrar{}, fakeMobileEvents{}, nil); err == nil {
		s.Close()
		reservation.Close()
		t.Fatal("listener conflict not detected synchronously")
	}
	reservation.Close()
	if err = s.Start(ctx, &fakeMobileModem{}, fakeMobileRegistrar{}, fakeMobileEvents{}, nil); err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	pool := x509.NewCertPool()
	certificate, _ := x509.ParseCertificate(der)
	pool.AddCert(certificate)
	client := &http.Client{Timeout: 3 * time.Second, Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}}}
	defer client.CloseIdleConnections()
	r, err := http.NewRequest(http.MethodGet, "https://"+cfg.MobileListen+"/v1/status", nil)
	if err != nil {
		t.Fatal(err)
	}
	r.Header.Set("Authorization", "Bearer "+testMobileToken)
	response, err := client.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, response.Body)
	response.Body.Close()
	if response.StatusCode != 200 || response.TLS == nil || response.TLS.Version < tls.VersionTLS12 || !s.Ready() {
		t.Fatal("TLS service not ready")
	}
	if s.server.ReadHeaderTimeout != 5*time.Second || s.server.ReadTimeout != 10*time.Second || s.server.WriteTimeout < 70*time.Second || s.server.IdleTimeout == 0 || s.server.MaxHeaderBytes > 16<<10 {
		t.Fatal("HTTP bounds cannot support modem send or bound idle clients")
	}
	cancel()
	deadline := time.Now().Add(2 * time.Second)
	for s.Ready() && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if s.Ready() {
		t.Fatal("mobile service remained ready after shutdown")
	}
}
