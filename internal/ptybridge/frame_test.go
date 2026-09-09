package ptybridge

import (
	"bytes"
	"testing"
)

func TestFrameRoundTrip(t *testing.T) {
	t.Run("data frame", func(t *testing.T) {
		enc := EncodeData([]byte("hello"))
		kind, payload, err := Decode(enc)
		if err != nil || kind != FrameData || !bytes.Equal(payload, []byte("hello")) {
			t.Fatalf("kind=%v payload=%q err=%v", kind, payload, err)
		}
	})

	t.Run("control frame", func(t *testing.T) {
		enc := EncodeControl([]byte(`{"type":"resize"}`))
		kind, payload, err := Decode(enc)
		if err != nil || kind != FrameControl {
			t.Fatalf("kind=%v err=%v", kind, err)
		}
		if string(payload) != `{"type":"resize"}` {
			t.Fatalf("payload=%q", payload)
		}
	})

	t.Run("empty frame is an error, not a panic", func(t *testing.T) {
		if _, _, err := Decode(nil); err == nil {
			t.Fatal("want error for empty frame")
		}
	})

	t.Run("unknown kind is rejected", func(t *testing.T) {
		if _, _, err := Decode([]byte{0xEE, 'x'}); err == nil {
			t.Fatal("want error for unknown frame kind")
		}
	})
}

// TestFrameWireBytes pins the bytes on the wire. The round-trip test above is
// symmetric: it would still pass if both prefixes changed together. The browser
// end implements this codec independently against these exact values.
//
// It does not also assert the two kinds differ: a collision is a duplicate case
// in Decode's switch, which the compiler rejects.
func TestFrameWireBytes(t *testing.T) {
	if got, want := EncodeData([]byte("hi")), []byte{0x00, 'h', 'i'}; !bytes.Equal(got, want) {
		t.Errorf("EncodeData = %#v, want %#v", got, want)
	}
	if got, want := EncodeControl([]byte("hi")), []byte{0x01, 'h', 'i'}; !bytes.Equal(got, want) {
		t.Errorf("EncodeControl = %#v, want %#v", got, want)
	}
}

// TestDecodeBarePrefix pins that a frame carrying only a kind byte is a valid
// frame with an empty payload, not a malformed one. A control message is never
// empty, but an empty write on either side must not tear the connection down.
func TestDecodeBarePrefix(t *testing.T) {
	kind, payload, err := Decode([]byte{FrameData})
	if err != nil || kind != FrameData || len(payload) != 0 {
		t.Fatalf("kind=%v payload=%q err=%v", kind, payload, err)
	}
}
