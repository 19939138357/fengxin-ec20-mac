package main

import (
	"encoding/json"
	"net/http"

	"github.com/iniwex5/vohive/internal/notify"
)

// Called only after the shared authentication and same-origin checks.
func handleNotifications(w http.ResponseWriter, r *http.Request, service *notify.Service) bool {
	switch r.URL.Path {
	case "/api/notifications/unlock":
		if r.Method != http.MethodPost {
			http.Error(w, "Method", http.StatusMethodNotAllowed)
			return true
		}
		public, err := service.Unlock()
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return true
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(public)
	case "/api/notifications/settings":
		w.Header().Set("Content-Type", "application/json")
		switch r.Method {
		case http.MethodGet:
			json.NewEncoder(w).Encode(service.Settings())
		case http.MethodPost:
			var settings notify.Settings
			decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16384))
			decoder.DisallowUnknownFields()
			if err := decoder.Decode(&settings); err != nil {
				http.Error(w, "设置格式无效", http.StatusBadRequest)
				return true
			}
			public, err := service.Configure(settings)
			if err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return true
			}
			json.NewEncoder(w).Encode(public)
		default:
			http.Error(w, "Method", http.StatusMethodNotAllowed)
		}
	case "/api/notifications/events":
		if r.Method != http.MethodGet {
			http.Error(w, "Method", http.StatusMethodNotAllowed)
			return true
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(service.Events(100))
	case "/api/notifications/test":
		if r.Method != http.MethodPost {
			http.Error(w, "Method", http.StatusMethodNotAllowed)
			return true
		}
		event, err := service.EnqueueTest()
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return true
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		json.NewEncoder(w).Encode(event)
	default:
		return false
	}
	return true
}
