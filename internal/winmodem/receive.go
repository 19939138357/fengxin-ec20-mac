package winmodem

import (
	"context"
	"crypto/sha256"
	"encoding/csv"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/warthog618/sms"
	"github.com/warthog618/sms/encoding/tpdu"
)

// IncomingSMS is a complete received SMS. ID is stable across scans/restarts.
// Historical records are imported on the first successful storage scan and
// are visible in the inbox but never passed to the notification callback.
type IncomingSMS struct {
	ID         string    `json:"id"`
	Number     string    `json:"number"`
	Text       string    `json:"text"`
	ReceivedAt time.Time `json:"received_at"`
	StoredAt   time.Time `json:"stored_at"`
	Historical bool      `json:"historical"`
	Parts      int       `json:"parts"`
}

type SMSReceiveStatus struct {
	Running      bool                        `json:"running"`
	Ready        bool                        `json:"ready"`
	LastScan     time.Time                   `json:"last_scan,omitempty"`
	Messages     int                         `json:"messages"`
	PendingParts int                         `json:"pending_parts"`
	Pending      int                         `json:"pending"`
	Error        string                      `json:"error,omitempty"`
	Storage      map[string]SMSStorageStatus `json:"storage,omitempty"`
	StorageFull  bool                        `json:"storage_full"`
}

type SMSStorageStatus struct {
	Used     int `json:"used"`
	Capacity int `json:"capacity"`
}

type storedPart struct {
	PDU        string    `json:"pdu"`
	FirstSeen  time.Time `json:"first_seen"`
	Historical bool      `json:"historical"`
	MessageID  string    `json:"message_id,omitempty"`
}
type receiveState struct {
	Version   int                    `json:"version"`
	Baselines map[string]bool        `json:"baselines"`
	Parts     map[string]storedPart  `json:"parts"`
	Messages  map[string]IncomingSMS `json:"messages"`
	Delivered map[string]bool        `json:"delivered"`
}
type smsLocation struct {
	memory string
	index  int
}
type smsRecord struct {
	index  int
	status int
	pdu    string
}
type smsReceiver struct {
	m         *Modem
	path      string
	callback  func(IncomingSMS) error
	mu        sync.Mutex
	state     receiveState
	status    SMSReceiveStatus
	pendingMu sync.Mutex
	pending   map[smsLocation]bool
	wake      chan struct{}
	// Only the worker issues AT commands or mutates durable state. mu also
	// protects readers, which never need the modem's command mutex.
}

// StartSMSReceiver starts a single asynchronous receiver. It does not require
// an already registered network/SIM. Setup errors are reported through status
// and retried every 30 seconds. callback must be idempotent by ID and return nil
// only after durably accepting the event; it runs outside all modem locks.
func (m *Modem) StartSMSReceiver(ctx context.Context, dataDir string, callback func(IncomingSMS) error) error {
	if callback == nil {
		return fmt.Errorf("SMS notification callback is required")
	}
	r, err := openSMSReceiver(m, dataDir, callback)
	if err != nil {
		return err
	}
	m.mu.Lock()
	if m.smsReceiver != nil {
		m.mu.Unlock()
		return fmt.Errorf("SMS receiver already started")
	}
	m.smsReceiver = r
	m.mu.Unlock()
	go r.run(ctx)
	return nil
}

func openSMSReceiver(m *Modem, dir string, callback func(IncomingSMS) error) (*smsReceiver, error) {
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	r := &smsReceiver{m: m, path: filepath.Join(dir, "received.json"), callback: callback, pending: map[smsLocation]bool{}, wake: make(chan struct{}, 1)}
	b, err := os.ReadFile(r.path)
	if err == nil {
		if err = json.Unmarshal(b, &r.state); err != nil {
			return nil, fmt.Errorf("SMS state invalid; preserving original file: %w", err)
		}
		if r.state.Version != 1 {
			return nil, fmt.Errorf("unsupported SMS state version")
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	r.state.Version = 1
	if r.state.Baselines == nil {
		r.state.Baselines = map[string]bool{}
	}
	if r.state.Parts == nil {
		r.state.Parts = map[string]storedPart{}
	}
	if r.state.Messages == nil {
		r.state.Messages = map[string]IncomingSMS{}
	}
	if r.state.Delivered == nil {
		r.state.Delivered = map[string]bool{}
	}
	r.status.Running = true
	return r, nil
}

func (m *Modem) SMSInbox() []IncomingSMS {
	m.mu.RLock()
	r := m.smsReceiver
	m.mu.RUnlock()
	if r == nil {
		return []IncomingSMS{}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	items := make([]IncomingSMS, 0, len(r.state.Messages))
	for _, item := range r.state.Messages {
		items = append(items, item)
	}
	sort.Slice(items, func(i, j int) bool { return items[i].StoredAt.After(items[j].StoredAt) })
	return items
}

func (m *Modem) SMSReceiverStatus() SMSReceiveStatus {
	m.mu.RLock()
	r := m.smsReceiver
	m.mu.RUnlock()
	if r == nil {
		return SMSReceiveStatus{}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	s := r.status
	s.Storage = make(map[string]SMSStorageStatus, len(r.status.Storage))
	for memory, storage := range r.status.Storage {
		s.Storage[memory] = storage
		if storage.Capacity > 0 && storage.Used >= storage.Capacity {
			s.StorageFull = true
		}
	}
	s.Messages = len(r.state.Messages)
	for _, p := range r.state.Parts {
		if p.MessageID == "" {
			s.PendingParts++
		}
	}
	for id, msg := range r.state.Messages {
		if !msg.Historical && !r.state.Delivered[id] {
			s.Pending++
		}
	}
	return s
}

// Called by the serial reader. It never performs AT or waits on the worker.
// Index notifications are coalesced, so repeated CMTI cannot fill a channel
// and block the AT response parser.
func (m *Modem) onSMSLine(line string) bool {
	if !strings.HasPrefix(line, "+CMTI:") {
		return false
	}
	fields, err := csv.NewReader(strings.NewReader(strings.TrimSpace(strings.TrimPrefix(line, "+CMTI:")))).Read()
	if err != nil || len(fields) != 2 {
		return true
	}
	mem := strings.ToUpper(strings.TrimSpace(fields[0]))
	index, err := strconv.Atoi(strings.TrimSpace(fields[1]))
	if err != nil || index < 0 || index > 100000 || (mem != "SM" && mem != "ME" && mem != "MT") {
		return true
	}
	if mem == "MT" {
		mem = "ME"
	} // EC2x AT manual defines MT as ME.
	m.mu.RLock()
	r := m.smsReceiver
	m.mu.RUnlock()
	if r != nil {
		r.pendingMu.Lock()
		r.pending[smsLocation{mem, index}] = true
		r.pendingMu.Unlock()
		select {
		case r.wake <- struct{}{}:
		default:
		}
	}
	return true
}

func (r *smsReceiver) run(ctx context.Context) {
	defer func() { r.mu.Lock(); r.status.Running = false; r.status.Ready = false; r.mu.Unlock() }()
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	configured := false
	for {
		select {
		case <-ctx.Done():
			return
		case <-r.m.done:
			return
		default:
		}
		var err error
		if !configured {
			for _, cmd := range []string{"AT+CMGF=0", "AT+CNMI=2,1,0,0,0", "AT+CLIP=1"} {
				if _, err = r.m.ExecuteATSilent(cmd, 5*time.Second); err != nil {
					break
				}
			}
			configured = err == nil
		}
		if err == nil {
			err = r.processPending()
			if scanErr := r.scan(); scanErr != nil {
				err = errors.Join(err, scanErr)
			}
			if deliverErr := r.deliver(); deliverErr != nil {
				err = errors.Join(err, deliverErr)
			}
		}
		r.mu.Lock()
		r.status.Ready = err == nil
		r.status.Error = ""
		if err != nil {
			r.status.Error = err.Error()
		}
		r.mu.Unlock()
		select {
		case <-ctx.Done():
			return
		case <-r.m.done:
			return
		case <-r.wake:
		case <-ticker.C:
		}
	}
}

func (r *smsReceiver) processPending() error {
	r.pendingMu.Lock()
	locations := make([]smsLocation, 0, len(r.pending))
	for loc := range r.pending {
		locations = append(locations, loc)
	}
	r.pendingMu.Unlock()
	var errs []error
	for _, loc := range locations {
		r.m.commandMu.Lock()
		storageResponse, err := r.m.commandLocked(`AT+CPMS="`+loc.memory+`"`+"\r", 5*time.Second, false)
		var response string
		if err == nil {
			response, err = r.m.commandLocked(fmt.Sprintf("AT+CMGR=%d\r", loc.index), 5*time.Second, false)
		}
		r.m.commandMu.Unlock()
		r.updateStorage(loc.memory, storageResponse)
		if err != nil {
			errs = append(errs, fmt.Errorf("read incoming SMS: %w", err))
			continue
		}
		records, err := parseSMSRecords(response, false)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		r.mu.Lock()
		for _, record := range records {
			if record.status <= 1 {
				err = r.ingest(record.pdu, false)
			}
			if err != nil {
				break
			}
		}
		if err == nil {
			err = r.persist()
		}
		if err == nil {
			r.pendingMu.Lock()
			delete(r.pending, loc)
			r.pendingMu.Unlock()
		}
		r.mu.Unlock()
		if err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func (r *smsReceiver) scan() error {
	var errs []error
	for _, mem := range []string{"SM", "ME"} {
		r.m.commandMu.Lock()
		storageResponse, err := r.m.commandLocked(`AT+CPMS="`+mem+`"`+"\r", 5*time.Second, false)
		var response string
		if err == nil {
			response, err = r.m.commandLocked("AT+CMGL=4\r", 10*time.Second, false)
		}
		r.m.commandMu.Unlock()
		r.updateStorage(mem, storageResponse)
		if err != nil {
			errs = append(errs, fmt.Errorf("scan SMS %s: %w", mem, err))
			continue
		}
		records, err := parseSMSRecords(response, true)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		r.mu.Lock()
		for _, record := range records {
			if record.status > 1 {
				continue
			} // Do not turn STO SENT/SUBMIT into incoming messages.
			r.pendingMu.Lock()
			historical := !r.state.Baselines[mem] && !r.pending[smsLocation{mem, record.index}]
			r.pendingMu.Unlock()
			if err = r.ingest(record.pdu, historical); err != nil {
				break
			}
		}
		if err == nil {
			r.state.Baselines[mem] = true
			err = r.persist()
		}
		if err == nil {
			r.status.LastScan = time.Now().UTC()
		}
		r.mu.Unlock()
		if err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func (r *smsReceiver) updateStorage(memory, response string) {
	for _, line := range strings.Split(response, "\n") {
		if !strings.HasPrefix(line, "+CPMS:") {
			continue
		}
		fields := strings.Split(strings.TrimSpace(strings.TrimPrefix(line, "+CPMS:")), ",")
		if len(fields) < 2 {
			return
		}
		used, err := strconv.Atoi(strings.TrimSpace(fields[0]))
		if err != nil {
			return
		}
		capacity, err := strconv.Atoi(strings.TrimSpace(fields[1]))
		if err != nil {
			return
		}
		r.mu.Lock()
		if r.status.Storage == nil {
			r.status.Storage = map[string]SMSStorageStatus{}
		}
		r.status.Storage[memory] = SMSStorageStatus{Used: used, Capacity: capacity}
		r.mu.Unlock()
		return
	}
}

func parseSMSRecords(response string, list bool) ([]smsRecord, error) {
	var records []smsRecord
	var pending *smsRecord
	for _, line := range strings.Split(response, "\n") {
		line = strings.TrimSpace(line)
		prefix := "+CMGR:"
		if list {
			prefix = "+CMGL:"
		}
		if strings.HasPrefix(line, prefix) {
			if pending != nil {
				return nil, fmt.Errorf("SMS response missing PDU")
			}
			fields, err := csv.NewReader(strings.NewReader(strings.TrimSpace(strings.TrimPrefix(line, prefix)))).Read()
			minimum := 3
			if list {
				minimum = 4
			}
			if err != nil || len(fields) < minimum {
				return nil, fmt.Errorf("malformed SMS response header")
			}
			record := smsRecord{}
			stat := 0
			if list {
				record.index, err = strconv.Atoi(strings.TrimSpace(fields[0]))
				stat = 1
				if err != nil {
					return nil, fmt.Errorf("invalid SMS index")
				}
			}
			record.status, err = strconv.Atoi(strings.TrimSpace(fields[stat]))
			if err != nil || record.status < 0 || record.status > 3 {
				return nil, fmt.Errorf("invalid SMS status")
			}
			pending = &record
		} else if pending != nil && line != "" {
			if _, err := hex.DecodeString(line); err != nil {
				return nil, fmt.Errorf("SMS response contains invalid PDU")
			}
			pending.pdu = strings.ToUpper(line)
			records = append(records, *pending)
			pending = nil
		}
	}
	if pending != nil {
		return nil, fmt.Errorf("SMS response missing PDU")
	}
	if !list && len(records) != 1 {
		return nil, fmt.Errorf("SMS response missing message")
	}
	return records, nil
}

func decodeReceivedPDU(raw string) (*tpdu.TPDU, error) {
	b, err := hex.DecodeString(raw)
	if err != nil || len(b) < 2 || int(b[0])+1 >= len(b) {
		return nil, fmt.Errorf("invalid received SMS PDU")
	}
	p, err := sms.Unmarshal(b[int(b[0])+1:])
	if err != nil {
		return nil, fmt.Errorf("cannot decode received SMS PDU: %w", err)
	}
	if p.MTI() != 0 {
		return nil, fmt.Errorf("received SMS is not SMS-DELIVER")
	}
	return p, nil
}

func digest(s string) string { sum := sha256.Sum256([]byte(s)); return hex.EncodeToString(sum[:]) }

// ingest and persist require r.mu. PDU bytes (including sender and service
// center timestamp), never mutable read/unread flags or SIM slot, identify SMS.
func (r *smsReceiver) ingest(raw string, historical bool) error {
	raw = strings.ToUpper(raw)
	id := digest(raw)
	if _, ok := r.state.Parts[id]; ok {
		return nil
	}
	p, err := decodeReceivedPDU(raw)
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	r.state.Parts[id] = storedPart{PDU: raw, FirstSeen: now, Historical: historical}
	count, seq, ref, multi := p.ConcatInfo()
	if multi && (count < 1 || count > 255 || seq < 1 || seq > count) {
		delete(r.state.Parts, id)
		return fmt.Errorf("invalid multipart SMS numbering")
	}
	ids := []string{id}
	parts := []*tpdu.TPDU{p}
	if multi {
		ids = make([]string, count)
		parts = make([]*tpdu.TPDU, count)
		for candidateID, candidate := range r.state.Parts {
			if candidate.MessageID != "" {
				continue
			}
			other, decodeErr := decodeReceivedPDU(candidate.PDU)
			if decodeErr != nil {
				continue
			}
			n, s, reference, isMulti := other.ConcatInfo()
			// The sender may reuse an 8-bit reference. Reject collisions rather
			// than mixing unrelated messages; keep parts for a later complete set.
			if !isMulti || n != count || reference != ref || s < 1 || s > count || other.OA != p.OA || other.DCS != p.DCS || other.PID != p.PID {
				continue
			}
			if delta := other.SCTS.Time.Sub(p.SCTS.Time); delta < -24*time.Hour || delta > 24*time.Hour {
				continue
			}
			if ids[s-1] != "" && ids[s-1] != candidateID {
				return nil
			}
			ids[s-1] = candidateID
			parts[s-1] = other
		}
		for _, part := range parts {
			if part == nil {
				return nil
			}
		}
		if !sms.IsCompleteMessage(parts) {
			return nil
		}
	}
	body, err := sms.Decode(parts)
	if err != nil {
		// Do not poison dedup with a segment that failed message decoding.
		// A later scan must retry it and keep the failure visible; otherwise
		// the initial error disappears while the SMS can never be delivered.
		delete(r.state.Parts, id)
		return fmt.Errorf("SMS text decode failed: %w", err)
	}
	if !utf8.Valid(body) {
		body = []byte("[二进制短信] " + strings.ToUpper(hex.EncodeToString(body)))
	}
	messageID := "sms-" + digest(strings.Join(ids, "|"))
	msg := IncomingSMS{ID: messageID, Number: p.OA.Number(), Text: string(body), ReceivedAt: parts[0].SCTS.Time, StoredAt: now, Historical: true, Parts: len(parts)}
	for _, partID := range ids {
		part := r.state.Parts[partID]
		if !part.Historical {
			msg.Historical = false
		}
		part.MessageID = messageID
		r.state.Parts[partID] = part
	}
	r.state.Messages[messageID] = msg
	return nil
}

func (r *smsReceiver) persist() error {
	b, err := json.MarshalIndent(r.state, "", "  ")
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(r.path), ".sms-state-*")
	if err != nil {
		return err
	}
	name := f.Name()
	defer os.Remove(name)
	if err = f.Chmod(0600); err == nil {
		_, err = f.Write(b)
	}
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err == nil {
		err = closeErr
	}
	if err == nil {
		err = os.Rename(name, r.path)
	}
	if err != nil {
		return fmt.Errorf("save SMS inbox: %w", err)
	}
	return nil
}

func (r *smsReceiver) deliver() error {
	r.mu.Lock()
	// Persist before invoking a callback, even after a previous write failure.
	if err := r.persist(); err != nil {
		r.mu.Unlock()
		return err
	}
	var items []IncomingSMS
	for id, msg := range r.state.Messages {
		if !msg.Historical && !r.state.Delivered[id] {
			items = append(items, msg)
		}
	}
	r.mu.Unlock()
	for _, msg := range items {
		if err := r.callback(msg); err != nil {
			return fmt.Errorf("SMS notification pending: %w", err)
		}
		r.mu.Lock()
		r.state.Delivered[msg.ID] = true
		err := r.persist()
		if err != nil {
			delete(r.state.Delivered, msg.ID)
		}
		r.mu.Unlock()
		if err != nil {
			return err
		}
	}
	return nil
}
