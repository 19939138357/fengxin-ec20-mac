package cscall

import (
	"context"
	"crypto/md5"
	"errors"
	"fmt"
	"github.com/emiago/sipgo/sip"
	"github.com/iniwex5/vohive/internal/sipgw"
	"net"
	"regexp"
	"strings"
	"testing"
	"time"
)

// The cellular side is synthetic: this test never opens a COM port or calls a number.
func TestIncomingCallOverLoopbackSIP(t *testing.T) {
	incomingCallOverLoopbackSIP(t, nil)
}

func TestIncomingCallModemAnswerFailureNotification(t *testing.T) {
	incomingCallOverLoopbackSIP(t, errors.New("synthetic modem answer failure"))
}

func incomingCallOverLoopbackSIP(t *testing.T, answerErr error) {
	t.Helper()
	reserve, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	addr := reserve.LocalAddr().String()
	reserve.Close()
	cfg := sipgw.DefaultConfig()
	cfg.SIP.Listen = addr
	cfg.SIP.ExternalIP = "127.0.0.1"
	cfg.Users = []sipgw.UserConfig{{Username: "1001", Password: "test-password-local", DeviceID: "test"}}
	reg, err := sipgw.NewRegistrar(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err = reg.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer reg.Stop()
	ctrl := &testController{answerErr: answerErr}
	m := NewManagerWithController("test", "synthetic", ctrl, reg)
	defer m.Stop()
	events := make(chan IncomingCallEvent, 4)
	m.SetCallObserver(func(e IncomingCallEvent) error { events <- e; return nil })
	m.audioFactory = func(_, _ string) (*AudioBridge, error) {
		ab, e := NewAudioBridge("synthetic", "test")
		if e == nil {
			ab.startFn = func() error { return nil }
		}
		return ab, e
	}
	reg.SetOnBye(func(_ string, r *sip.Request, _ sip.ServerTransaction) { m.HandleClientBye(r.CallID().Value()) })
	peer, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer peer.Close()
	dst, _ := net.ResolveUDPAddr("udp4", addr)
	local := peer.LocalAddr().String()
	read := func() sip.Message {
		t.Helper()
		peer.SetReadDeadline(time.Now().Add(3 * time.Second))
		b := make([]byte, 8192)
		n, _, e := peer.ReadFromUDP(b)
		if e != nil {
			t.Fatal(e)
		}
		msg, e := sip.ParseMessage(b[:n])
		if e != nil {
			t.Fatal(e)
		}
		return msg
	}
	send := func(wire string) {
		t.Helper()
		if _, e := peer.WriteToUDP([]byte(wire), dst); e != nil {
			t.Fatal(e)
		}
	}
	register := func(seq int, auth string) {
		send(fmt.Sprintf("REGISTER sip:%s SIP/2.0\r\nVia: SIP/2.0/UDP %s;branch=z9hG4bK-reg%d;rport\r\nFrom: <sip:1001@vohive.local>;tag=test\r\nTo: <sip:1001@vohive.local>\r\nCall-ID: incoming-register\r\nCSeq: %d REGISTER\r\nContact: <sip:1001@%s>\r\nExpires: 60\r\n%sContent-Length: 0\r\n\r\n", addr, local, seq, seq, local, auth))
	}
	register(1, "")
	challenge := read().(*sip.Response)
	if challenge.StatusCode != 401 {
		t.Fatal(challenge)
	}
	nonce := regexp.MustCompile(`nonce="([^"]+)"`).FindStringSubmatch(challenge.GetHeader("WWW-Authenticate").Value())[1]
	hash := func(s string) string { return fmt.Sprintf("%x", md5.Sum([]byte(s))) }
	uri := "sip:" + addr
	digest := hash(hash("1001:"+cfg.SIP.Realm+":test-password-local") + ":" + nonce + ":" + hash("REGISTER:"+uri))
	register(2, fmt.Sprintf("Authorization: Digest username=\"1001\", realm=\"%s\", nonce=\"%s\", uri=\"%s\", response=\"%s\"\r\n", cfg.SIP.Realm, nonce, uri, digest))
	if read().(*sip.Response).StatusCode != 200 {
		t.Fatal("registration failed")
	}
	m.onIncoming("test", "123456")
	callReceived := nextCallEvent(t, events)
	if callReceived.Type != "received" || callReceived.Number != "123456" {
		t.Fatalf("missing incoming notification: %+v", callReceived)
	}
	req := read().(*sip.Request)
	if req.Method != sip.INVITE {
		t.Fatal(req)
	}
	if req.Contact().Address.Port != dst.Port {
		t.Fatal("incoming Contact lost SIP listener port")
	}
	body := []byte("v=0\r\nc=IN IP4 127.0.0.1\r\nm=audio 17000 RTP/AVP 0\r\na=rtpmap:0 PCMU/8000\r\n")
	res := sip.NewResponseFromRequest(req, 200, "OK", body)
	res.To().Params.Add("tag", "callee")
	res.AppendHeader(sip.NewHeader("Contact", "<sip:1001@"+local+">"))
	res.AppendHeader(sip.NewHeader("Content-Type", "application/sdp"))
	send(res.String())
	ack := read().(*sip.Request)
	if ack.Method != sip.ACK {
		t.Fatal(ack)
	}
	if answerErr != nil {
		bye := read().(*sip.Request)
		if bye.Method != sip.BYE {
			t.Fatal("modem answer failure did not end SIP dialog")
		}
		callEnded := nextCallEvent(t, events)
		if callEnded.ID != callReceived.ID || callEnded.Type != "ended" || callEnded.Outcome != "failed" || !callEnded.AnsweredAt.IsZero() {
			t.Fatalf("failed modem answer reported as answered: %+v", callEnded)
		}
		return
	}
	deadline := time.Now().Add(time.Second)
	for {
		m.mu.Lock()
		ready := m.state == CallStateConnected && m.currentCall.audio.pcmReady.Load()
		m.mu.Unlock()
		if ready {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("incoming call not answered")
		}
		time.Sleep(time.Millisecond)
	}
	bye := sip.NewRequest(sip.BYE, sip.Uri{User: "123456", Host: "127.0.0.1", Port: dst.Port})
	bye.AppendHeader(sip.NewHeader("Via", "SIP/2.0/UDP "+local+";branch=z9hG4bK-inbound-bye;rport"))
	bye.AppendHeader(&sip.FromHeader{Address: res.To().Address, Params: res.To().Params.Clone()})
	bye.AppendHeader(&sip.ToHeader{Address: req.From().Address, Params: req.From().Params.Clone()})
	sip.CopyHeaders("Call-ID", req, bye)
	bye.AppendHeader(sip.NewHeader("CSeq", "2 BYE"))
	send(bye.String())
	if r := read().(*sip.Response); r.StatusCode != 200 {
		t.Fatal(r)
	}
	for {
		m.mu.Lock()
		idle := m.state == CallStateIdle
		m.mu.Unlock()
		if idle {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("incoming BYE left active call")
		}
		time.Sleep(time.Millisecond)
	}
	if !strings.Contains(ack.To().String(), "callee") {
		t.Fatal("ACK missing callee dialog tag")
	}
	callEnded := nextCallEvent(t, events)
	if callEnded.ID != callReceived.ID || callEnded.Type != "ended" || callEnded.Outcome != "answered" || callEnded.AnsweredAt.IsZero() {
		t.Fatalf("answered SIP call notification incorrect: %+v", callEnded)
	}
}
