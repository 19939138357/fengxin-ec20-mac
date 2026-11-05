package winmodem

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
)

var bandReply = regexp.MustCompile(`(?i)\+QCFG:\s*"band",\s*(0x[0-9a-f]+),\s*(0x[0-9a-f]+),\s*(0x[0-9a-f]+)`)

func parseBandReply(s string) ([3]uint64, error) {
	var values [3]uint64
	match := bandReply.FindStringSubmatch(s)
	if match == nil {
		return values, fmt.Errorf("cannot safely parse original band configuration")
	}
	for i := range values {
		n, err := strconv.ParseUint(match[i+1][2:], 16, 64)
		if err != nil {
			return values, err
		}
		values[i] = n
	}
	return values, nil
}

// Caller owns trafficMu and commandMu, and has checked CLCC. The original
// configuration is restored even when setting, observation or verification fails.
func (m *Modem) runB8TestLocked(samples int, interval time.Duration) (out NetworkResult, err error) {
	return m.runBandTestLocked(8, samples, interval, false)
}

// A successful explicitly requested B1/B3 selection can remain active. All
// unsuccessful paths restore and verify the complete original band tuple.
func (m *Modem) runBandTestLocked(band int, samples int, interval time.Duration, keepRegistered bool) (out NetworkResult, err error) {
	if band != 1 && band != 3 && band != 8 {
		return out, fmt.Errorf("unsupported band selection")
	}
	mask := uint64(1) << (band - 1)
	retained := false
	query := func(cmd string) (string, error) {
		response, e := m.commandLocked(cmd+"\r", 5*time.Second, false)
		out.Log = append(out.Log, cmd, redactNetworkDiagnostic(cmd, response))
		return response, e
	}
	if m.Snapshot().Registered {
		return out, fmt.Errorf("already registered; refusing disruptive band test")
	}
	ready, e := query("AT+CPIN?")
	if e != nil {
		return out, e
	}
	if !strings.Contains(ready, "+CPIN: READY") {
		return out, fmt.Errorf("SIM is not ready")
	}
	raw, e := query(`AT+QCFG="band"`)
	if e != nil {
		return out, e
	}
	original, e := parseBandReply(raw)
	if e != nil {
		return out, e
	}
	if original[1]&mask == 0 {
		return out, fmt.Errorf("B%d is not in the original allowed bands", band)
	}
	restore := fmt.Sprintf(`AT+QCFG="band",%x,%x,%x,1`, original[0], original[1], original[2])
	defer func() {
		if retained {
			out.Log = append(out.Log, fmt.Sprintf("Registered B%d configuration retained; original tuple: %x,%x,%x", band, original[0], original[1], original[2]))
			return
		}
		if _, e := query(restore); e != nil {
			err = fmt.Errorf("%v; restore failed: %w; original bands: %x", err, e, original)
			return
		}
		raw, e := query(`AT+QCFG="band"`)
		if e != nil {
			err = fmt.Errorf("restore verification failed: %w", e)
			return
		}
		got, e := parseBandReply(raw)
		if e != nil || got != original {
			err = fmt.Errorf("original bands not verified after restore: %v", e)
			return
		}
		out.Log = append(out.Log, "Original band configuration restored and verified")
	}()
	if _, err = query(fmt.Sprintf(`AT+QCFG="band",%x,%x,%x,1`, original[0], mask, original[2])); err != nil {
		return
	}
	raw, err = query(`AT+QCFG="band"`)
	if err != nil {
		return
	}
	current, e := parseBandReply(raw)
	expected := original
	expected[1] = mask
	if e != nil || current != expected {
		return out, fmt.Errorf("B%d-only setting not verified", band)
	}
	for i := 0; i < samples; i++ {
		time.Sleep(interval)
		if _, err = query(`AT+QENG="servingcell"`); err != nil {
			return
		}
		var registration string
		registration, err = query("AT+CEREG?")
		if err != nil {
			return
		}
		if regexp.MustCompile(`\+CEREG:\s*\d+,\s*[15](?:\s|,|$)`).MatchString(registration) {
			out.Log = append(out.Log, fmt.Sprintf("LTE registered during B%d test", band))
			if keepRegistered {
				var network string
				network, err = query("AT+QNWINFO")
				if err != nil {
					return
				}
				if !strings.Contains(network, fmt.Sprintf(`"LTE BAND %d"`, band)) {
					return out, fmt.Errorf("registered network did not confirm B%d", band)
				}
				retained = true
			}
			return
		}
	}
	out.Log = append(out.Log, fmt.Sprintf("No LTE registration during bounded B%d test", band))
	return
}
