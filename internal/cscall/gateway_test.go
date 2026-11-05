package cscall

import (
	"bytes"
	"context"
	"fmt"
	"github.com/emiago/sipgo/sip"
	"github.com/iniwex5/vohive/internal/sipgw"
	"io"
	"net"
	"testing"
	"time"
)

type testController struct {
	dialed, answered, hung int
	answerErr              error
}

func (f *testController) Start(context.Context) error { return nil }
func (f *testController) Stop()                       {}
func (f *testController) Dial(context.Context, string) (CallRef, error) {
	f.dialed++
	return CallRef{ID: "test"}, nil
}
func (f *testController) Answer(context.Context, string) error                { f.answered++; return f.answerErr }
func (f *testController) Hangup(context.Context, string, HangupOptions) error { f.hung++; return nil }
func (f *testController) GetCalls(context.Context) ([]CallInfo, error) {
	return []CallInfo{{ID: "test", State: CallStateConnected}}, nil
}
func (f *testController) Events() <-chan Event  { return nil }
func (f *testController) PCMReady() <-chan bool { return nil }

type responseRecorder struct {
	sip.ServerTransaction
	responses []*sip.Response
}

func (t *responseRecorder) Respond(r *sip.Response) error {
	t.responses = append(t.responses, r)
	return nil
}
func invite(t *testing.T, codec string) *sip.Request {
	t.Helper()
	body := "v=0\r\nc=IN IP4 127.0.0.1\r\nm=audio 17000 RTP/AVP " + codec + "\r\n"
	raw := fmt.Sprintf("INVITE sip:123456@127.0.0.1:5068 SIP/2.0\r\nVia: SIP/2.0/UDP 127.0.0.1:16000;branch=z9hG4bK-test\r\nFrom: <sip:1001@vohive.local>;tag=client\r\nTo: <sip:123456@127.0.0.1>\r\nCall-ID: test-call\r\nCSeq: 1 INVITE\r\nContact: <sip:1001@127.0.0.1:16000>\r\nContent-Type: application/sdp\r\nContent-Length: %d\r\n\r\n%s", len(body), body)
	msg, err := sip.ParseMessage([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	r := msg.(*sip.Request)
	r.SetSource("127.0.0.1:16000")
	r.SetTransport("UDP")
	return r
}
func TestSIPCallUsesControllerAndCorrectDialog(t *testing.T) {
	cfg := sipgw.DefaultConfig()
	cfg.SIP.Listen = "127.0.0.1:5068"
	cfg.SIP.ExternalIP = "127.0.0.1"
	reg, _ := sipgw.NewRegistrar(cfg)
	defer reg.Stop()
	ctrl := &testController{}
	m := NewManagerWithController("test", "synthetic", ctrl, reg)
	defer m.Stop()
	m.audioFactory = func(_, _ string) (*AudioBridge, error) {
		ab, err := NewAudioBridge("synthetic", "test")
		if err == nil {
			ab.startFn = func() error { return nil }
		}
		return ab, err
	}
	rejected := &responseRecorder{}
	m.HandleOutboundInvite("test", invite(t, "8"), rejected)
	if ctrl.dialed != 0 || rejected.responses[0].StatusCode != 488 {
		t.Fatal("unsupported codec reached modem")
	}
	recorder := &responseRecorder{}
	req := invite(t, "0")
	m.HandleOutboundInvite("test", req, recorder)
	if ctrl.dialed != 1 || m.state != CallStateConnected || len(recorder.responses) != 2 || recorder.responses[1].StatusCode != 200 {
		t.Fatal("outbound signaling failed")
	}
	if recorder.responses[1].Contact().Address.Port != 5068 {
		t.Fatal("wrong advertised SIP Contact port")
	}
	bye := dialogBye(m.currentCall)
	if bye.From().Address.User != "123456" || bye.To().Address.User != "1001" || bye.Recipient.Port != 16000 || bye.Destination() != "127.0.0.1:16000" {
		t.Fatalf("wrong BYE dialog direction: %s", bye)
	}
	if !m.HandleClientBye("test-call") || ctrl.hung != 1 || m.state != CallStateIdle {
		t.Fatal("BYE did not release modem")
	}
}

type pcmSink struct{ chunks chan []byte }

func (s *pcmSink) Write(p []byte) (int, error) {
	s.chunks <- append([]byte(nil), p...)
	return len(p), nil
}
func (s *pcmSink) Close() error { return nil }
func TestBidirectionalRTPPCMData(t *testing.T) {
	peer, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer peer.Close()
	ab, err := NewAudioBridge("synthetic", "test")
	if err != nil {
		t.Fatal(err)
	}
	defer ab.Stop()
	pr, pw := io.Pipe()
	defer pw.Close()
	sink := &pcmSink{chunks: make(chan []byte, 2)}
	ab.captureOut = pr
	ab.playbackIn = sink
	ab.SetClientAddr("127.0.0.1", peer.LocalAddr().(*net.UDPAddr).Port)
	ab.SetPCMReady(true)
	ab.wg.Add(2)
	go ab.loopCapture()
	go ab.loopPlayback()
	pcm := bytes.Repeat([]byte{0, 0}, 160)
	go pw.Write(pcm)
	peer.SetReadDeadline(time.Now().Add(time.Second))
	buffer := make([]byte, 2048)
	n, _, err := peer.ReadFromUDP(buffer)
	if err != nil {
		t.Fatal(err)
	}
	payload, err := RTPPayload(buffer[:n])
	if err != nil || len(payload) != 160 || !bytes.Equal(payload, bytes.Repeat([]byte{255}, 160)) {
		t.Fatal("PCM to RTP failed")
	}
	packet := make([]byte, 172)
	packet[0] = 128
	copy(packet[12:], bytes.Repeat([]byte{0xff}, 160))
	peer.WriteToUDP(packet, &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: ab.LocalPort()})
	select {
	case got := <-sink.chunks:
		if !bytes.Equal(got, pcm) {
			t.Fatal("RTP to PCM failed")
		}
	case <-time.After(time.Second):
		t.Fatal("no reverse PCM data")
	}
}
