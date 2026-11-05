//go:build !darwin

package winmodem

import "io"

func openTransport(name string) (io.ReadWriteCloser, error) { return openSerial(name) }
