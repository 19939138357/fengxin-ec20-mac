package winmodem

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"github.com/warthog618/sms"
	"github.com/warthog618/sms/encoding/tpdu"
	"io"
	"regexp"
	"strings"
	"sync"
	"time"
)

var numberPattern = regexp.MustCompile(`^\+?[0-9]{3,20}$`)
var registrationPattern = regexp.MustCompile(`(?m)\+(?:CE|C)REG:\s*\d+,([15])(?:,|$)`)

type Status struct {
	Port         string    `json:"port"`
	Model        string    `json:"model"`
	Connected    bool      `json:"connected"`
	LastUpdated  time.Time `json:"last_updated"`
	SIM          string    `json:"sim"`
	Registration string    `json:"registration"`
	Signal       string    `json:"signal"`
	Radio        string    `json:"radio"`
	Operator     string    `json:"operator"`
	Registered   bool      `json:"registered"`
	Error        string    `json:"error,omitempty"`
}
type Modem struct {
	port               io.ReadWriteCloser
	name               string
	commandMu          sync.Mutex
	mu                 sync.RWMutex
	lines              chan string
	done               chan struct{}
	once               sync.Once
	pcm                chan int
	ring               func()
	clip               func(string)
	hangup             func()
	status             Status
	smsReceiver        *smsReceiver
	callResultExpected bool
	trafficMu          sync.RWMutex
}

// A desktop session remains useful without USB: archived messages, audio
// selection and mailbox settings are available, while all AT writes fail.
func NewDisconnected(name string, cause error) *Modem {
	m := &Modem{port: disconnectedTransport{}, name: name, lines: make(chan string, 1), done: make(chan struct{}), pcm: make(chan int, 1), status: Status{Port: name}}
	if cause != nil {
		m.status.Error = cause.Error()
	}
	m.Close()
	return m
}

type disconnectedTransport struct{}

func (disconnectedTransport) Read([]byte) (int, error)  { return 0, io.EOF }
func (disconnectedTransport) Write([]byte) (int, error) { return 0, fmt.Errorf("EC20 is disconnected") }
func (disconnectedTransport) Close() error              { return nil }

func Open(name string) (*Modem, error) {
	p, err := openTransport(name)
	if err != nil {
		return nil, err
	}
	m := &Modem{port: p, name: name, lines: make(chan string, 128), done: make(chan struct{}), pcm: make(chan int, 8), status: Status{Port: name, Connected: true, LastUpdated: time.Now().UTC()}}
	go m.readLoop()
	return m, nil
}
func (m *Modem) Close() {
	m.once.Do(func() {
		m.mu.Lock()
		m.invalidateLiveStatusLocked()
		previous := m.status.Error
		m.status.Error = "Serial port closed; restart gateway"
		if previous != "" && previous != m.status.Error {
			m.status.Error += "; " + previous
		}
		close(m.done)
		m.mu.Unlock()
		m.port.Close()
	})
}

// A disconnected instance cannot supply current network readings. Preserve
// only static identity and expose the time this status changed to the UI.
func (m *Modem) invalidateLiveStatusLocked() {
	m.status.Connected = false
	m.status.Registered = false
	m.status.SIM = ""
	m.status.Registration = ""
	m.status.Signal = ""
	m.status.Radio = ""
	m.status.Operator = ""
	m.status.LastUpdated = time.Now().UTC()
}
func (m *Modem) SetRingCallback(f func())       { m.mu.Lock(); m.ring = f; m.mu.Unlock() }
func (m *Modem) SetClipCallback(f func(string)) { m.mu.Lock(); m.clip = f; m.mu.Unlock() }
func (m *Modem) SetHangupCallback(f func())     { m.mu.Lock(); m.hangup = f; m.mu.Unlock() }
func (m *Modem) GetQPCMVChan() <-chan int       { return m.pcm }
func (m *Modem) Snapshot() Status               { m.mu.RLock(); defer m.mu.RUnlock(); return m.status }
func (m *Modem) readLoop() {
	var line strings.Builder
	buf := make([]byte, 512)
	for {
		n, err := m.port.Read(buf)
		if err != nil {
			m.Close()
			return
		}
		select {
		case <-m.done:
			return
		default:
		}
		for _, c := range buf[:n] {
			if c == '\r' || c == '\n' {
				s := strings.TrimSpace(line.String())
				line.Reset()
				if s != "" {
					m.onLine(s)
				}
			} else if c == '>' && strings.TrimSpace(line.String()) == "" {
				line.Reset()
				m.onLine(">")
			} else if line.Len() < 16384 {
				line.WriteByte(c)
			}
		}
	}
}
func (m *Modem) onLine(s string) {
	if m.onSMSLine(s) {
		return
	}
	m.mu.RLock()
	ring, clip, hang, callResultExpected := m.ring, m.clip, m.hangup, m.callResultExpected
	m.mu.RUnlock()
	if s == "RING" {
		if ring != nil {
			ring()
		}
		return
	}
	if strings.HasPrefix(s, "+CLIP:") {
		parts := strings.Split(s, "\"")
		if len(parts) > 1 && clip != nil {
			clip(parts[1])
		}
		return
	}
	if s == "NO CARRIER" || s == "BUSY" || s == "NO ANSWER" {
		if hang != nil {
			hang()
		}
		// A call may end during any unrelated command. Its URC must not
		// terminate CMGL/CPIN/ATH early and leave their final OK unread.
		if !callResultExpected {
			return
		}
	}
	if strings.HasPrefix(s, "+QPCMV:") {
		v := 0
		if strings.TrimSpace(strings.TrimPrefix(s, "+QPCMV:")) == "1" {
			v = 1
		}
		select {
		case m.pcm <- v:
		default:
		}
		return
	}
	select {
	case m.lines <- s:
	case <-m.done:
	}
}
func (m *Modem) commandLocked(cmd string, timeout time.Duration, prompt bool) (string, error) {
	select {
	case <-m.done:
		return "", fmt.Errorf("serial port closed; restart gateway")
	default:
	}
	for {
		select {
		case <-m.lines:
			continue
		default:
		}
		break
	}
	commandName := strings.ToUpper(strings.TrimSpace(cmd))
	callCommand := strings.HasPrefix(commandName, "ATD") || commandName == "ATA"
	m.mu.Lock()
	m.callResultExpected = callCommand
	m.mu.Unlock()
	defer func() { m.mu.Lock(); m.callResultExpected = false; m.mu.Unlock() }()
	if _, err := m.port.Write([]byte(cmd)); err != nil {
		return "", err
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	var out []string
	for {
		select {
		case <-m.done:
			return strings.Join(out, "\n"), fmt.Errorf("serial port closed")
		case <-timer.C:
			m.mu.Lock()
			m.status.Error = "AT timeout; port closed to avoid ambiguous retry"
			m.mu.Unlock()
			m.Close()
			return strings.Join(out, "\n"), fmt.Errorf("AT timeout; result uncertain; no automatic retry")
		case line := <-m.lines:
			if line == strings.TrimSpace(cmd) {
				continue
			}
			callResult := line == "NO CARRIER" || line == "BUSY" || line == "NO ANSWER"
			if callResult && !callCommand {
				continue
			}
			out = append(out, line)
			if line == "OK" || (prompt && line == ">") {
				return strings.Join(out, "\n"), nil
			}
			if line == "ERROR" || strings.HasPrefix(line, "+CME ERROR") || strings.HasPrefix(line, "+CMS ERROR") || callResult {
				return strings.Join(out, "\n"), fmt.Errorf("modem rejected command: %s", line)
			}
		}
	}
}
func (m *Modem) ExecuteATSilent(cmd string, t time.Duration) (string, error) {
	m.commandMu.Lock()
	defer m.commandMu.Unlock()
	return m.commandLocked(cmd+"\r", t, false)
}
func (m *Modem) Init() error {
	for _, cmd := range []string{"AT", "ATE0", "AT+CMEE=2"} {
		if _, err := m.ExecuteATSilent(cmd, 5*time.Second); err != nil {
			return err
		}
	}
	// SIM readiness errors must remain visible in the web UI, not prevent it
	// from starting. Commands are retried by the receiver when the SIM is ready.
	for _, cmd := range []string{"AT+CLIP=1", "AT+CMGF=0", "AT+CFUN=1"} {
		if _, err := m.ExecuteATSilent(cmd, 5*time.Second); err != nil {
			select {
			case <-m.done:
				return err
			default:
			}
		}
	}
	model, err := m.ExecuteATSilent("ATI", 3*time.Second)
	if err != nil {
		return err
	}
	m.mu.Lock()
	m.status.Model = model
	m.mu.Unlock()
	_ = m.Refresh()
	select {
	case <-m.done:
		return fmt.Errorf("serial port closed during initialization")
	default:
		return nil
	}
}
func (m *Modem) Refresh() error {
	values := make([]string, 6)
	var queryErrors []error
	for i, cmd := range []string{"AT+CPIN?", "AT+CREG?", "AT+CEREG?", "AT+CSQ", "AT+CFUN?", "AT+COPS?"} {
		resp, err := m.ExecuteATSilent(cmd, 3*time.Second)
		if err != nil {
			queryErrors = append(queryErrors, fmt.Errorf("%s: %w", cmd, err))
			if resp == "" {
				resp = "Unavailable"
			}
		}
		values[i] = resp
		select {
		case <-m.done:
			break
		default:
			continue
		}
		break
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.status.SIM = values[0]
	m.status.Registration = values[1] + "\n" + values[2]
	m.status.Signal = values[3]
	m.status.Radio = values[4]
	m.status.Operator = values[5]
	m.status.Registered = strings.Contains(values[0], "+CPIN: READY") && (registrationPattern.MatchString(values[1]) || registrationPattern.MatchString(values[2]))
	err := errors.Join(queryErrors...)
	select {
	case <-m.done:
		m.invalidateLiveStatusLocked()
	default:
		m.status.Connected = true
		m.status.LastUpdated = time.Now().UTC()
		m.status.Error = ""
	}
	if err != nil {
		m.status.Error = err.Error()
	}
	return err
}
func (m *Modem) DialCall(number string) error {
	if !m.trafficMu.TryRLock() {
		return fmt.Errorf("network selection in progress")
	}
	defer m.trafficMu.RUnlock()
	if !numberPattern.MatchString(number) {
		return fmt.Errorf("invalid phone number")
	}
	if !m.Snapshot().Registered {
		return fmt.Errorf("modem is not registered on cellular network")
	}
	if _, err := m.ExecuteATSilent("AT+QPCMV=1,2", 3*time.Second); err != nil {
		return err
	}
	_, err := m.ExecuteATSilent("ATD"+number+";", 20*time.Second)
	return err
}
func (m *Modem) AnswerCall() error {
	if !m.trafficMu.TryRLock() {
		return fmt.Errorf("network selection in progress")
	}
	defer m.trafficMu.RUnlock()
	if _, err := m.ExecuteATSilent("AT+QPCMV=1,2", 3*time.Second); err != nil {
		return err
	}
	_, err := m.ExecuteATSilent("ATA", 10*time.Second)
	return err
}
func (m *Modem) HangupCall() error { _, err := m.ExecuteATSilent("ATH", 5*time.Second); return err }
func (m *Modem) SendSMS(number, text string) error {
	if !m.trafficMu.TryRLock() {
		return fmt.Errorf("network selection in progress")
	}
	defer m.trafficMu.RUnlock()
	if !numberPattern.MatchString(number) || len(text) == 0 || len(text) > 2048 {
		return fmt.Errorf("invalid number or message")
	}
	if !m.Snapshot().Registered {
		return fmt.Errorf("modem is not registered on cellular network")
	}
	// sms.To assumes international numbering even without '+'. Preserve local
	// numbers and short codes as network-interpreted addresses instead.
	address := tpdu.NewAddress(tpdu.FromNumber(number))
	if !strings.HasPrefix(number, "+") {
		address.SetTypeOfNumber(tpdu.TonUnknown)
	}
	parts, err := sms.Encode([]byte(text), sms.AsSubmit, sms.TemplateOption{tpdu.WithDA(address)})
	if err != nil {
		return err
	}
	if len(parts) != 1 {
		return fmt.Errorf("single SMS only; shorten message to prevent partial multi-part submission")
	}
	raw, err := parts[0].MarshalBinary()
	if err != nil {
		return err
	}
	full := append([]byte{0}, raw...)
	m.commandMu.Lock()
	defer m.commandMu.Unlock()
	if _, err = m.commandLocked(fmt.Sprintf("AT+CMGS=%d\r", len(raw)), 5*time.Second, true); err != nil {
		return err
	}
	resp, err := m.commandLocked(strings.ToUpper(hex.EncodeToString(full))+"\x1a", 60*time.Second, false)
	if err != nil {
		return err
	}
	if !strings.Contains(resp, "+CMGS:") {
		return fmt.Errorf("missing modem submission reference")
	}
	return nil
}
func (m *Modem) Monitor(ctx context.Context) {
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-m.done:
			return
		case <-ticker.C:
			if err := m.Refresh(); err != nil {
				m.mu.Lock()
				m.status.Error = err.Error()
				m.mu.Unlock()
			}
		}
	}
}
