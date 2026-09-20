// Package tunnel implements the control protocol between the home relay
// (the "backend") and the cloud tunnel component: a length-prefixed JSON
// handshake proving the backend holds a shared secret, followed by a yamux
// session multiplexing one logical stream per remote client. See
// docs/DESIGN.md's cloud-tunnel section for the full design.
package tunnel

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
)

// maxFrameBytes bounds a single handshake or stream-preamble frame. These
// are small, fixed-shape control messages, never message payloads, so this
// is a defensive cap against a misbehaving peer, not a real limit in
// practice.
const maxFrameBytes = 64 * 1024

// WriteFrame writes v as a length-prefixed JSON frame: a 4-byte big-endian
// length followed by the JSON body.
func WriteFrame(w io.Writer, v any) error {
	body, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("tunnel: marshal frame: %w", err)
	}
	if len(body) > maxFrameBytes {
		return fmt.Errorf("tunnel: frame too large: %d bytes", len(body))
	}
	var hdr [4]byte
	binary.BigEndian.PutUint32(hdr[:], uint32(len(body)))
	if _, err := w.Write(hdr[:]); err != nil {
		return fmt.Errorf("tunnel: write frame header: %w", err)
	}
	if _, err := w.Write(body); err != nil {
		return fmt.Errorf("tunnel: write frame body: %w", err)
	}
	return nil
}

// ReadFrame reads one length-prefixed JSON frame written by [WriteFrame] and
// decodes it into v.
func ReadFrame(r io.Reader, v any) error {
	var hdr [4]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return fmt.Errorf("tunnel: read frame header: %w", err)
	}
	n := binary.BigEndian.Uint32(hdr[:])
	if n > maxFrameBytes {
		return fmt.Errorf("tunnel: frame too large: %d bytes", n)
	}
	body := make([]byte, n)
	if _, err := io.ReadFull(r, body); err != nil {
		return fmt.Errorf("tunnel: read frame body: %w", err)
	}
	if err := json.Unmarshal(body, v); err != nil {
		return fmt.Errorf("tunnel: unmarshal frame: %w", err)
	}
	return nil
}
