// Package ptybridge pumps bytes between a tmux attach PTY and a WebSocket.
package ptybridge

import "errors"

// Frame kinds. One socket carries both streams so that auth, reconnect, and
// keepalive exist in one place rather than two.
//
// These values are a cross-language wire contract: the browser implements this
// same codec independently, so changing them silently breaks the frontend.
// TestFrameWireBytes pins the literal bytes for that reason.
const (
	FrameData    byte = 0x00 // raw PTY bytes, both directions
	FrameControl byte = 0x01 // JSON control message
)

var errBadFrame = errors.New("ptybridge: malformed frame")

// EncodeData wraps raw PTY bytes in a data frame.
//
// This allocates per frame, which is deliberate. Measured at 341ns/577B for a
// 512-byte frame and 8.5us/41KB for a 32KB one, dominated by the memcpy rather
// than the allocator; a saturated build log costs a few percent of one core,
// against a path where tmux's rendering and the WebSocket's own framing copy
// are the real bottlenecks. Reserving byte 0 in the PTY read buffer would avoid
// both the allocation and the copy, at the cost of moving framing out of the
// codec and into the pump's buffer management. Not worth it without a profile
// that says otherwise.
func EncodeData(b []byte) []byte { return append([]byte{FrameData}, b...) }

// EncodeControl wraps a JSON control message in a control frame.
func EncodeControl(b []byte) []byte { return append([]byte{FrameControl}, b...) }

// Decode splits a frame into its kind and payload. A frame carrying only a
// prefix byte is valid and yields an empty payload.
//
// The returned payload aliases frame and is only valid while frame is. That is
// safe for the current caller, which reads whole messages from coder/websocket
// and so owns a fresh buffer per message. It would stop being safe if the read
// path ever moved to a pooled or reused scratch buffer -- the same change that
// avoiding EncodeData's allocation would motivate, so treat the two together.
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
