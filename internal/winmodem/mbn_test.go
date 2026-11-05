package winmodem

import (
	"strings"
	"testing"
)

func TestMBNSelectionAndRollback(t *testing.T) {
	for _, fail := range []bool{false, true} {
		m, f := testModem()
		selected := "CU-VoLTE"
		auto := "1"
		failed := false
		f.answer = func(raw string) {
			cmd := strings.TrimSpace(raw)
			switch cmd {
			case "AT+CIMI":
				m.onLine("460011234567890")
			case `AT+QMBNCFG="AutoSel"`:
				m.onLine(`+QMBNCFG: "AutoSel",` + auto)
			case `AT+QMBNCFG="List"`:
				for i, name := range []string{"ROW_Generic_3GPP", "CU-VoLTE"} {
					flag := "0"
					if name == selected {
						flag = "1"
					}
					m.onLine(`+QMBNCFG: "List",` + string(rune('0'+i)) + `,` + flag + `,0,"` + name + `",0x0001,20220101`)
				}
			case `AT+QMBNCFG="AutoSel",0`:
				auto = "0"
			case `AT+QMBNCFG="AutoSel",1`:
				auto = "1"
			case `AT+QMBNCFG="Deactivate"`:
				selected = ""
			case `AT+QMBNCFG="Select","ROW_Generic_3GPP"`:
				if fail && !failed {
					failed = true
					m.onLine("ERROR")
					return
				}
				selected = "ROW_Generic_3GPP"
			case `AT+QMBNCFG="Select","CU-VoLTE"`:
				selected = "CU-VoLTE"
			default:
				t.Errorf("unexpected command %q", cmd)
			}
			m.onLine("OK")
		}
		result, err := m.selectUnicomMBNLocked("mbn-generic")
		if fail {
			if err == nil || selected != "CU-VoLTE" || auto != "1" {
				t.Fatalf("rollback failed: %v %s %s", err, selected, auto)
			}
		} else {
			if err != nil || selected != "ROW_Generic_3GPP" || auto != "0" {
				t.Fatalf("selection failed: %v", err)
			}
		}
		if strings.Contains(strings.Join(result.Log, "\n"), "1234567890") {
			t.Fatal("identity leaked")
		}
	}
}

func TestMBNRejectsDifferentSIMBeforeMutation(t *testing.T) {
	m, f := testModem()
	f.answer = func(raw string) { m.onLine("460111234567890"); m.onLine("OK") }
	if _, err := m.selectUnicomMBNLocked("mbn-generic"); err == nil || len(f.writes) != 1 {
		t.Fatal("mutated another SIM")
	}
}
