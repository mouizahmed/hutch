package guestproto

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
)

const (
	Version = 1
	Port    = 1024

	maxFrame = 4 << 20
)

const (
	TypeHello   = "hello"
	TypeRestore = "restore"
	TypeExec    = "exec"
)

const (
	RespHello  = "hello"
	RespOK     = "ok"
	RespError  = "error"
	RespStdout = "stdout"
	RespStderr = "stderr"
	RespExit   = "exit"
)

type Request struct {
	Type    string   `json:"type"`
	Version int      `json:"version"`
	Restore *Restore `json:"restore,omitempty"`
	Exec    *Exec    `json:"exec,omitempty"`
}

type Restore struct {
	UnixNano int64  `json:"unix_nano"`
	BoxID    string `json:"box_id"`
	Hostname string `json:"hostname"`
	Seed     []byte `json:"seed,omitempty"`
}

type Exec struct {
	Argv []string `json:"argv"`
	User string   `json:"user,omitempty"`
	Dir  string   `json:"dir,omitempty"`
	Env  []string `json:"env,omitempty"`
}

type Response struct {
	Type     string `json:"type"`
	Version  int    `json:"version,omitempty"`
	Guest    string `json:"guest,omitempty"`
	Error    string `json:"error,omitempty"`
	Data     []byte `json:"data,omitempty"`
	ExitCode int    `json:"exit_code,omitempty"`
}

func Write(w io.Writer, v any) error {
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

func Read(r io.Reader, v any) error {
	var hdr [4]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return err
	}
	n := binary.BigEndian.Uint32(hdr[:])
	if n > maxFrame {
		return fmt.Errorf("frame of %d bytes exceeds %d", n, maxFrame)
	}
	b := make([]byte, n)
	if _, err := io.ReadFull(r, b); err != nil {
		return err
	}
	return json.Unmarshal(b, v)
}
