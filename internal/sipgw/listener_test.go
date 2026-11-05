package sipgw

import (
	"context"
	"net"
	"testing"
)

func TestListenerConflictFailsStartup(t *testing.T) {
	occupied, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer occupied.Close()
	cfg := DefaultConfig()
	cfg.SIP.Listen = occupied.LocalAddr().String()
	r, err := NewRegistrar(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Stop()
	if r.Start(context.Background()) == nil {
		t.Fatal("reported ready while UDP bind failed")
	}
}
