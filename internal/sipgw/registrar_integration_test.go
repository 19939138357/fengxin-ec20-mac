package sipgw

import (
	"context"
	"crypto/md5"
	"fmt"
	"github.com/emiago/sipgo/sip"
	"net"
	"regexp"
	"strings"
	"testing"
	"time"
)

func hash(s string) string { return fmt.Sprintf("%x", md5.Sum([]byte(s))) }
func TestLoopbackRegisterMessageAndForgedSource(t *testing.T) {
	reserve, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	addr := reserve.LocalAddr().String()
	reserve.Close()
	cfg := DefaultConfig()
	cfg.SIP.Listen = addr
	cfg.SIP.ExternalIP = "127.0.0.1"
	cfg.Users = []UserConfig{{Username: "1001", Password: "local-test-password", DeviceID: "test-modem"}}
	r, err := NewRegistrar(cfg)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err = r.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer r.Stop()
	dst, _ := net.ResolveUDPAddr("udp4", addr)
	client, _ := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	defer client.Close()
	forger, _ := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	defer forger.Close()
	seq := 0
	request := func(conn *net.UDPConn, method, target, extra, body string) string {
		seq++
		local := conn.LocalAddr().String()
		return fmt.Sprintf("%s sip:%s SIP/2.0\r\nVia: SIP/2.0/UDP %s;branch=z9hG4bK-test-%d;rport\r\nMax-Forwards: 70\r\nFrom: <sip:1001@vohive.local>;tag=test\r\nTo: <sip:%s>\r\nCall-ID: local-test-%d\r\nCSeq: %d %s\r\nContact: <sip:1001@%s>\r\n%sContent-Length: %d\r\n\r\n%s", method, target, local, seq, target, seq, seq, method, local, extra, len(body), body)
	}
	exchange := func(conn *net.UDPConn, wire string) string {
		t.Helper()
		conn.WriteToUDP([]byte(wire), dst)
		buf := make([]byte, 16384)
		conn.SetReadDeadline(time.Now().Add(2 * time.Second))
		for {
			n, _, err := conn.ReadFromUDP(buf)
			if err != nil {
				t.Fatal(err)
			}
			s := string(buf[:n])
			if !strings.HasPrefix(s, "SIP/2.0 100") {
				return s
			}
		}
	}
	// Readiness is a real TCP listener check, rather than a fixed startup sleep.
	deadline := time.Now().Add(3 * time.Second)
	for {
		c, e := net.DialTimeout("tcp", addr, 100*time.Millisecond)
		if e == nil {
			c.Close()
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("SIP listener unavailable")
		}
		time.Sleep(10 * time.Millisecond)
	}
	response := exchange(client, request(client, "REGISTER", addr, "Expires: 60\r\n", ""))
	if !strings.HasPrefix(response, "SIP/2.0 401") {
		t.Fatalf("expected digest challenge: %s", response)
	}
	nonce := regexp.MustCompile(`nonce="([^"]+)"`).FindStringSubmatch(response)
	if len(nonce) != 2 {
		t.Fatal("missing nonce")
	}
	uri := "sip:" + addr
	digest := hash(hash("1001:"+cfg.SIP.Realm+":local-test-password") + ":" + nonce[1] + ":" + hash("REGISTER:"+uri))
	authorization := fmt.Sprintf("Authorization: Digest username=\"1001\", realm=\"%s\", nonce=\"%s\", uri=\"%s\", response=\"%s\"\r\nExpires: 60\r\n", cfg.SIP.Realm, nonce[1], uri, digest)
	response = exchange(client, request(client, "REGISTER", addr, authorization, ""))
	if !strings.HasPrefix(response, "SIP/2.0 200") || r.CountRegistered() != 1 {
		t.Fatalf("register failed: %s", response)
	}
	submitted := make(chan string, 2)
	r.SetOnMessage(func(device, number, text string) error { submitted <- device + ":" + number + ":" + text; return nil })
	response = exchange(client, request(client, "MESSAGE", "123456@"+addr, "Content-Type: text/plain\r\n", "synthetic SMS"))
	if !strings.HasPrefix(response, "SIP/2.0 200") {
		t.Fatalf("MESSAGE failed: %s", response)
	}
	select {
	case got := <-submitted:
		if got != "test-modem:123456:synthetic SMS" {
			t.Fatal(got)
		}
	case <-time.After(time.Second):
		t.Fatal("SMS callback missing")
	}
	response = exchange(forger, request(forger, "MESSAGE", "123456@"+addr, "Content-Type: text/plain\r\n", "forged"))
	if !strings.HasPrefix(response, "SIP/2.0 403") {
		t.Fatalf("forged identity accepted: %s", response)
	}
	r.SetOnInvite(func(_ string, req *sip.Request, tx sip.ServerTransaction) {
		tx.Respond(sip.NewResponseFromRequest(req, 503, "No Cellular Registration", nil))
	})
	response = exchange(client, request(client, "INVITE", "123456@"+addr, "Content-Type: application/sdp\r\n", "v=0\r\n"))
	if !strings.HasPrefix(response, "SIP/2.0 503") {
		t.Fatalf("INVITE callback failed: %s", response)
	}
	select {
	case <-submitted:
		t.Fatal("forged MESSAGE reached modem callback")
	default:
	}
}
