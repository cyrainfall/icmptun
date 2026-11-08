package main

import "testing"

// The checks themselves live in selftest.go so that `icmptun -mode selftest`
// and `go test` run exactly the same code.

func TestSelfChecks(t *testing.T) {
	for _, c := range selfChecks() {
		c := c
		t.Run(c.name, func(t *testing.T) {
			if err := c.run(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestDecodeFrameRejectsShortPayload(t *testing.T) {
	f := encodeFrame(&Frame{Payload: []byte{1, 2, 3, 4}})
	if _, ok := decodeFrame(f[:hdrSize+2]); ok {
		t.Fatal("decodeFrame accepted a payload shorter than the length field claims")
	}
}
