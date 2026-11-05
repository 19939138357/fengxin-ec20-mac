package winmodem

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/warthog618/sms"
)

func TestSMSWorkerReceivesWithoutBlockingCommandReader(t *testing.T) {
	m, f := testModem()
	pdu := incomingPDUs(t, "worker 收信")[0]
	f.answer = func(cmd string) {
		switch strings.TrimSpace(cmd) {
		case `AT+CPMS="SM"`, `AT+CPMS="ME"`:
			m.onLine("+CPMS: 1,50,1,50,1,50")
		case "AT+CMGR=6":
			m.onLine("+CMGR: 1,,23")
			m.onLine(pdu)
		}
		m.onLine("OK")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	incoming := make(chan IncomingSMS, 1)
	if err := m.StartSMSReceiver(ctx, t.TempDir(), func(msg IncomingSMS) error { incoming <- msg; return nil }); err != nil {
		t.Fatal(err)
	}
	deadline := time.After(2 * time.Second)
	for !m.SMSReceiverStatus().Ready {
		select {
		case <-deadline:
			t.Fatal("receiver did not initialize")
		case <-time.After(time.Millisecond):
		}
	}
	m.onLine(`+CMTI: "SM",6`)
	select {
	case msg := <-incoming:
		if msg.Text != "worker 收信" {
			t.Fatal("wrong worker SMS")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("worker failed to deliver")
	}
	cancel()
	deadline = time.After(2 * time.Second)
	for m.SMSReceiverStatus().Running {
		select {
		case <-deadline:
			t.Fatal("worker did not stop")
		case <-time.After(time.Millisecond):
		}
	}
}

func TestSMSURCDoesNotWaitForInboxDiskLock(t *testing.T) {
	m, _ := testModem()
	r := attachReceiver(t, m, t.TempDir(), func(IncomingSMS) error { return nil })
	// Simulate a slow inbox fsync. The serial reader must still route CMTI
	// and return immediately so subsequent AT responses remain readable.
	r.mu.Lock()
	defer r.mu.Unlock()
	done := make(chan struct{})
	go func() { m.onLine(`+CMTI: "SM",8`); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("serial reader blocked behind inbox persistence")
	}
	r.pendingMu.Lock()
	queued := r.pending[smsLocation{"SM", 8}]
	r.pendingMu.Unlock()
	if !queued {
		t.Fatal("SMS indication lost")
	}
}

func incomingPDUs(t *testing.T, text string) []string {
	t.Helper()
	parts, err := sms.Encode([]byte(text), sms.AsDeliver, sms.From("+8613012345678"))
	if err != nil {
		t.Fatal(err)
	}
	var pdus []string
	for _, p := range parts {
		p.SCTS.Time = time.Date(2026, 9, 23, 11, 12, 13, 0, time.FixedZone("CST", 8*3600))
		b, err := p.MarshalBinary()
		if err != nil {
			t.Fatal(err)
		}
		pdus = append(pdus, strings.ToUpper(hex.EncodeToString(append([]byte{0}, b...))))
	}
	return pdus
}

func attachReceiver(t *testing.T, m *Modem, dir string, cb func(IncomingSMS) error) *smsReceiver {
	t.Helper()
	r, err := openSMSReceiver(m, dir, cb)
	if err != nil {
		t.Fatal(err)
	}
	m.smsReceiver = r
	return r
}

func TestSMSURCInterleavingAndChineseCMGR(t *testing.T) {
	m, f := testModem()
	var received []IncomingSMS
	r := attachReceiver(t, m, t.TempDir(), func(msg IncomingSMS) error {
		// Callback may use AT without locking the receive worker or reader.
		if _, err := m.ExecuteATSilent("AT", time.Second); err != nil {
			return err
		}
		received = append(received, msg)
		return nil
	})
	pdu := incomingPDUs(t, "收到中文短信，测试自动转发。")[0]
	rings := 0
	m.SetRingCallback(func() { rings++ })
	f.answer = func(cmd string) {
		switch strings.TrimSpace(cmd) {
		case "AT+CSQ":
			m.onLine(`+CMTI: "SM",7`)
			m.onLine("RING")
			m.onLine("+CSQ: 20,99")
		case `AT+CPMS="SM"`:
			m.onLine("+CPMS: 1,50,1,50,1,50")
		case "AT+CMGR=7":
			m.onLine("+CMGR: 0,,38")
			m.onLine(pdu)
		case "AT":
		default:
			t.Errorf("unexpected command %s", cmd)
		}
		m.onLine("OK")
	}
	response, err := m.ExecuteATSilent("AT+CSQ", time.Second)
	if err != nil || response != "+CSQ: 20,99\nOK" || rings != 1 {
		t.Fatalf("interleaved URC corrupted AT: %q %v rings=%d", response, err, rings)
	}
	if err = r.processPending(); err != nil {
		t.Fatal(err)
	}
	if err = r.deliver(); err != nil {
		t.Fatal(err)
	}
	if len(received) != 1 || received[0].Text != "收到中文短信，测试自动转发。" || received[0].Number != "+8613012345678" || received[0].Historical {
		t.Fatalf("wrong decoded SMS: %+v", received)
	}
	if received[0].ReceivedAt.Format(time.RFC3339) != "2026-09-23T11:12:13+08:00" {
		t.Fatal("lost service-center timestamp")
	}
	// Repeated index URCs and read/unread status changes are not new messages.
	m.onLine(`+CMTI: "SM",7`)
	m.onLine(`+CMTI: "SM",7`)
	if err = r.processPending(); err != nil {
		t.Fatal(err)
	}
	if err = r.deliver(); err != nil {
		t.Fatal(err)
	}
	if len(received) != 1 {
		t.Fatal("duplicate notification")
	}
}

func TestSMSBaselineRestartDedupAndUnreadDoesNotMeanNew(t *testing.T) {
	m, f := testModem()
	dir := t.TempDir()
	var received []IncomingSMS
	cb := func(msg IncomingSMS) error { received = append(received, msg); return nil }
	r := attachReceiver(t, m, dir, cb)
	old := incomingPDUs(t, "old unread")[0]
	fresh := incomingPDUs(t, "new received")[0]
	secondScan := false
	f.answer = func(cmd string) {
		if strings.TrimSpace(cmd) == "AT+CMGL=4" {
			m.onLine("+CMGL: 1,0,,24")
			m.onLine(old)
			if secondScan {
				m.onLine("+CMGL: 2,1,,24")
				m.onLine(fresh)
			}
		}
		m.onLine("OK")
	}
	if err := r.scan(); err != nil {
		t.Fatal(err)
	}
	if err := r.deliver(); err != nil {
		t.Fatal(err)
	}
	if len(received) != 0 || len(m.SMSInbox()) != 1 || !m.SMSInbox()[0].Historical {
		t.Fatal("historical unread SMS forwarded")
	}
	secondScan = true
	if err := r.scan(); err != nil {
		t.Fatal(err)
	}
	if err := r.deliver(); err != nil {
		t.Fatal(err)
	}
	if len(received) != 1 || received[0].Text != "new received" {
		t.Fatalf("new read SMS not recovered: %+v", received)
	}
	// A replacement modem instance reading the same SIM produces no duplicates.
	r = attachReceiver(t, m, dir, cb)
	if err := r.scan(); err != nil {
		t.Fatal(err)
	}
	if err := r.deliver(); err != nil {
		t.Fatal(err)
	}
	if len(received) != 1 || len(m.SMSInbox()) != 2 {
		t.Fatal("restart duplicate or inbox loss")
	}
}

func TestSMSMultipartPersistsAcrossRestart(t *testing.T) {
	m, _ := testModem()
	dir := t.TempDir()
	text := strings.Repeat("这是中文长短信，按照正确的顺序拼接。", 11)
	pdus := incomingPDUs(t, text)
	if len(pdus) < 3 {
		t.Fatal("fixture is not multipart")
	}
	var received []IncomingSMS
	cb := func(msg IncomingSMS) error { received = append(received, msg); return nil }
	r := attachReceiver(t, m, dir, cb)
	if err := r.ingest(pdus[len(pdus)-1], false); err != nil {
		t.Fatal(err)
	}
	if err := r.persist(); err != nil {
		t.Fatal(err)
	}
	if err := r.deliver(); err != nil {
		t.Fatal(err)
	}
	if len(received) != 0 {
		t.Fatal("incomplete long SMS forwarded")
	}
	r = attachReceiver(t, m, dir, cb)
	for i := len(pdus) - 2; i >= 0; i-- {
		if err := r.ingest(pdus[i], false); err != nil {
			t.Fatal(err)
		}
	}
	if err := r.deliver(); err != nil {
		t.Fatal(err)
	}
	if len(received) != 1 || received[0].Text != text || received[0].Parts != len(pdus) {
		t.Fatalf("multipart did not assemble: count=%d", len(received))
	}
	r = attachReceiver(t, m, dir, cb)
	for _, pdu := range pdus {
		if err := r.ingest(pdu, false); err != nil {
			t.Fatal(err)
		}
	}
	if err := r.deliver(); err != nil {
		t.Fatal(err)
	}
	if len(received) != 1 {
		t.Fatal("replayed multipart was forwarded")
	}
}

func TestSMSDeliveryRetryUsesStableIDAndDurableMessage(t *testing.T) {
	m, _ := testModem()
	dir := t.TempDir()
	var id string
	r := attachReceiver(t, m, dir, func(msg IncomingSMS) error { id = msg.ID; return errors.New("outbox unavailable") })
	if err := r.ingest(incomingPDUs(t, "retry me")[0], false); err != nil {
		t.Fatal(err)
	}
	if err := r.deliver(); err == nil {
		t.Fatal("callback failure ignored")
	}
	calls := 0
	r = attachReceiver(t, m, dir, func(msg IncomingSMS) error {
		calls++
		if msg.ID != id {
			t.Fatal("unstable event ID")
		}
		return nil
	})
	if err := r.deliver(); err != nil {
		t.Fatal(err)
	}
	if err := r.deliver(); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("got %d callbacks after restart", calls)
	}
}

func TestMalformedUCS2DoesNotPoisonDedupOrHideOnRescan(t *testing.T) {
	m, _ := testModem()
	r := attachReceiver(t, m, t.TempDir(), func(IncomingSMS) error { return nil })
	parts, err := sms.Encode([]byte("你"), sms.AsDeliver, sms.From("+8613012345678"))
	if err != nil {
		t.Fatal(err)
	}
	parts[0].SCTS.Time = time.Date(2026, 9, 23, 11, 12, 13, 0, time.UTC)
	// A lone UTF-16 high surrogate is a syntactically valid TPDU payload
	// whose SMS text decoder must reject the incomplete Unicode sequence.
	parts[0].UD = []byte{0xD8, 0x00}
	raw, err := parts[0].MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	pdu := strings.ToUpper(hex.EncodeToString(append([]byte{0}, raw...)))
	if _, err := decodeReceivedPDU(pdu); err != nil {
		t.Fatalf("fixture must reach text decoding: %v", err)
	}
	for attempt := 0; attempt < 2; attempt++ {
		if err := r.ingest(pdu, false); err == nil || !strings.Contains(err.Error(), "SMS text decode failed") {
			t.Fatalf("attempt %d silently ignored malformed text: %v", attempt, err)
		}
		if len(r.state.Parts) != 0 || len(r.state.Messages) != 0 {
			t.Fatal("failed decode left a permanent dedup entry")
		}
	}
	if err := r.ingest(incomingPDUs(t, "后续正常短信")[0], false); err != nil {
		t.Fatal(err)
	}
	if len(r.state.Messages) != 1 {
		t.Fatal("malformed message blocked later valid message")
	}
}

func TestSMSFailedSIMScanDoesNotCreateBaseline(t *testing.T) {
	m, f := testModem()
	r := attachReceiver(t, m, t.TempDir(), func(IncomingSMS) error { return nil })
	f.answer = func(string) { m.onLine("+CMS ERROR: 310") }
	if err := r.scan(); err == nil {
		t.Fatal("SIM failure ignored")
	}
	if len(r.state.Baselines) != 0 {
		t.Fatal("baselined unreadable SIM")
	}
	if _, err := openSMSReceiver(m, filepath.Dir(r.path), func(IncomingSMS) error { return nil }); err != nil {
		t.Fatal(err)
	}
}

func TestParseSMSRecordsStatusesAndIncompleteResponses(t *testing.T) {
	pdu := incomingPDUs(t, "test")[0]
	for _, status := range []int{0, 1} {
		items, err := parseSMSRecords(fmt.Sprintf("+CMGR: %d,,23\n%s\nOK", status, pdu), false)
		if err != nil || len(items) != 1 || items[0].status != status {
			t.Fatal("CMGR read/unread parse failed", err)
		}
	}
	for _, bad := range []string{"+CMGR: 0,,23\nOK", "OK", "+CMGR: x,,23\n" + pdu} {
		if _, err := parseSMSRecords(bad, false); err == nil {
			t.Fatal("accepted incomplete/malformed response")
		}
	}
}

func TestRefreshSIMFailureClearsStaleRegistrationAndRecovers(t *testing.T) {
	m, f := testModem()
	m.status = Status{SIM: "+CPIN: READY", Registered: true, Signal: "old"}
	ready := false
	f.answer = func(cmd string) {
		switch strings.TrimSpace(cmd) {
		case "AT+CPIN?":
			if !ready {
				m.onLine("+CME ERROR: SIM failure")
				return
			}
			m.onLine("+CPIN: READY")
		case "AT+CREG?":
			m.onLine("+CREG: 0,1")
		case "AT+CEREG?":
			m.onLine("+CEREG: 0,1")
		case "AT+CSQ":
			m.onLine("+CSQ: 19,99")
		case "AT+CFUN?":
			m.onLine("+CFUN: 1")
		case "AT+COPS?":
			m.onLine(`+COPS: 0,2,"46001",7`)
		}
		m.onLine("OK")
	}
	if err := m.Refresh(); err == nil {
		t.Fatal("SIM error not returned")
	}
	s := m.Snapshot()
	if s.Registered || s.Signal == "old" || s.SIM == "+CPIN: READY" || s.Error == "" {
		t.Fatalf("stale status: %+v", s)
	}
	ready = true
	if err := m.Refresh(); err != nil {
		t.Fatal(err)
	}
	s = m.Snapshot()
	if !s.Registered || s.Error != "" {
		t.Fatalf("status did not recover: %+v", s)
	}
}

func TestInitSIMFailureStillAllowsStatusUI(t *testing.T) {
	m, f := testModem()
	f.answer = func(cmd string) {
		switch strings.TrimSpace(cmd) {
		case "AT+CPIN?", "AT+CLIP=1", "AT+CMGF=0":
			m.onLine("+CME ERROR: SIM failure")
			return
		case "ATI":
			m.onLine("EC20F")
		}
		m.onLine("OK")
	}
	if err := m.Init(); err != nil {
		t.Fatalf("live AT port should permit UI despite SIM error: %v", err)
	}
	if m.Snapshot().Error == "" || m.Snapshot().Registered {
		t.Fatal("SIM failure not exposed")
	}
}
