package cscall

// Windows adaptation of VoHive's PCM/G.711/RTP bridge. The helper opens only
// the explicitly selected modem USB audio endpoint; it never uses a default mic.
import (
	"bufio"
	"crypto/rand"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type AudioBridge struct {
	startFn                 func() error
	alsaDev, deviceID       string
	rtpConn                 *net.UDPConn
	clientAddr              atomic.Pointer[net.UDPAddr]
	pcmReady                atomic.Bool
	captureCmd, playbackCmd *exec.Cmd
	captureOut              io.ReadCloser
	playbackIn              io.WriteCloser
	seqNum                  uint16
	timestamp, ssrc         uint32
	stop                    chan struct{}
	stopOnce                sync.Once
	wg                      sync.WaitGroup
	sendMu                  sync.Mutex
}

func NewAudioBridge(device, deviceID string) (*AudioBridge, error) {
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4zero, Port: 0})
	if err != nil {
		return nil, err
	}
	var random [10]byte
	rand.Read(random[:])
	return &AudioBridge{alsaDev: device, deviceID: deviceID, rtpConn: conn, stop: make(chan struct{}), ssrc: binary.BigEndian.Uint32(random[:4]), timestamp: binary.BigEndian.Uint32(random[4:8]), seqNum: binary.BigEndian.Uint16(random[8:])}, nil
}
func (ab *AudioBridge) LocalPort() int { return ab.rtpConn.LocalAddr().(*net.UDPAddr).Port }
func (ab *AudioBridge) SetClientAddr(ip string, port int) {
	ab.clientAddr.Store(&net.UDPAddr{IP: net.ParseIP(ip), Port: port})
}
func (ab *AudioBridge) SetPCMReady(v bool) { ab.pcmReady.Store(v) }
func (ab *AudioBridge) Start() error {
	if ab.startFn != nil {
		return ab.startFn()
	}
	var err error
	ab.captureCmd, err = audioCommand("capture", ab.alsaDev)
	if err != nil {
		return err
	}
	hideConsole(ab.captureCmd)
	ab.captureOut, err = ab.captureCmd.StdoutPipe()
	if err != nil {
		return err
	}
	ab.playbackCmd, err = audioCommand("playback", ab.alsaDev)
	if err != nil {
		return err
	}
	hideConsole(ab.playbackCmd)
	ab.playbackIn, err = ab.playbackCmd.StdinPipe()
	if err != nil {
		return err
	}
	if err = ab.startReady(ab.captureCmd); err != nil {
		return fmt.Errorf("USB capture: %w", err)
	}
	if err = ab.startReady(ab.playbackCmd); err != nil {
		return fmt.Errorf("USB playback: %w", err)
	}
	ab.wg.Add(2)
	go ab.loopCapture()
	go ab.loopPlayback()
	return nil
}

// Readiness is sent only after NAudio actually opens the selected USB endpoint.
func (ab *AudioBridge) startReady(cmd *exec.Cmd) error {
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return err
	}
	if err = cmd.Start(); err != nil {
		stderr.Close()
		return err
	}
	ready := make(chan error, 1)
	ab.wg.Add(1)
	go func() {
		defer ab.wg.Done()
		reader := bufio.NewReader(stderr)
		line, err := reader.ReadString('\n')
		if err != nil || strings.TrimSpace(line) != "READY" {
			ready <- fmt.Errorf("audio helper failed to open endpoint: %s (%v)", strings.TrimSpace(line), err)
			return
		}
		ready <- nil
		io.Copy(os.Stderr, reader)
	}()
	select {
	case err := <-ready:
		return err
	case <-time.After(5 * time.Second):
		return fmt.Errorf("audio helper readiness timeout")
	}
}
func (ab *AudioBridge) Stop() {
	ab.stopOnce.Do(func() {
		close(ab.stop)
		ab.rtpConn.Close()
		if ab.captureOut != nil {
			ab.captureOut.Close()
		}
		if ab.playbackIn != nil {
			ab.playbackIn.Close()
		}
		for _, cmd := range []*exec.Cmd{ab.captureCmd, ab.playbackCmd} {
			if cmd != nil && cmd.Process != nil {
				cmd.Process.Kill()
				cmd.Wait()
			}
		}
		ab.wg.Wait()
	})
}
func (ab *AudioBridge) loopCapture() {
	defer ab.wg.Done()
	pcm := make([]byte, 320)
	for {
		_, err := io.ReadFull(ab.captureOut, pcm)
		if err != nil {
			return
		}
		dst := ab.clientAddr.Load()
		if dst == nil {
			continue
		}
		payload := EncodePCMToUlaw(pcm)
		pkt := ab.buildRTPPacket(payload)
		if _, err = ab.rtpConn.WriteToUDP(pkt, dst); err != nil {
			return
		}
	}
}

// PCMU RTP parsing includes CSRCs, header extensions and padding.
func RTPPayload(packet []byte) ([]byte, error) {
	if len(packet) < 12 || packet[0]>>6 != 2 || packet[1]&127 != 0 {
		return nil, fmt.Errorf("invalid PCMU RTP")
	}
	offset := 12 + 4*int(packet[0]&15)
	if offset > len(packet) {
		return nil, fmt.Errorf("short CSRC header")
	}
	if packet[0]&16 != 0 {
		if offset+4 > len(packet) {
			return nil, fmt.Errorf("short extension")
		}
		offset += 4 + 4*int(binary.BigEndian.Uint16(packet[offset+2:offset+4]))
		if offset > len(packet) {
			return nil, fmt.Errorf("short extension data")
		}
	}
	end := len(packet)
	if packet[0]&32 != 0 {
		padding := int(packet[end-1])
		if padding == 0 || padding > end-offset {
			return nil, fmt.Errorf("bad RTP padding")
		}
		end -= padding
	}
	if end <= offset {
		return nil, fmt.Errorf("empty RTP")
	}
	return packet[offset:end], nil
}
func (ab *AudioBridge) loopPlayback() {
	defer ab.wg.Done()
	buf := make([]byte, 2048)
	for {
		n, src, err := ab.rtpConn.ReadFromUDP(buf)
		if err != nil {
			return
		}
		dst := ab.clientAddr.Load()
		// Source is fixed by the authenticated SIP SDP. Do not learn arbitrary RTP peers.
		if dst == nil || !dst.IP.Equal(src.IP) || dst.Port != src.Port || !ab.pcmReady.Load() {
			continue
		}
		payload, err := RTPPayload(buf[:n])
		if err != nil {
			continue
		}
		if _, err = ab.playbackIn.Write(DecodeUlawToPCM(payload)); err != nil {
			return
		}
	}
}
func (ab *AudioBridge) buildRTPPacket(payload []byte) []byte {
	ab.sendMu.Lock()
	defer ab.sendMu.Unlock()
	p := make([]byte, 12+len(payload))
	p[0] = 128
	binary.BigEndian.PutUint16(p[2:4], ab.seqNum)
	binary.BigEndian.PutUint32(p[4:8], ab.timestamp)
	binary.BigEndian.PutUint32(p[8:12], ab.ssrc)
	copy(p[12:], payload)
	ab.seqNum++
	ab.timestamp += uint32(len(payload))
	return p
}
