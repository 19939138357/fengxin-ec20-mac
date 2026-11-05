package winmodem

import (
	"strings"
	"testing"
	"time"
)

func TestRadioDiagnosticOnlyQueriesAndRedactsUnsolicitedText(t *testing.T) {
	m, f := testModem()
	f.answer = func(s string) {
		m.onLine("private unsolicited SMS body")
		m.onLine(`+QNWLOCK: "common/lte",0,0,0,0`)
		m.onLine("+CEER: 0,-1")
		m.onLine("OK")
	}
	r, err := m.RunNetwork(NetworkRequest{Action: "radio-diagnose"})
	if err != nil {
		t.Fatal(err)
	}
	for _, command := range f.writes {
		command = strings.TrimSpace(command)
		if command != "AT+CEER" && (!strings.HasSuffix(command, `"`) || strings.Contains(command, ",")) {
			t.Fatalf("not a read-only query: %q", command)
		}
	}
	joined := strings.Join(r.Log, "\n")
	if len(f.writes) != 7 || strings.Contains(joined, "private unsolicited") || !strings.Contains(joined, "+QNWLOCK:") || !strings.Contains(joined, "+CEER:") {
		t.Fatal(joined)
	}
}

func TestReadOnlyDiagnosticRedactsIdentityAndCaller(t *testing.T) {
	m, f := testModem()
	f.answer = func(s string) {
		if strings.TrimSpace(s) == "AT+CIMI" {
			m.onLine("460111234567890")
		}
		if strings.TrimSpace(s) == "AT+CLCC" {
			m.onLine(`+CLCC: 1,0,0,1,0,"1234567890",129`)
		}
		m.onLine("OK")
	}
	r, err := m.RunNetwork(NetworkRequest{Action: "diagnose"})
	joined := strings.Join(r.Log, "\n")
	if err != nil || !strings.Contains(joined, "IMSI prefix: 46011") || strings.Contains(joined, "1234567890") || strings.Contains(joined, "460111234567890") {
		t.Fatalf("bad redaction: %v %s", err, joined)
	}
	if len(f.writes) != 6 {
		t.Fatalf("unexpected commands: %d", len(f.writes))
	}
}

func TestSIMReinitializeRestoresRadioAndProtectsCalls(t *testing.T) {
	for _, busy := range []bool{false, true} {
		m, f := testModem()
		f.answer = func(s string) {
			if busy && strings.TrimSpace(s) == "AT+CLCC" {
				m.onLine("+CLCC: 1,0,0,0,0")
			}
			m.onLine("OK")
		}
		_, err := m.RunNetwork(NetworkRequest{Action: "sim-reinitialize"})
		if busy {
			if err == nil || len(f.writes) != 1 {
				t.Fatal("active call interrupted")
			}
			continue
		}
		if err != nil || len(f.writes) != 3 || strings.TrimSpace(f.writes[1]) != "AT+CFUN=0" || strings.TrimSpace(f.writes[2]) != "AT+CFUN=1" {
			t.Fatalf("radio restore missing: %v %v", f.writes, err)
		}
	}
}

func TestNetworkAllowsLTEDataButProtectsVoice(t *testing.T) {
	for _, raw := range []string{`+CLCC: 1,1,0,0,0`, `+CLCC: 1,0,2,0,0`, `+CLCC: 1,1,4,0,0`, `+CLCC: malformed`} {
		if !hasNonDataCall(raw) {
			t.Fatal("unprotected call", raw)
		}
	}
	m, f := testModem()
	f.answer = func(s string) {
		if strings.TrimSpace(s) == "AT+CLCC" {
			m.onLine("+CLCC: 1,1,0,1,0")
			m.onLine("+CLCC: 2,1,0,1,0")
		}
		m.onLine("OK")
	}
	if _, err := m.RunNetwork(NetworkRequest{Action: "scan"}); err != nil {
		t.Fatal(err)
	}
	if len(f.writes) != 2 || strings.TrimSpace(f.writes[1]) != "AT+COPS=?" {
		t.Fatal("scan not executed")
	}
}

func TestNetworkValidationAndCommands(t *testing.T) {
	for _, r := range []NetworkRequest{{Action: "manual", PLMN: "46011\rATD123;", RAT: 7}, {Action: "manual", PLMN: "46011", RAT: 99}, {Action: "reset"}} {
		m, f := testModem()
		if _, err := m.RunNetwork(r); err == nil || len(f.writes) != 0 {
			t.Fatal("invalid input reached serial", r)
		}
	}
	for _, tc := range []struct {
		r    NetworkRequest
		want string
	}{{NetworkRequest{Action: "manual", PLMN: "46011", RAT: 7, Fallback: true}, `AT+COPS=4,2,"46011",7`}, {NetworkRequest{Action: "manual", PLMN: "46001", RAT: -1}, `AT+COPS=1,2,"46001"`}, {NetworkRequest{Action: "auto"}, "AT+COPS=0"}} {
		got, err := tc.r.Command()
		if err != nil || got != tc.want {
			t.Fatalf("%q %v", got, err)
		}
	}
}
func TestNetworkScanParsesOperatorsWithoutCapabilityTuples(t *testing.T) {
	m, f := testModem()
	f.answer = func(s string) {
		if strings.TrimSpace(s) == "AT+COPS=?" {
			m.onLine(`+COPS: (1,"China, Telecom","CT","46011",7),(3,"Other","","46001",7),(0,"","","310260"),,(0,1,2,3,4),(0,1,2)`)
		}
		m.onLine("OK")
	}
	out, err := m.RunNetwork(NetworkRequest{Action: "scan"})
	if err != nil || len(out.Operators) != 3 {
		t.Fatalf("%+v %v", out, err)
	}
	if out.Operators[0].Name != "China, Telecom" || out.Operators[1].Status != 3 || out.Operators[2].RAT != -1 {
		t.Fatal(out)
	}
}
func TestNetworkDoesNotInterruptExistingCall(t *testing.T) {
	m, f := testModem()
	f.answer = func(s string) { m.onLine(`+CLCC: 1,0,0,0,0,"1234567890",129`); m.onLine("OK") }
	out, err := m.RunNetwork(NetworkRequest{Action: "auto"})
	if err == nil || len(f.writes) != 1 || len(out.Log) != 0 {
		t.Fatal("existing call was not protected", out, err)
	}
}
func TestNetworkSerializesAndBlocksBusiness(t *testing.T) {
	m, f := testModem()
	m.status.Registered = true
	entered := make(chan struct{})
	release := make(chan struct{})
	done := make(chan error, 1)
	f.answer = func(s string) {
		if strings.TrimSpace(s) == "AT+COPS=?" {
			close(entered)
			<-release
		}
		m.onLine("OK")
	}
	go func() { _, err := m.RunNetwork(NetworkRequest{Action: "scan"}); done <- err }()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("not started")
	}
	if err := m.SendSMS("12345", "test"); err == nil {
		t.Fatal("SMS entered during scan")
	}
	if err := m.DialCall("12345"); err == nil {
		t.Fatal("call entered during scan")
	}
	if _, err := m.RunNetwork(NetworkRequest{Action: "auto"}); err == nil {
		t.Fatal("overlapping operation")
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}
func TestNetworkErrorsNotEmptySuccessAndOKNotRegistration(t *testing.T) {
	m, f := testModem()
	f.answer = func(s string) {
		if strings.TrimSpace(s) == "AT+COPS=?" {
			m.onLine("+CME ERROR: operation not allowed")
		} else {
			m.onLine("OK")
		}
	}
	out, err := m.RunNetwork(NetworkRequest{Action: "scan"})
	if err == nil || len(out.Operators) != 0 || !strings.Contains(strings.Join(out.Log, " "), "operation not allowed") {
		t.Fatal(out, err)
	}
	if _, err = m.RunNetwork(NetworkRequest{Action: "auto"}); err != nil || m.Snapshot().Registered {
		t.Fatal("command OK falsely reported registration")
	}
}
