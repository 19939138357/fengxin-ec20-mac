package main

import (
	"encoding/json"
	"github.com/iniwex5/vohive/internal/winmodem"
	"io"
	"net/http"
	"sync"
	"time"
)

type networkModem interface {
	RunNetwork(winmodem.NetworkRequest) (winmodem.NetworkResult, error)
	Refresh() error
	Snapshot() winmodem.Status
}
type networkJob struct {
	ID       uint64                 `json:"id"`
	State    string                 `json:"state"`
	Action   string                 `json:"action"`
	Started  time.Time              `json:"started,omitempty"`
	Finished time.Time              `json:"finished,omitempty"`
	Error    string                 `json:"error,omitempty"`
	Result   winmodem.NetworkResult `json:"result"`
}
type networkService struct {
	mu    sync.Mutex
	job   networkJob
	modem networkModem
}

func (s *networkService) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if r.Method == http.MethodGet {
		s.mu.Lock()
		job := s.job
		s.mu.Unlock()
		if job.State == "" {
			job.State = "idle"
		}
		json.NewEncoder(w).Encode(job)
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "Method", 405)
		return
	}
	var req winmodem.NetworkRequest
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1024))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		http.Error(w, "无效请求", 400)
		return
	}
	if dec.Decode(&struct{}{}) != io.EOF {
		http.Error(w, "无效请求", 400)
		return
	}
	if _, err := req.Command(); err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	s.mu.Lock()
	if s.job.State == "running" {
		s.mu.Unlock()
		http.Error(w, "操作正在执行，请等待结果", 409)
		return
	}
	if !s.modem.Snapshot().Connected {
		s.mu.Unlock()
		http.Error(w, "串口已断开，请重新启动网关", 409)
		return
	}
	s.job = networkJob{ID: s.job.ID + 1, State: "running", Action: req.Action, Started: time.Now().UTC()}
	job := s.job
	s.mu.Unlock()
	go func() {
		result, err := s.modem.RunNetwork(req)
		// Fresh registration is separate from command acceptance. An OK response
		// alone must never turn the modem status into "registered".
		refreshErr := s.modem.Refresh()
		s.mu.Lock()
		defer s.mu.Unlock()
		s.job.Result = result
		s.job.State = "done"
		s.job.Finished = time.Now().UTC()
		if err != nil {
			s.job.State = "error"
			s.job.Error = err.Error()
		}
		if refreshErr != nil {
			s.job.State = "error"
			s.job.Error += "；状态刷新失败：" + refreshErr.Error()
		}
	}()
	w.WriteHeader(http.StatusAccepted)
	json.NewEncoder(w).Encode(job)
}
