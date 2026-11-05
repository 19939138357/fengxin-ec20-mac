package winmodem

import (
	"go.bug.st/serial"
	"io"
	"time"
)

func openSerial(name string) (io.ReadWriteCloser, error) {
	p, err := serial.Open(name, &serial.Mode{BaudRate: 115200})
	if err != nil {
		return nil, err
	}
	if err := p.SetReadTimeout(200 * time.Millisecond); err != nil {
		p.Close()
		return nil, err
	}
	return p, nil
}
