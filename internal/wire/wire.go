package wire

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
)

// MaxFrame bounds frames from guests, which are untrusted. MaxRPCFrame bounds calls
// between the CLI, server, and workers, which carry user layers and project secrets.
const (
	MaxFrame    = 4 << 20
	MaxRPCFrame = 64 << 20
)

// Write sends v as one frame: a 4-byte big-endian length, then JSON.
func Write(w io.Writer, v any) error { return WriteMax(w, v, MaxFrame) }

func Read(r io.Reader, v any) error { return ReadMax(r, v, MaxFrame) }

func WriteMax(w io.Writer, v any, maxFrame int) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	if len(b) > maxFrame {
		return fmt.Errorf("frame of %d bytes exceeds %d", len(b), maxFrame)
	}
	var hdr [4]byte
	binary.BigEndian.PutUint32(hdr[:], uint32(len(b)))
	if _, err := w.Write(hdr[:]); err != nil {
		return err
	}
	_, err = w.Write(b)
	return err
}

func ReadMax(r io.Reader, v any, maxFrame int) error {
	var hdr [4]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return err
	}
	n := binary.BigEndian.Uint32(hdr[:])
	if int64(n) > int64(maxFrame) {
		return fmt.Errorf("frame of %d bytes exceeds %d", n, maxFrame)
	}
	b := make([]byte, n)
	if _, err := io.ReadFull(r, b); err != nil {
		return err
	}
	return json.Unmarshal(b, v)
}
