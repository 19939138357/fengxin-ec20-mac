package winmodem

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"go.bug.st/serial"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestSMSDestinationDoesNotTurnLocalNumberIntoInternational(t *testing.T) {
	for _, tc := range []struct {
		number string
		toa    byte
		digits string
	}{
		{"17325817368", 0x81, "7123857163F8"},
		{"+8617325817368", 0x91, "687123857163F8"},
		{"10000", 0x81, "0100F0"},
	} {
		t.Run(tc.number, func(t *testing.T) {
			m, f := testModem()
			m.status.Registered = true
			f.answer = func(s string) {
				if strings.HasPrefix(s, "AT+CMGS=") {
					m.onLine(">")
				} else {
					m.onLine("+CMGS: 3")
					m.onLine("OK")
				}
			}
			if err := m.SendSMS(tc.number, "test"); err != nil {
				t.Fatal(err)
			}
			pdu, err := hex.DecodeString(strings.TrimSuffix(f.writes[1], "\x1a"))
			if err != nil {
				t.Fatal(err)
			}
			if len(pdu) < 5 || pdu[4] != tc.toa {
				t.Fatalf("destination TOA=%02X want=%02X", pdu[4], tc.toa)
			}
			address := strings.ToUpper(hex.EncodeToString(pdu[5 : 5+(int(pdu[3])+1)/2]))
			if address != tc.digits {
				t.Fatalf("encoded destination %s want %s", address, tc.digits)
			}
		})
	}
}

type unpluggedPort struct {
	*fakePort
	unplug chan struct{}
}

func (p *unpluggedPort) Read([]byte) (int, error) {
	<-p.unplug
	return 0, errors.New("USB device disconnected")
}

func TestSerialDisconnectInvalidatesLiveReadings(t *testing.T) {
	m, f := testModem()
	f.answer = func(command string) {
		switch strings.TrimSpace(command) {
		case "AT+CPIN?":
			m.onLine("+CPIN: READY")
		case "AT+CREG?":
			m.onLine("+CREG: 0,1")
		case "AT+CEREG?":
			m.onLine("+CEREG: 0,1")
		case "AT+CSQ":
			m.onLine("+CSQ: 25,99")
		case "AT+CFUN?":
			m.onLine("+CFUN: 1")
		case "AT+COPS?":
			m.onLine(`+COPS: 0,2,"46001",7`)
		}
		m.onLine("OK")
	}
	if err := m.Refresh(); err != nil {
		t.Fatal(err)
	}
	if !m.Snapshot().Registered {
		t.Fatal("fixture did not begin registered")
	}
	p := &unpluggedPort{fakePort: f, unplug: make(chan struct{})}
	m.port = p
	go m.readLoop()
	close(p.unplug)
	select {
	case <-m.done:
	case <-time.After(time.Second):
		t.Fatal("disconnect was not detected")
	}
	s := m.Snapshot()
	if s.Registered || s.SIM != "" || s.Registration != "" || s.Signal != "" || s.Radio != "" || s.Operator != "" {
		t.Fatalf("disconnected modem retained live readings: %+v", s)
	}
	var public map[string]any
	encoded, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	if err = json.Unmarshal(encoded, &public); err != nil {
		t.Fatal(err)
	}
	if connected, ok := public["connected"].(bool); !ok || connected {
		t.Fatal("missing explicit disconnected state")
	}
	if updated, ok := public["last_updated"].(string); !ok || updated == "" || strings.HasPrefix(updated, "0001-") {
		t.Fatal("missing status update time")
	}
	// Calling Refresh on the closed instance must not revive stale data.
	if err = m.Refresh(); err == nil {
		t.Fatal("closed instance refreshed successfully")
	}
	s = m.Snapshot()
	if s.SIM != "" || s.Registration != "" || s.Signal != "" {
		t.Fatal("failed refresh repopulated disconnected readings")
	}
}

func TestDisconnectDuringRefreshCannotRestoreConnectedReadings(t *testing.T) {
	m, f := testModem()
	lastQuery := make(chan struct{})
	release := make(chan struct{})
	f.answer = func(command string) {
		switch strings.TrimSpace(command) {
		case "AT+CPIN?":
			m.onLine("+CPIN: READY")
		case "AT+CREG?":
			m.onLine("+CREG: 0,1")
		case "AT+CEREG?":
			m.onLine("+CEREG: 0,1")
		case "AT+CSQ":
			m.onLine("+CSQ: 25,99")
		case "AT+CFUN?":
			m.onLine("+CFUN: 1")
		case "AT+COPS?":
			close(lastQuery)
			<-release
			m.onLine(`+COPS: 0,2,"46001",7`)
		}
		m.onLine("OK")
	}
	finished := make(chan struct{})
	go func() { _ = m.Refresh(); close(finished) }()
	<-lastQuery
	m.Close()
	close(release)
	select {
	case <-finished:
	case <-time.After(time.Second):
		t.Fatal("in-flight refresh did not finish after disconnect")
	}
	s := m.Snapshot()
	if s.Connected || s.Registered || s.SIM != "" || s.Signal != "" || s.Radio != "" || s.Operator != "" || s.Registration != "" {
		t.Fatalf("late query responses resurrected disconnected status: %+v", s)
	}
}

type fakePort struct {
	serial.Port
	mu     sync.Mutex
	writes []string
	lines  chan string
	closed bool
	answer func(string)
}

func (f *fakePort) Write(b []byte) (int, error) {
	s := string(b)
	f.mu.Lock()
	f.writes = append(f.writes, s)
	f.mu.Unlock()
	if f.answer != nil {
		f.answer(s)
	}
	return len(b), nil
}
func (f *fakePort) Close() error { f.closed = true; return nil }
func testModem() (*Modem, *fakePort) {
	f := &fakePort{}
	m := &Modem{port: f, lines: make(chan string, 32), done: make(chan struct{}), pcm: make(chan int, 8)}
	f.lines = m.lines
	return m, f
}
func TestRegistrationParsing(t *testing.T) {
	for _, s := range []string{"+CREG: 0,1\nOK", "+CEREG: 2,5,\"1234\",\"ABC\",7\nOK"} {
		if !registrationPattern.MatchString(s) {
			t.Fatalf("registration missed %s", s)
		}
	}
	if registrationPattern.MatchString("+CEREG: 0,2\nOK") {
		t.Fatal("searching considered registered")
	}
}
func TestSMSSendsOnlyWithRegistrationAndReference(t *testing.T) {
	m, f := testModem()
	if err := m.SendSMS("+8613800000000", "hello"); err == nil || len(f.writes) != 0 {
		t.Fatal("sent while offline")
	}
	m.status.Registered = true
	f.answer = func(s string) {
		if strings.HasPrefix(s, "AT+CMGS=") {
			m.lines <- ">"
		} else if strings.HasSuffix(s, "\x1a") {
			m.lines <- "+CMGS: 42"
			m.lines <- "OK"
		} else {
			t.Errorf("unexpected AT command")
		}
	}
	if err := m.SendSMS("+8613800000000", "hello"); err != nil {
		t.Fatal(err)
	}
	if len(f.writes) != 2 || !strings.HasPrefix(f.writes[1], "00") {
		t.Fatal("wrong PDU sequence")
	}
	f.answer = func(s string) {
		if strings.HasPrefix(s, "AT+CMGS=") {
			m.lines <- ">"
		} else {
			m.lines <- "OK"
		}
	}
	if err := m.SendSMS("+8613800000000", "hello"); err == nil {
		t.Fatal("accepted without CMGS reference")
	}
}
func TestATTimeoutClosesPortWithoutRetry(t *testing.T) {
	m, f := testModem()
	if _, err := m.ExecuteATSilent("AT", time.Millisecond); err == nil || !f.closed {
		t.Fatal("timeout did not close ambiguous session")
	}
	m.ExecuteATSilent("AT", time.Millisecond)
	if len(f.writes) != 1 {
		t.Fatal("retried timed out session")
	}
}

func TestCallHangupURCDoesNotTerminateUnrelatedCommand(t *testing.T) {
	for _, command := range []string{"AT+CMGL=4", `AT+CPMS="SM"`, "AT+CPIN?", "ATH"} {
		for _, result := range []string{"NO CARRIER", "BUSY", "NO ANSWER"} {
			t.Run(command+"/"+result, func(t *testing.T) {
				m, f := testModem()
				hangups := 0
				m.SetHangupCallback(func() { hangups++ })
				release := make(chan struct{})
				seenURC := make(chan struct{})
				f.answer = func(cmd string) {
					if strings.TrimSpace(cmd) == command {
						go func() { m.onLine(result); close(seenURC); <-release; m.onLine("+EXPECTED: first"); m.onLine("OK") }()
					} else {
						m.onLine("+EXPECTED: second")
						m.onLine("OK")
					}
				}
				type answer struct {
					response string
					err      error
				}
				answers := make(chan answer, 1)
				go func() { response, err := m.ExecuteATSilent(command, time.Second); answers <- answer{response, err} }()
				<-seenURC
				select {
				case a := <-answers:
					close(release)
					t.Fatalf("URC prematurely ended command: %+v", a)
				case <-time.After(15 * time.Millisecond):
				}
				close(release)
				a := <-answers
				if a.err != nil || a.response != "+EXPECTED: first\nOK" || hangups != 1 {
					t.Fatalf("first response corrupted: %+v, callbacks=%d", a, hangups)
				}
				response, err := m.ExecuteATSilent("AT+CSQ", time.Second)
				if err != nil || response != "+EXPECTED: second\nOK" {
					t.Fatalf("trailing OK contaminated next command: %q %v", response, err)
				}
			})
		}
	}
}

func TestDialAndAnswerStillAcceptCallFailureAsFinalResult(t *testing.T) {
	for _, command := range []string{"ATD12345;", "ATA"} {
		m, f := testModem()
		f.answer = func(string) { m.onLine("NO CARRIER") }
		response, err := m.ExecuteATSilent(command, time.Second)
		if err == nil || response != "NO CARRIER" || f.closed {
			t.Fatalf("call final result lost: %q %v", response, err)
		}
	}
}
