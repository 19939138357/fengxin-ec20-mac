package cscall

import (
	"encoding/binary"
	"os"
	"testing"
)

// Fixtures generated from NAudio 1.10.0 MuLawEncoder/MuLawDecoder (MIT).
func TestG711AgainstNAudioFullDomain(t *testing.T) {
	enc, err := os.ReadFile("testdata/naudio-mulaw-encode.bin")
	if err != nil {
		t.Fatal(err)
	}
	dec, err := os.ReadFile("testdata/naudio-mulaw-decode.bin")
	if err != nil {
		t.Fatal(err)
	}
	if len(enc) != 65536 || len(dec) != 512 {
		t.Fatal("invalid reference fixtures")
	}
	for i := -32768; i <= 32767; i++ {
		// NAudio 1.10.0 negates Int16.MinValue in int16 and wraps. Saturate
		// this endpoint explicitly instead of reproducing that oracle defect.
		if i == -32768 {
			if LinearToUlaw(int16(i)) != 0x00 {
				t.Fatal("minimum PCM must clip to negative full scale")
			}
			continue
		}
		if got := LinearToUlaw(int16(i)); got != enc[i+32768] {
			t.Fatalf("encode %d got %02x want %02x", i, got, enc[i+32768])
		}
	}
	for i := 0; i < 256; i++ {
		// NAudio's table represents negative zero (0x7f) as -1; the
		// arithmetic G.711 mapping used here represents both zeros as 0.
		if i == 0x7f {
			if UlawToLinear(0x7f) != 0 {
				t.Fatal("negative zero")
			}
			continue
		}
		want := int16(binary.LittleEndian.Uint16(dec[i*2:]))
		if got := UlawToLinear(byte(i)); got != want {
			t.Fatalf("decode %02x got %d want %d", i, got, want)
		}
	}
}
