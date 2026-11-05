package winmodem

import (
	"fmt"
	"regexp"
	"strings"
	"time"
)

var mbnListEntry = regexp.MustCompile(`(?i)\+QMBNCFG:\s*"List",\d+,([01]),([01]),"([A-Za-z0-9_.-]+)"`)
var mbnAutoEntry = regexp.MustCompile(`(?i)\+QMBNCFG:\s*"AutoSel",([01])`)

// Fixed, preinstalled profiles only. RunNetwork already holds the traffic and
// serial locks and protects any active voice call. Reboot is an explicit action.
func (m *Modem) selectUnicomMBNLocked(action string) (out NetworkResult, err error) {
	query := func(cmd string) (string, error) {
		response, e := m.commandLocked(cmd+"\r", 5*time.Second, false)
		out.Log = append(out.Log, cmd, redactNetworkDiagnostic(cmd, response))
		return response, e
	}
	raw, e := query("AT+CIMI")
	if e != nil {
		return out, e
	}
	if !regexp.MustCompile(`(?m)^46001[0-9]{9,11}\s*$`).MatchString(raw) {
		return out, fmt.Errorf("this controlled comparison requires the verified 46001 SIM")
	}
	raw, e = query(`AT+QMBNCFG="AutoSel"`)
	if e != nil {
		return out, e
	}
	auto := mbnAutoEntry.FindStringSubmatch(raw)
	if auto == nil {
		return out, fmt.Errorf("cannot read original MBN AutoSel")
	}
	raw, e = query(`AT+QMBNCFG="List"`)
	if e != nil {
		return out, e
	}
	target, targetAuto := "ROW_Generic_3GPP", "0"
	if action == "mbn-unicom" {
		target, targetAuto = "CU-VoLTE", "1"
	}
	original := ""
	found := false
	for _, entry := range mbnListEntry.FindAllStringSubmatch(raw, -1) {
		if entry[1] == "1" {
			if original != "" {
				return out, fmt.Errorf("multiple selected profiles")
			}
			original = entry[3]
		}
		if entry[3] == target {
			found = true
		}
	}
	if !found || original == "" {
		return out, fmt.Errorf("target or original MBN not present")
	}
	if original != "CU-VoLTE" && original != "ROW_Generic_3GPP" {
		return out, fmt.Errorf("unexpected original profile %s", original)
	}
	if original == target && auto[1] == targetAuto {
		out.Log = append(out.Log, "Target profile already selected; inspect active state after reboot")
		return
	}
	// On any pre-reboot failure restore the selected profile and automatic policy.
	// No MBN is uploaded or deleted. Logs contain only the profile names and flags.
	defer func() {
		if err != nil {
			for _, cmd := range []string{`AT+QMBNCFG="AutoSel",0`, `AT+QMBNCFG="Deactivate"`, fmt.Sprintf(`AT+QMBNCFG="Select","%s"`, original), `AT+QMBNCFG="AutoSel",` + auto[1]} {
				if _, restoreErr := query(cmd); restoreErr != nil {
					err = fmt.Errorf("%v; rollback failed: %w", err, restoreErr)
					return
				}
			}
			out.Log = append(out.Log, "Original selection restored; reboot still needed to activate it")
		}
	}()
	for _, cmd := range []string{`AT+QMBNCFG="AutoSel",0`, `AT+QMBNCFG="Deactivate"`, fmt.Sprintf(`AT+QMBNCFG="Select","%s"`, target), `AT+QMBNCFG="AutoSel",` + targetAuto} {
		if _, err = query(cmd); err != nil {
			return
		}
	}
	raw, err = query(`AT+QMBNCFG="List"`)
	if err != nil {
		return
	}
	selected := false
	for _, entry := range mbnListEntry.FindAllStringSubmatch(raw, -1) {
		if entry[1] == "1" && entry[3] == target {
			selected = true
		}
	}
	if !selected {
		return out, fmt.Errorf("target selection not verified")
	}
	raw, err = query(`AT+QMBNCFG="AutoSel"`)
	if err != nil {
		return
	}
	got := mbnAutoEntry.FindStringSubmatch(raw)
	if got == nil || strings.TrimSpace(got[1]) != targetAuto {
		return out, fmt.Errorf("MBN AutoSel verification failed")
	}
	out.Log = append(out.Log, "Selected profile verified; modem reboot required before evaluating registration")
	return
}
