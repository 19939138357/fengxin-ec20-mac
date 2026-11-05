package winmodem

import (
	"fmt"
	"strings"
	"testing"
)

func TestRequestedBandsRestoreUnlessRegistrationAndBandConfirmed(t *testing.T) {
	for _, band := range []int{3, 1} {
		for _, scenario := range []string{"search", "registered", "setting-error", "readback-mismatch", "observation-error", "network-mismatch"} {
			t.Run(fmt.Sprintf("B%d/%s", band, scenario), func(t *testing.T) {
				m, f := testModem()
				mask := uint64(1) << (band - 1)
				narrow, restored := false, false
				f.answer = func(raw string) {
					cmd := strings.TrimSpace(raw)
					switch cmd {
					case "AT+CPIN?":
						m.onLine("+CPIN: READY")
					case `AT+QCFG="band"`:
						if narrow {
							gsm := "493"
							if scenario == "readback-mismatch" {
								gsm = "1"
							}
							m.onLine(fmt.Sprintf(`+QCFG: "band",0x%s,0x%x,0x21`, gsm, mask))
						} else {
							m.onLine(`+QCFG: "band",0x493,0x1e200000095,0x21`)
						}
					case fmt.Sprintf(`AT+QCFG="band",493,%x,21,1`, mask):
						narrow = true
						if scenario == "setting-error" {
							m.onLine("ERROR")
							return
						}
					case `AT+QCFG="band",493,1e200000095,21,1`:
						narrow, restored = false, true
					case `AT+QENG="servingcell"`:
						if scenario == "observation-error" {
							m.onLine("ERROR")
							return
						}
						m.onLine(`+QENG: "servingcell","SEARCH"`)
					case "AT+CEREG?":
						if scenario == "registered" || scenario == "network-mismatch" {
							m.onLine("+CEREG: 0,1")
						} else {
							m.onLine("+CEREG: 0,2")
						}
					case "AT+QNWINFO":
						actualBand := band
						if scenario == "network-mismatch" {
							actualBand = 8
						}
						m.onLine(fmt.Sprintf(`+QNWINFO: "FDD LTE","46001","LTE BAND %d",1300`, actualBand))
					default:
						t.Errorf("unexpected command %q", cmd)
					}
					m.onLine("OK")
				}
				result, err := m.runBandTestLocked(band, 1, 0, true)
				wantError := scenario != "search" && scenario != "registered"
				if (err != nil) != wantError {
					t.Fatalf("error = %v", err)
				}
				if scenario == "registered" {
					if !narrow || restored || !strings.Contains(strings.Join(result.Log, "\n"), "configuration retained") {
						t.Fatalf("registered band not retained: %+v", result)
					}
				} else if narrow || !restored || !strings.Contains(strings.Join(result.Log, "\n"), "restored and verified") {
					t.Fatalf("original not restored: %+v", result)
				}
			})
		}
	}
}

func TestB8ProbeRestoresOriginalOnSuccessAndError(t *testing.T) {
	for _, fail := range []string{"", "setting", "observation"} {
		t.Run(fail, func(t *testing.T) {
			m, f := testModem()
			narrow := false
			restored := false
			f.answer = func(raw string) {
				cmd := strings.TrimSpace(raw)
				switch cmd {
				case "AT+CPIN?":
					m.onLine("+CPIN: READY")
				case `AT+QCFG="band"`:
					if narrow {
						m.onLine(`+QCFG: "band",0x493,0x80,0x21`)
					} else {
						m.onLine(`+QCFG: "band",0x493,0x1e200000095,0x21`)
					}
				case `AT+QCFG="band",493,80,21,1`:
					if fail == "setting" {
						m.onLine("ERROR")
						return
					}
					narrow = true
				case `AT+QCFG="band",493,1e200000095,21,1`:
					narrow = false
					restored = true
				case `AT+QENG="servingcell"`:
					if fail == "observation" {
						m.onLine("ERROR")
						return
					}
					m.onLine(`+QENG: "servingcell","SEARCH"`)
				case "AT+CEREG?":
					m.onLine("+CEREG: 0,2")
				default:
					t.Errorf("unexpected command %q", cmd)
				}
				m.onLine("OK")
			}
			result, err := m.runB8TestLocked(1, 0)
			if (err != nil) != (fail != "") {
				t.Fatalf("unexpected error %v", err)
			}
			if narrow || !restored || !strings.Contains(strings.Join(result.Log, "\n"), "restored and verified") {
				t.Fatalf("did not restore: %+v", result)
			}
		})
	}
}

func TestB8ProbeRefusesUnreadableOriginal(t *testing.T) {
	m, f := testModem()
	f.answer = func(cmd string) {
		if strings.TrimSpace(cmd) == "AT+CPIN?" {
			m.onLine("+CPIN: READY")
		}
		m.onLine("OK")
	}
	if _, err := m.runB8TestLocked(1, 0); err == nil {
		t.Fatal("accepted missing original")
	}
	if len(f.writes) != 2 {
		t.Fatal("changed configuration without original")
	}
}
