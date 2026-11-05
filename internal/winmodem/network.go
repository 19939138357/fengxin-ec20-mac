package winmodem

import (
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
)

type NetworkRequest struct {
	Action   string `json:"action"`
	PLMN     string `json:"plmn"`
	RAT      int    `json:"rat"`
	Fallback bool   `json:"fallback"`
}
type NetworkOperator struct {
	PLMN   string `json:"plmn"`
	Name   string `json:"name"`
	Status int    `json:"status"`
	RAT    int    `json:"rat"`
}
type NetworkResult struct {
	Operators []NetworkOperator `json:"operators"`
	Log       []string          `json:"log"`
}

var plmnPattern = regexp.MustCompile(`^[0-9]{5,6}$`)
var operatorTuple = regexp.MustCompile(`\(([0-3]),\s*"([^"]*)",\s*"([^"]*)",\s*"([0-9]{5,6})"(?:,\s*([0-9]+))?\)`)

func (r NetworkRequest) Command() (string, error) {
	switch r.Action {
	case "diagnose", "sms-diagnose", "radio-diagnose", "config-diagnose", "rf-diagnose":
		return "diagnose", nil
	case "scan":
		return "AT+COPS=?", nil
	case "auto":
		return "AT+COPS=0", nil
	case "lte-only":
		return `AT+QCFG="nwscanmode",3,1`, nil
	case "wcdma-only":
		return `AT+QCFG="nwscanmode",2,1`, nil
	case "radio-auto":
		return `AT+QCFG="nwscanmode",0,1`, nil
	case "b8-only":
		// Zero leaves the GSM/WCDMA and TD-SCDMA masks unchanged.
		return `AT+QCFG="band",0,80,0,1`, nil
	case "b1-b3-b8":
		return `AT+QCFG="band",0,85,0,1`, nil
	case "sim-reinitialize":
		return "AT+CFUN=0", nil
	case "b8-test", "b3-test", "b1-test":
		return r.Action, nil
	case "mbn-generic", "mbn-unicom", "modem-reboot":
		return r.Action, nil
	case "manual":
		if !plmnPattern.MatchString(r.PLMN) {
			return "", fmt.Errorf("运营商代码必须是5或6位数字")
		}
		if r.RAT != -1 && r.RAT != 0 && r.RAT != 2 && r.RAT != 7 && r.RAT != 100 {
			return "", fmt.Errorf("不支持的网络制式")
		}
		mode := 1
		if r.Fallback {
			mode = 4
		}
		cmd := fmt.Sprintf("AT+COPS=%d,2,%q", mode, r.PLMN)
		if r.RAT != -1 {
			cmd += fmt.Sprintf(",%d", r.RAT)
		}
		return cmd, nil
	default:
		return "", fmt.Errorf("未知选网操作")
	}
}
func parseOperators(raw string) []NetworkOperator {
	out := []NetworkOperator{}
	seen := map[string]bool{}
	for _, m := range operatorTuple.FindAllStringSubmatch(raw, -1) {
		stat, _ := strconv.Atoi(m[1])
		rat := -1
		if m[5] != "" {
			rat, _ = strconv.Atoi(m[5])
		}
		key := m[4] + ":" + strconv.Itoa(rat)
		if seen[key] {
			continue
		}
		seen[key] = true
		name := m[2]
		if name == "" {
			name = m[3]
		}
		if name == "" {
			name = m[4]
		}
		out = append(out, NetworkOperator{PLMN: m[4], Name: name, Status: stat, RAT: rat})
	}
	return out
}

// Long network commands share the gateway's existing serial transaction lock.
// No second serial connection, automatic scan, call, or SMS is created here.
func (m *Modem) RunNetwork(r NetworkRequest) (NetworkResult, error) {
	out := NetworkResult{Operators: []NetworkOperator{}, Log: []string{}}
	cmd, err := r.Command()
	if err != nil {
		return out, err
	}
	if !m.trafficMu.TryLock() {
		return out, fmt.Errorf("模组正在处理电话、短信或选网操作，请稍后重试")
	}
	defer m.trafficMu.Unlock()
	m.commandMu.Lock()
	defer m.commandMu.Unlock()
	if r.Action == "diagnose" || r.Action == "sms-diagnose" || r.Action == "radio-diagnose" || r.Action == "config-diagnose" || r.Action == "rf-diagnose" {
		queries := []string{"AT+CIMI", "AT+CLCC", "AT+QNWINFO", "AT+QENG=\"servingcell\"", "AT+QCFG=\"ims\"", "AT+QMBNCFG=\"List\""}
		if r.Action == "sms-diagnose" {
			queries = []string{"AT+CSCA?", "AT+CGSMS?", "AT+CNMI?", "AT+CMGF?", "AT+CSMS?", "AT+CPMS?"}
		}
		if r.Action == "radio-diagnose" {
			queries = []string{"AT+CEER", "AT+QCFG=\"nwscanmode\"", "AT+QCFG=\"band\"", "AT+QCFG=\"roamservice\"", "AT+QCFG=\"servicedomain\"", "AT+QNWLOCK=\"common/lte\"", "AT+QCFG=\"nwscanseq\""}
		}
		if r.Action == "config-diagnose" {
			queries = []string{`AT+QMBNCFG="AutoSel"`, `AT+QMBNCFG="List"`, "AT+CGDCONT?", "AT+CGATT?", `AT+QCFG="ims"`, `AT+QFPLMNCFG="List"`}
		}
		if r.Action == "rf-diagnose" {
			// Fixed read-only queries; no arbitrary AT or RF test writes.
			queries = []string{"AT+QCSQ", `AT+QENG="neighbourcell"`, "AT+QSCLK?", "AT+QRFTESTMODE?", `AT+QCFG="airplanecontrol"`, `AT+QCFG="airplane"`, `AT+QCFG="lte/divctl"`, "AT+QTEMP", `AT+QCFG="usbcfg"`}
		}
		var diagnosticErr error
		for _, query := range queries {
			response, queryErr := m.commandLocked(query+"\r", 5*time.Second, false)
			out.Log = append(out.Log, query, redactNetworkDiagnostic(query, response))
			if queryErr != nil {
				// Firmware may reject an optional query. Its explicit terminal
				// error is recorded; a timeout/disconnection always stops here.
				if r.Action == "rf-diagnose" && strings.HasPrefix(queryErr.Error(), "modem rejected command:") {
					diagnosticErr = errors.Join(diagnosticErr, fmt.Errorf("%s: %w", query, queryErr))
					continue
				}
				return out, queryErr
			}
		}
		return out, diagnosticErr
	}
	calls, err := m.commandLocked("AT+CLCC\r", 5*time.Second, false)
	if err != nil {
		return out, err
	}
	if hasNonDataCall(calls) {
		return out, fmt.Errorf("存在通话，请结束通话后再选网")
	}
	if r.Action == "mbn-generic" || r.Action == "mbn-unicom" {
		return m.selectUnicomMBNLocked(r.Action)
	}
	if r.Action == "modem-reboot" {
		response, err := m.commandLocked("AT+CFUN=1,1\r", 15*time.Second, false)
		out.Log = append(out.Log, "AT+CFUN=1,1", redactNetworkDiagnostic("", response), "Module reboot requested; reopen the gateway after USB enumeration")
		m.Close()
		return out, err
	}
	if r.Action == "b8-test" {
		return m.runB8TestLocked(12, 5*time.Second)
	}
	if r.Action == "b3-test" {
		return m.runBandTestLocked(3, 24, 5*time.Second, true)
	}
	if r.Action == "b1-test" {
		return m.runBandTestLocked(1, 24, 5*time.Second, true)
	}
	if r.Action == "sim-reinitialize" {
		response, offErr := m.commandLocked("AT+CFUN=0\r", 15*time.Second, false)
		out.Log = append(out.Log, "AT+CFUN=0", redactNetworkDiagnostic("", response))
		if offErr != nil {
			return out, offErr
		}
		time.Sleep(time.Second)
		response, onErr := m.commandLocked("AT+CFUN=1\r", 15*time.Second, false)
		out.Log = append(out.Log, "AT+CFUN=1", redactNetworkDiagnostic("", response))
		return out, onErr
	}
	response, err := m.commandLocked(cmd+"\r", 185*time.Second, false)
	// Keep only the response to this operation; never include unrelated SMS URCs.
	safe := []string{}
	for _, line := range strings.Split(response, "\n") {
		if strings.HasPrefix(line, "+COPS:") || strings.HasPrefix(line, "+CME ERROR:") || strings.HasPrefix(line, "+CMS ERROR:") || line == "OK" || line == "ERROR" {
			safe = append(safe, line)
		}
	}
	out.Log = append(out.Log, cmd, strings.Join(safe, "\n"))
	if err != nil {
		return out, err
	}
	if r.Action == "scan" {
		out.Operators = parseOperators(response)
	}
	return out, nil
}

func redactNetworkDiagnostic(query, response string) string {
	var safe []string
	for _, line := range strings.Split(response, "\n") {
		line = strings.TrimSpace(line)
		if query == "AT+CIMI" && regexp.MustCompile(`^[0-9]{14,16}$`).MatchString(line) {
			safe = append(safe, "IMSI prefix: "+line[:5]+" (remaining digits redacted)")
		} else if strings.HasPrefix(line, "+QCSQ:") || strings.HasPrefix(line, "+QSCLK:") || strings.HasPrefix(line, "+QRFTESTMODE:") || strings.HasPrefix(line, "+QTEMP:") {
			safe = append(safe, line)
		} else if strings.HasPrefix(line, "+CLCC:") {
			fields := strings.Split(line, ",")
			if len(fields) >= 5 {
				safe = append(safe, strings.Join(fields[:5], ","))
			}
		} else if strings.HasPrefix(line, "+QFPLMNCFG:") || strings.HasPrefix(line, "+CGDCONT:") || strings.HasPrefix(line, "+CGATT:") || strings.HasPrefix(line, "+CEREG:") || strings.HasPrefix(line, "+CPIN:") || strings.HasPrefix(line, "+CEER:") || strings.HasPrefix(line, "+QNWLOCK:") || strings.HasPrefix(line, "+CSCA:") || strings.HasPrefix(line, "+CGSMS:") || strings.HasPrefix(line, "+CNMI:") || strings.HasPrefix(line, "+CMGF:") || strings.HasPrefix(line, "+CSMS:") || strings.HasPrefix(line, "+CPMS:") || strings.HasPrefix(line, "+QNWINFO:") || strings.HasPrefix(line, "+QENG:") || strings.HasPrefix(line, "+QCFG:") || strings.HasPrefix(line, "+QMBNCFG:") || strings.HasPrefix(line, "+CME ERROR:") || line == "OK" || line == "ERROR" {
			safe = append(safe, line)
		}
	}
	return strings.Join(safe, "\n")
}

// LTE data connections also appear in CLCC. Keep voice, fax and unknown
// entries protected, while allowing an explicit network selection with data.
func hasNonDataCall(response string) bool {
	for _, line := range strings.Split(response, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "+CLCC:") {
			continue
		}
		fields := strings.Split(strings.TrimPrefix(line, "+CLCC:"), ",")
		if len(fields) < 5 || strings.TrimSpace(fields[3]) != "1" {
			return true
		}
	}
	return false
}
