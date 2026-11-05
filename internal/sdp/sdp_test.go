package sdp

import "testing"

func TestSDPNegotiation(t *testing.T) {
	good := []byte("v=0\r\nc=IN IP4 127.0.0.1\r\nm=audio 18000 RTP/AVP 0 101\r\n")
	got, err := ParseSDP(good)
	if err != nil || got.MediaPort != 18000 {
		t.Fatalf("%+v %v", got, err)
	}
	for _, bad := range []string{"v=0\nc=IN IP4 127.0.0.1\nm=audio 18000 RTP/AVP 8", "v=0\nc=IN IP4 0.0.0.0\nm=audio 18000 RTP/AVP 0", "v=0\nc=IN IP4 127.0.0.1\nm=audio 0 RTP/AVP 0", "v=0\nc=IN IP4 127.0.0.1\nm=audio 18000 RTP/SAVP 0"} {
		if _, err := ParseSDP([]byte(bad)); err == nil {
			t.Fatalf("accepted invalid offer %q", bad)
		}
	}
}
