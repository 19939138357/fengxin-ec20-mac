package sdp

import (
	"fmt"
	"net"
	"strconv"
	"strings"
)

type Info struct {
	ConnectionIP string
	MediaPort    int
}

// ParseSDP accepts one IPv4 RTP/AVP audio stream with static PCMU (payload 0).
// Unsupported offers are rejected before a modem is dialed.
func ParseSDP(body []byte) (Info, error) {
	var out Info
	inAudio, found, pcmu := false, false, false
	for _, raw := range strings.Split(string(body), "\n") {
		line := strings.TrimSpace(raw)
		if strings.HasPrefix(line, "m=") {
			f := strings.Fields(line[2:])
			inAudio = false
			if len(f) >= 4 && f[0] == "audio" && !found {
				found = true
				inAudio = true
				if f[2] != "RTP/AVP" {
					return out, fmt.Errorf("only RTP/AVP is supported")
				}
				out.MediaPort, _ = strconv.Atoi(f[1])
				for _, p := range f[3:] {
					if p == "0" {
						pcmu = true
					}
				}
			}
		}
		if strings.HasPrefix(line, "c=IN IP4 ") && (!found || inAudio) {
			out.ConnectionIP = strings.TrimSpace(strings.TrimPrefix(line, "c=IN IP4 "))
		}
	}
	ip := net.ParseIP(out.ConnectionIP)
	if ip == nil || ip.To4() == nil || ip.IsUnspecified() || ip.IsMulticast() || out.MediaPort < 1 || out.MediaPort > 65535 || !pcmu {
		return out, fmt.Errorf("valid IPv4 PCMU/8000 SDP required")
	}
	return out, nil
}
