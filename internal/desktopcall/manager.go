package desktopcall

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/iniwex5/vohive/internal/machelper"
	"github.com/iniwex5/vohive/internal/winmodem"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

type Modem interface {
	Snapshot() winmodem.Status
	DialCall(string) error
	AnswerCall() error
	HangupCall() error
	ExecuteATSilent(string, time.Duration) (string, error)
	SetRingCallback(func())
	SetClipCallback(func(string))
	SetHangupCallback(func())
}
type AudioConfig struct {
	Input  string `json:"input"`
	Output string `json:"output"`
	Modem  string `json:"modem"`
}
type Status struct {
	State       string    `json:"state"`
	Number      string    `json:"number"`
	Error       string    `json:"error"`
	Incoming    bool      `json:"incoming"`
	Muted       bool      `json:"muted"`
	AudioActive bool      `json:"audio_active"`
	StartedAt   time.Time `json:"started_at"`
	ConnectedAt time.Time `json:"connected_at"`
}
type event struct{ kind, number string }
type audioProcess interface {
	Close()
	Mute(bool) error
	Done() <-chan error
}
type Manager struct {
	modem      Modem
	mu         sync.Mutex
	op         sync.Mutex
	status     Status
	audio      AudioConfig
	path       string
	events     chan event
	ctx        context.Context
	cancel     context.CancelFunc
	wg         sync.WaitGroup
	process    audioProcess
	startAudio func(AudioConfig) (audioProcess, error)
	absent     int
}

func New(modem Modem, device, dataDir string) *Manager {
	ctx, cancel := context.WithCancel(context.Background())
	m := &Manager{modem: modem, status: Status{State: "idle"}, audio: AudioConfig{Modem: device}, path: filepath.Join(dataDir, "desktop-audio.json"), events: make(chan event, 32), ctx: ctx, cancel: cancel, startAudio: openAudio}
	if b, err := os.ReadFile(m.path); err == nil {
		_ = json.Unmarshal(b, &m.audio)
		m.audio.Modem = device
	}
	modem.SetRingCallback(func() { m.emit(event{kind: "ring"}) })
	modem.SetClipCallback(func(n string) { m.emit(event{kind: "clip", number: n}) })
	modem.SetHangupCallback(func() { m.emit(event{kind: "end"}) })
	m.wg.Add(1)
	go m.run()
	return m
}
func (m *Manager) emit(e event) {
	select {
	case m.events <- e:
	default:
	}
}
func (m *Manager) Snapshot() Status { m.mu.Lock(); defer m.mu.Unlock(); return m.status }
func (m *Manager) Stop() {
	m.cancel()
	m.wg.Wait()
	m.op.Lock()
	defer m.op.Unlock()
	if m.Snapshot().State != "idle" {
		m.finish(true, "")
	}
}
func (m *Manager) run() {
	defer m.wg.Done()
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	for {
		select {
		case <-m.ctx.Done():
			return
		case e := <-m.events:
			m.op.Lock()
			switch e.kind {
			case "ring", "clip":
				m.mu.Lock()
				if m.status.State == "idle" {
					m.status = Status{State: "ringing", Incoming: true, StartedAt: time.Now()}
				}
				if m.status.Incoming && e.number != "" {
					m.status.Number = e.number
				}
				m.mu.Unlock()
			case "end":
				if m.Snapshot().State != "idle" {
					m.finish(false, "")
				}
			}
			m.op.Unlock()
		case <-tick.C:
			if !m.op.TryLock() {
				continue
			}
			m.poll()
			m.op.Unlock()
		}
	}
}

var clcc = regexp.MustCompile(`(?m)\+CLCC:\s*\d+,\d+,(\d+),0,`)

func (m *Manager) poll() {
	s := m.Snapshot()
	if s.State == "idle" {
		return
	}
	if m.process != nil {
		select {
		case <-m.process.Done():
			m.finish(true, "音频设备中断，已结束通话")
			return
		default:
		}
	}
	raw, err := m.modem.ExecuteATSilent("AT+CLCC", 2*time.Second)
	if err != nil {
		m.finish(true, "无法确认通话状态："+err.Error())
		return
	}
	matches := clcc.FindAllStringSubmatch(raw, -1)
	if len(matches) == 0 {
		m.absent++
		if m.absent >= 3 {
			m.finish(false, "")
		}
		return
	}
	m.absent = 0
	for _, match := range matches {
		if match[1] == "0" {
			m.mu.Lock()
			m.status.State = "connected"
			if m.status.ConnectedAt.IsZero() {
				m.status.ConnectedAt = time.Now()
			}
			m.mu.Unlock()
			return
		}
	}
	if !s.Incoming && time.Since(s.StartedAt) > 60*time.Second {
		m.finish(true, "60 秒未接通，已结束拨号")
	}
}
func (m *Manager) finish(signal bool, detail string) {
	if signal {
		if err := m.modem.HangupCall(); err != nil && detail == "" {
			detail = "挂断结果未确认：" + err.Error()
		}
	}
	if m.process != nil {
		m.process.Close()
		m.process = nil
	}
	_, _ = m.modem.ExecuteATSilent("AT+QPCMV=0", 2*time.Second)
	m.mu.Lock()
	m.status.State = "idle"
	m.status.Error = detail
	m.status.AudioActive = false
	m.status.Muted = false
	m.mu.Unlock()
	m.absent = 0
}

var numberRE = regexp.MustCompile(`^\+?[0-9]{3,20}$`)

func (m *Manager) Action(action, number string, muted bool) error {
	if !m.op.TryLock() {
		return errors.New("通话操作正在执行，请稍后")
	}
	defer m.op.Unlock()
	s := m.Snapshot()
	switch action {
	case "dial", "answer":
		if action == "dial" {
			if !numberRE.MatchString(number) {
				return errors.New("请输入有效电话号码")
			}
			if s.State != "idle" {
				return errors.New("已有通话，请先挂断")
			}
			if !m.modem.Snapshot().Registered {
				return errors.New("EC20 尚未注册运营商网络，暂不能拨号")
			}
		}
		if action == "answer" && (s.State != "ringing" || !s.Incoming) {
			return errors.New("没有待接听来电")
		}
		m.mu.Lock()
		cfg := m.audio
		m.mu.Unlock()
		if cfg.Input == "" || cfg.Output == "" {
			return errors.New("请先在设备设置中选择麦克风和扬声器")
		}
		audio, err := m.startAudio(cfg)
		if err != nil {
			return fmt.Errorf("音频未就绪：%w", err)
		}
		m.process = audio
		m.mu.Lock()
		if action == "dial" {
			m.status = Status{State: "dialing", Number: number, StartedAt: time.Now(), AudioActive: true}
		} else {
			m.status.State = "answering"
			m.status.AudioActive = true
		}
		m.mu.Unlock()
		if action == "dial" {
			err = m.modem.DialCall(number)
		} else {
			err = m.modem.AnswerCall()
		}
		if err != nil {
			m.finish(true, err.Error())
			return err
		}
		return nil
	case "hangup":
		if s.State == "idle" {
			return nil
		}
		m.finish(true, "")
		if detail := m.Snapshot().Error; detail != "" {
			return errors.New(detail)
		}
		return nil
	case "mute":
		if m.process == nil {
			return errors.New("没有正在使用的通话音频")
		}
		if err := m.process.Mute(muted); err != nil {
			return err
		}
		m.mu.Lock()
		m.status.Muted = muted
		m.mu.Unlock()
		return nil
	default:
		return errors.New("未知通话操作")
	}
}
func (m *Manager) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if r.URL.Path == "/api/desktop/call" {
		if r.Method == "GET" {
			json.NewEncoder(w).Encode(m.Snapshot())
			return
		}
		if r.Method != "POST" {
			http.Error(w, "Method", 405)
			return
		}
		var body struct {
			Action, Number string
			Muted          bool
		}
		dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1024))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&body); err != nil {
			http.Error(w, "Invalid JSON", 400)
			return
		}
		if err := m.Action(body.Action, body.Number, body.Muted); err != nil {
			http.Error(w, err.Error(), 409)
			return
		}
		json.NewEncoder(w).Encode(m.Snapshot())
		return
	}
	if r.URL.Path != "/api/desktop/audio" {
		http.NotFound(w, r)
		return
	}
	if r.Method == "POST" {
		if !m.op.TryLock() {
			http.Error(w, "通话操作正在执行", 409)
			return
		}
		defer m.op.Unlock()
		if m.Snapshot().State != "idle" {
			http.Error(w, "通话中不能切换音频设备", 409)
			return
		}
		var cfg AudioConfig
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&cfg); err != nil {
			http.Error(w, "Invalid JSON", 400)
			return
		}
		m.mu.Lock()
		cfg.Modem = m.audio.Modem
		m.mu.Unlock()
		if len(cfg.Input) > 256 || len(cfg.Output) > 256 {
			http.Error(w, "设备名过长", 400)
			return
		}
		b, _ := json.Marshal(cfg)
		if err := os.WriteFile(m.path+".new", b, 0600); err != nil {
			http.Error(w, "无法保存音频配置", 500)
			return
		}
		if err := os.Rename(m.path+".new", m.path); err != nil {
			http.Error(w, "无法保存音频配置", 500)
			return
		}
		m.mu.Lock()
		m.audio = cfg
		m.mu.Unlock()
		json.NewEncoder(w).Encode(cfg)
		return
	}
	if r.Method != "GET" {
		http.Error(w, "Method", 405)
		return
	}
	cmd, err := machelper.Command("devices", "")
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	// A disconnected Core Audio service must not block the HTTP server indefinitely.
	out, err := outputWithTimeout(cmd, 5*time.Second)
	if err != nil {
		http.Error(w, "读取音频设备失败", 500)
		return
	}
	var devices any
	if json.Unmarshal(out, &devices) != nil {
		http.Error(w, "无效音频设备列表", 500)
		return
	}
	m.mu.Lock()
	cfg := m.audio
	m.mu.Unlock()
	json.NewEncoder(w).Encode(map[string]any{"selected": cfg, "devices": devices})
}
func outputWithTimeout(cmd *exec.Cmd, timeout time.Duration) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	bounded := exec.CommandContext(ctx, cmd.Path, cmd.Args[1:]...)
	bounded.Env = cmd.Env
	bounded.Dir = cmd.Dir
	return bounded.Output()
}

type pipeAudio struct {
	cmd     *exec.Cmd
	in      io.WriteCloser
	done    chan error
	once    sync.Once
	writeMu sync.Mutex
}

func openAudio(cfg AudioConfig) (audioProcess, error) {
	raw, _ := json.Marshal(cfg)
	cmd, err := machelper.Command("desktop", string(raw))
	if err != nil {
		return nil, err
	}
	in, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		in.Close()
		return nil, err
	}
	if err = cmd.Start(); err != nil {
		in.Close()
		stderr.Close()
		return nil, err
	}
	p := &pipeAudio{cmd: cmd, in: in, done: make(chan error, 1)}
	ready := make(chan error, 1)
	go func() {
		reader := bufio.NewReader(stderr)
		line, e := reader.ReadString('\n')
		if e != nil || strings.TrimSpace(line) != "READY" {
			ready <- fmt.Errorf("%s", strings.TrimSpace(line))
		} else {
			ready <- nil
		}
		io.Copy(os.Stderr, reader)
		p.done <- cmd.Wait()
	}()
	select {
	case err = <-ready:
		if err != nil {
			p.Close()
			return nil, err
		}
		return p, nil
	case <-time.After(8 * time.Second):
		p.Close()
		return nil, errors.New("打开音频设备超时")
	}
}
func (p *pipeAudio) Mute(v bool) error {
	p.writeMu.Lock()
	defer p.writeMu.Unlock()
	_, err := fmt.Fprintf(p.in, "{\"mute\":%t}\n", v)
	return err
}
func (p *pipeAudio) Done() <-chan error { return p.done }
func (p *pipeAudio) Close() {
	p.once.Do(func() { p.writeMu.Lock(); p.in.Close(); p.writeMu.Unlock(); p.cmd.Process.Kill() })
}
