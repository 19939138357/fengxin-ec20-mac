package winmodem

import (
	"bufio"
	"fmt"
	"github.com/iniwex5/vohive/internal/machelper"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
)

type usbPipe struct {
	cmd  *exec.Cmd
	out  io.ReadCloser
	in   io.WriteCloser
	once sync.Once
}

func (p *usbPipe) Read(b []byte) (int, error)  { return p.out.Read(b) }
func (p *usbPipe) Write(b []byte) (int, error) { return p.in.Write(b) }
func (p *usbPipe) Close() error {
	p.once.Do(func() {
		p.in.Close()
		p.out.Close()
		done := make(chan struct{})
		go func() { p.cmd.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			p.cmd.Process.Kill()
			<-done
		}
	})
	return nil
}

func openTransport(name string) (io.ReadWriteCloser, error) {
	if !strings.HasPrefix(name, "usb:") {
		return openSerial(name)
	}
	cmd, err := machelper.Command("usb", name)
	if err != nil {
		return nil, err
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	in, err := cmd.StdinPipe()
	if err != nil {
		out.Close()
		return nil, err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		out.Close()
		in.Close()
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		out.Close()
		in.Close()
		stderr.Close()
		return nil, err
	}
	p := &usbPipe{cmd: cmd, out: out, in: in}
	ready := make(chan error, 1)
	go func() {
		r := bufio.NewReader(stderr)
		line, err := r.ReadString('\n')
		if err != nil || strings.TrimSpace(line) != "READY" {
			ready <- fmt.Errorf("USB helper startup: %s (%v)", strings.TrimSpace(line), err)
			return
		}
		ready <- nil
		io.Copy(os.Stderr, r)
	}()
	select {
	case err := <-ready:
		if err != nil {
			p.Close()
			return nil, err
		}
		return p, nil
	case <-time.After(5 * time.Second):
		p.Close()
		return nil, fmt.Errorf("USB helper readiness timeout")
	}
}
