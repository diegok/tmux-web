// Package ptybridge pumps bytes between a tmux attach PTY and a WebSocket.
package ptybridge

import "errors"

// Frame kinds. One socket carries both streams so that auth, reconnect, and
// keepalive exist in one place rather than two.
const (
	FrameData    byte = 0x00 // raw PTY bytes, both directions
	FrameControl byte = 0x01 // JSON control message
)

var errBadFrame = errors.New("ptybridge: malformed frame")

func EncodeData(b []byte) []byte    { return append([]byte{FrameData}, b...) }
func EncodeControl(b []byte) []byte { return append([]byte{FrameControl}, b...) }

func Decode(frame []byte) (kind byte, payload []byte, err error) {
	if len(frame) == 0 {
		return 0, nil, errBadFrame
	}
	switch frame[0] {
	case FrameData, FrameControl:
		return frame[0], frame[1:], nil
	default:
		return 0, nil, errBadFrame
	}
}
