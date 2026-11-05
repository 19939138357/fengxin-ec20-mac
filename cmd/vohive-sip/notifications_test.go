package main

import (
	"bytes"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/iniwex5/vohive/internal/notify"
)

func TestNotificationAPISettingsSecretAndDisabledTest(t *testing.T) {
	dir := t.TempDir()
	if err := preparePrivateDataDir(dir); err != nil {
		t.Fatal(err)
	}
	service, err := notify.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	call := func(method, path, body string) *httptest.ResponseRecorder {
		t.Helper()
		w := httptest.NewRecorder()
		if !handleNotifications(w, httptest.NewRequest(method, path, strings.NewReader(body)), service) {
			t.Fatal("route was not handled")
		}
		return w
	}
	if w := call("POST", "/api/notifications/test", "{}"); w.Code != 400 {
		t.Fatalf("disabled forwarding test = %d", w.Code)
	}
	settings := `{"enabled":false,"host":"smtp.example.invalid","port":465,"security":"tls","username":"user@example.invalid","password":"fixture-secret","from":"user@example.invalid","to":"recipient@example.invalid"}`
	w := call("POST", "/api/notifications/settings", settings)
	if w.Code != 200 || bytes.Contains(w.Body.Bytes(), []byte("fixture-secret")) {
		t.Fatalf("bad response %d", w.Code)
	}
	w = call("GET", "/api/notifications/settings", "")
	var public map[string]any
	if err = json.Unmarshal(w.Body.Bytes(), &public); err != nil {
		t.Fatal(err)
	}
	if _, ok := public["password"]; ok {
		t.Fatal("password field leaked")
	}
	if public["password_configured"] != true {
		t.Fatal("missing secret configured indicator")
	}
	if w = call("PUT", "/api/notifications/settings", "{}"); w.Code != 405 {
		t.Fatal("unexpected method accepted")
	}
	if w = call("POST", "/api/notifications/settings", `{"unknown_key":true}`); w.Code != 400 {
		t.Fatal("invalid settings accepted")
	}
	if len(service.Events(100)) != 0 {
		t.Fatal("disabled test created an event")
	}
}
