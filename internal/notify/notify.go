// Package notify records incoming events and forwards new events through verified SMTP TLS.
package notify

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/mail"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

type Settings struct {
	Enabled       bool   `json:"enabled"`
	SMSOnly       bool   `json:"sms_only"`
	Host          string `json:"host"`
	Port          int    `json:"port"`
	Security      string `json:"security"`
	Username      string `json:"username"`
	Password      string `json:"password,omitempty"`
	ClearPassword bool   `json:"clear_password,omitempty"`
	From          string `json:"from"`
	To            string `json:"to"`
}

// PublicSettings intentionally contains no password or reversible password value.
type PublicSettings struct {
	Enabled            bool   `json:"enabled"`
	SMSOnly            bool   `json:"sms_only"`
	Host               string `json:"host"`
	Port               int    `json:"port"`
	Security           string `json:"security"`
	Username           string `json:"username"`
	From               string `json:"from"`
	To                 string `json:"to"`
	PasswordConfigured bool   `json:"password_configured"`
	PasswordLocked     bool   `json:"password_locked,omitempty"`
	CredentialError    string `json:"credential_error,omitempty"`
}

type Event struct {
	ID          string    `json:"id"`
	Type        string    `json:"type"`
	Number      string    `json:"number,omitempty"`
	Title       string    `json:"title"`
	Body        string    `json:"body"`
	OccurredAt  time.Time `json:"occurred_at"`
	Status      string    `json:"status"`
	Attempts    int       `json:"attempts"`
	Recipient   string    `json:"recipient,omitempty"`
	NextAttempt time.Time `json:"next_attempt,omitempty"`
	LastError   string    `json:"last_error,omitempty"`
	SentAt      time.Time `json:"sent_at,omitempty"`
}

type diskState struct {
	Version           int            `json:"version"`
	Settings          PublicSettings `json:"settings"`
	ProtectedPassword []byte         `json:"protected_password,omitempty"`
	Events            []Event        `json:"events"`
}

type deliveryResult struct {
	Status, Detail string
	Retry          bool
}
type activeDelivery struct {
	cancel    context.CancelFunc
	cancelled bool
}
type Service struct {
	mu              sync.Mutex
	path            string
	state           diskState
	password        string
	credentialError string
	wake            chan struct{}
	running         bool
	send            func(context.Context, Settings, Event) deliveryResult
	delivery        *activeDelivery
}

func New(dir string) (*Service, error) {
	return newService(dir, false)
}

// NewDesktop preserves an inaccessible secret and pauses delivery without blocking
// the telephone, inbox, or settings service. Unlock requires an explicit action.
func NewDesktop(dir string) (*Service, error) {
	return newService(dir, true)
}

func newService(dir string, recoverCredential bool) (*Service, error) {
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	s := &Service{path: filepath.Join(dir, "notifications.json"), wake: make(chan struct{}, 1), send: sendSMTP}
	s.state = diskState{Version: 1, Settings: PublicSettings{Security: "tls", Port: 465}, Events: []Event{}}
	b, err := os.ReadFile(s.path)
	if err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	if err == nil {
		if err = json.Unmarshal(b, &s.state); err != nil {
			return nil, fmt.Errorf("读取通知存储失败: %w", err)
		}
		if s.state.Version != 1 {
			return nil, errors.New("通知存储版本不受支持")
		}
		if len(s.state.ProtectedPassword) > 0 {
			secret, unsealErr := unprotect(s.state.ProtectedPassword)
			if unsealErr != nil {
				if !recoverCredential {
					return nil, errors.New("无法解密邮箱授权码；请使用原系统账户，并允许访问本机凭据存储")
				}
				s.credentialError = "已保存的邮箱授权码暂不可用。请解锁钥匙串；若仍失败，请在本机重新填写授权码。"
			} else {
				s.password = string(secret)
			}
		}
		s.state.Settings.PasswordConfigured = len(s.state.ProtectedPassword) > 0
		changed := false
		for i := range s.state.Events {
			if s.state.Events[i].Status == "sending" {
				s.state.Events[i].Status = "uncertain"
				s.state.Events[i].LastError = "程序上次在投递过程中退出，结果未知；为避免重复通知，不自动重发"
				changed = true
			}
		}
		if changed {
			if err = s.persist(s.state); err != nil {
				return nil, err
			}
		}
	}
	return s, nil
}

func (s *Service) Settings() PublicSettings {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.publicSettings()
}

func (s *Service) publicSettings() PublicSettings {
	p := s.state.Settings
	p.PasswordLocked, p.CredentialError = s.credentialError != "", s.credentialError
	return p
}

// Unlock never holds the service mutex while the user responds to macOS.
func (s *Service) Unlock() (PublicSettings, error) {
	s.mu.Lock()
	sealed := append([]byte(nil), s.state.ProtectedPassword...)
	s.mu.Unlock()
	if len(sealed) == 0 {
		return PublicSettings{}, errors.New("尚未保存邮箱授权码")
	}
	secret, err := unprotectInteractive(sealed)
	if err != nil {
		return PublicSettings{}, errors.New("邮箱授权码仍不可用。请允许钥匙串访问，或在本机重新填写授权码。")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if !bytes.Equal(sealed, s.state.ProtectedPassword) {
		return PublicSettings{}, errors.New("邮箱设置已更改，请重新解锁")
	}
	s.password, s.credentialError = string(secret), ""
	s.signal()
	return s.publicSettings(), nil
}

func validateSettings(c Settings, password string) error {
	if c.Security != "tls" && c.Security != "starttls" {
		return errors.New("加密方式必须是 tls 或 starttls")
	}
	if c.Port < 1 || c.Port > 65535 {
		return errors.New("SMTP 端口必须在 1 到 65535 之间")
	}
	if strings.ContainsAny(c.Host, "/\\\r\n\t @:") && net.ParseIP(c.Host) == nil {
		return errors.New("SMTP 主机只能填写域名或 IP 地址")
	}
	if strings.ContainsAny(c.Username, "\r\n") {
		return errors.New("SMTP 用户名不能包含换行")
	}
	for _, address := range []string{c.From, c.To} {
		if address == "" && !c.Enabled {
			continue
		}
		a, err := mail.ParseAddress(address)
		if err != nil || a.Address != address || strings.ContainsAny(address, "\r\n") {
			return errors.New("请填写单个有效的邮箱地址")
		}
	}
	if c.Enabled && (c.Host == "" || c.Username == "" || password == "") {
		return errors.New("启用通知前请填写 SMTP 主机、用户名和邮箱授权码")
	}
	return nil
}

// Configure saves a replacement configuration. An empty Password preserves the saved secret.
// Disabling forwarding or changing its destination cancels queued events, preventing replay to a new mailbox.
func (s *Service) Configure(c Settings) (PublicSettings, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c.Host, c.Username = strings.TrimSpace(c.Host), strings.TrimSpace(c.Username)
	c.From, c.To = strings.TrimSpace(c.From), strings.TrimSpace(c.To)
	if c.Security == "" {
		c.Security = "tls"
	}
	if c.Port == 0 {
		if c.Security == "tls" {
			c.Port = 465
		} else {
			c.Port = 587
		}
	}
	password := s.password
	if c.ClearPassword {
		password = ""
	}
	if c.Password != "" {
		password = c.Password
	}
	if s.credentialError != "" && c.Enabled && password == "" {
		return PublicSettings{}, errors.New("请先解锁已保存的授权码，或在本机重新填写后保存")
	}
	if err := validateSettings(c, password); err != nil {
		return PublicSettings{}, err
	}
	next := s.state
	next.Settings = PublicSettings{Enabled: c.Enabled, SMSOnly: c.SMSOnly, Host: c.Host, Port: c.Port, Security: c.Security, Username: c.Username, From: c.From, To: c.To, PasswordConfigured: password != "" || (len(s.state.ProtectedPassword) > 0 && !c.ClearPassword)}
	if password != s.password || c.ClearPassword {
		next.ProtectedPassword = nil
		if password != "" {
			sealed, err := protect([]byte(password))
			if err != nil {
				return PublicSettings{}, fmt.Errorf("无法加密邮箱授权码：%w", err)
			}
			next.ProtectedPassword = sealed
		}
	}
	if !c.Enabled || c.To != s.state.Settings.To {
		next.Events = append([]Event(nil), next.Events...)
		for i := range next.Events {
			if next.Events[i].Status == "queued" || next.Events[i].Status == "retry" {
				next.Events[i].Status = "cancelled"
				next.Events[i].LastError = "邮箱转发已停用或收件邮箱已更改，未发送历史通知"
			}
		}
	}
	if err := s.persist(next); err != nil {
		return PublicSettings{}, err
	}
	cancelActive := !c.Enabled || c.To != s.state.Settings.To
	s.state, s.password = next, password
	if c.ClearPassword || c.Password != "" {
		s.credentialError = ""
	}
	if cancelActive && s.delivery != nil {
		s.delivery.cancelled = true
		s.delivery.cancel()
	}
	s.signal()
	return s.publicSettings(), nil
}

// Enqueue persists an event exactly once by its stable ID. False, nil means that ID already exists.
// Events arriving while forwarding is disabled remain visible but are never replayed automatically.
func (s *Service) Enqueue(e Event) (bool, error) {
	if e.ID == "" || len(e.ID) > 256 {
		return false, errors.New("通知事件 ID 无效")
	}
	if e.Type != "incoming_sms" && e.Type != "incoming_call" && e.Type != "test" {
		return false, errors.New("不支持的通知事件类型")
	}
	if len(e.Title) > 512 || strings.ContainsAny(e.Title, "\r\n") || len(e.Body) > 65536 || len(e.Number) > 128 {
		return false, errors.New("通知内容长度或格式无效")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if e.Type == "test" && !s.state.Settings.Enabled {
		return false, errors.New("请先保存并启用邮箱转发设置")
	}
	if e.Type == "test" && s.credentialError != "" {
		return false, errors.New("请先解锁邮箱授权码")
	}
	for _, previous := range s.state.Events {
		if previous.ID == e.ID {
			return false, nil
		}
	}
	if e.OccurredAt.IsZero() {
		e.OccurredAt = time.Now()
	}
	e.Status, e.Attempts, e.LastError, e.Recipient = "disabled", 0, "", ""
	e.NextAttempt, e.SentAt = time.Time{}, time.Time{}
	if s.state.Settings.Enabled && (!s.state.Settings.SMSOnly || e.Type != "incoming_call") {
		e.Status, e.Recipient = "queued", s.state.Settings.To
	}
	next := s.state
	next.Events = append(append([]Event(nil), s.state.Events...), e)
	if err := s.persist(next); err != nil {
		return false, err
	}
	s.state = next
	s.signal()
	return true, nil
}

func (s *Service) Events(limit int) []Event {
	s.mu.Lock()
	defer s.mu.Unlock()
	if limit <= 0 || limit > 1000 {
		limit = 100
	}
	if limit > len(s.state.Events) {
		limit = len(s.state.Events)
	}
	out := make([]Event, 0, limit)
	for i := len(s.state.Events) - 1; i >= len(s.state.Events)-limit; i-- {
		out = append(out, s.state.Events[i])
	}
	return out
}

func (s *Service) EnqueueTest() (Event, error) {
	if !s.Settings().Enabled {
		return Event{}, errors.New("请先保存并启用邮箱转发设置")
	}
	var id [16]byte
	if _, err := rand.Read(id[:]); err != nil {
		return Event{}, err
	}
	e := Event{ID: "test-" + hex.EncodeToString(id[:]), Type: "test", Title: "VoHive 邮箱通知测试", Body: "这是一封 VoHive EC20 网关测试邮件。收到此邮件表示当前 SMTP 邮箱配置可以投递通知。", OccurredAt: time.Now()}
	if _, err := s.Enqueue(e); err != nil {
		return Event{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, current := range s.state.Events {
		if current.ID == e.ID {
			return current, nil
		}
	}
	return e, nil
}

func (s *Service) signal() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

// Run sends one notification at a time. A durable-state failure stops the worker rather than risking duplicates.
func (s *Service) Run(ctx context.Context) error {
	s.mu.Lock()
	if s.running {
		s.mu.Unlock()
		return errors.New("通知服务已经运行")
	}
	s.running = true
	s.mu.Unlock()
	defer func() { s.mu.Lock(); s.running = false; s.mu.Unlock() }()
	tick := time.NewTicker(5 * time.Second)
	defer tick.Stop()
	for {
		if ctx.Err() != nil {
			return nil
		}
		worked, err := s.process(ctx)
		if err != nil {
			return err
		}
		if worked {
			continue
		}
		select {
		case <-ctx.Done():
			return nil
		case <-tick.C:
		case <-s.wake:
		}
	}
}

func (s *Service) process(ctx context.Context) (bool, error) {
	s.mu.Lock()
	if !s.state.Settings.Enabled || s.credentialError != "" {
		s.mu.Unlock()
		return false, nil
	}
	index := -1
	for i, e := range s.state.Events {
		if (e.Status == "queued" || e.Status == "retry") && !e.NextAttempt.After(time.Now()) {
			index = i
			break
		}
	}
	if index < 0 {
		s.mu.Unlock()
		return false, nil
	}
	next := s.state
	next.Events = append([]Event(nil), next.Events...)
	next.Events[index].Status = "sending"
	next.Events[index].Attempts++
	if err := s.persist(next); err != nil {
		s.mu.Unlock()
		return false, err
	}
	s.state = next
	e := next.Events[index]
	p := next.Settings
	c := Settings{Enabled: p.Enabled, Host: p.Host, Port: p.Port, Security: p.Security, Username: p.Username, Password: s.password, From: p.From, To: e.Recipient}
	deliveryCtx, cancel := context.WithCancel(ctx)
	active := &activeDelivery{cancel: cancel}
	s.delivery = active
	s.mu.Unlock()
	result := s.send(deliveryCtx, c, e)
	cancel()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.delivery = nil
	if active.cancelled {
		result.Retry = false
		if result.Status != "sent" && result.Status != "uncertain" {
			result.Status, result.Detail = "cancelled", "邮箱转发已关闭或收件邮箱已更改，本次发送已取消"
		}
	}
	next = s.state
	next.Events = append([]Event(nil), next.Events...)
	updated := &next.Events[index]
	updated.Status, updated.LastError, updated.NextAttempt = result.Status, result.Detail, time.Time{}
	if result.Status == "sent" {
		updated.SentAt = time.Now()
	}
	if result.Retry {
		if updated.Attempts < 5 && s.state.Settings.Enabled && updated.Recipient == s.state.Settings.To {
			delays := []time.Duration{time.Minute, 5 * time.Minute, 15 * time.Minute, time.Hour}
			updated.Status, updated.NextAttempt = "retry", time.Now().Add(delays[updated.Attempts-1])
		} else {
			updated.Status = "failed"
		}
	}
	if err := s.persist(next); err != nil {
		return true, err
	}
	s.state = next
	return true, nil
}

func (s *Service) persist(state diskState) error {
	b, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	f, err := os.OpenFile(s.path+".tmp", os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0600)
	if err != nil {
		return err
	}
	_, err = f.Write(b)
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	return os.Rename(s.path+".tmp", s.path)
}
