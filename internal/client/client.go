// Package client connects the CLI to hutchd, locally or over SSH.
package client

import (
	"bytes"
	"errors"
	"io"
	"log"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/hashicorp/yamux"
)

// Socket is hutchd's API socket on its machine.
const Socket = "/run/hutch/api.sock"

// Dial connects to hutchd on this machine through its socket, or, when sshTarget
// is set, by running `hutchd api` on that host over SSH.
func Dial(sshTarget, socketPath string) (*yamux.Session, error) {
	var conn net.Conn
	if sshTarget == "" {
		c, err := net.Dial("unix", socketPath)
		if err != nil {
			return nil, err
		}
		conn = c
	} else {
		c, err := sshConn(sshTarget)
		if err != nil {
			return nil, err
		}
		conn = c
	}
	cfg := yamux.DefaultConfig()
	cfg.LogOutput = nil
	cfg.Logger = log.New(io.Discard, "", 0)
	return yamux.Client(conn, cfg)
}

// SSHKeyPath is Hutch's key for reaching boxes over SSH (hutch ssh-config).
func SSHKeyPath() string {
	if d := os.Getenv("XDG_CONFIG_HOME"); d != "" {
		return filepath.Join(d, "hutch", "ssh", "id_ed25519")
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".config", "hutch", "ssh", "id_ed25519")
}

func sshConn(target string) (net.Conn, error) {
	cmd := exec.Command("ssh", "-o", "BatchMode=yes", target, "hutchd", "api")
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	cmd.Stderr = &sshStderr
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return &pipeConn{r: stdout, w: stdin, cmd: cmd}, nil
}

// sshStderr keeps what ssh and hutchd api said, for when the connection fails.
var sshStderr lockedBuffer

type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

// SSHError is what ssh or hutchd api printed before a connection closed.
func SSHError() string {
	sshStderr.mu.Lock()
	defer sshStderr.mu.Unlock()
	return strings.TrimSpace(sshStderr.b.String())
}

// pipeConn presents an SSH child process's stdio as a net.Conn for yamux.
type pipeConn struct {
	r   io.ReadCloser
	w   io.WriteCloser
	cmd *exec.Cmd
}

func (p *pipeConn) Read(b []byte) (int, error)  { return p.r.Read(b) }
func (p *pipeConn) Write(b []byte) (int, error) { return p.w.Write(b) }
func (p *pipeConn) Close() error {
	p.w.Close()
	p.r.Close()
	p.cmd.Process.Kill()
	p.cmd.Wait()
	return nil
}
func (p *pipeConn) LocalAddr() net.Addr              { return pipeAddr{} }
func (p *pipeConn) RemoteAddr() net.Addr             { return pipeAddr{} }
func (p *pipeConn) SetDeadline(time.Time) error      { return nil }
func (p *pipeConn) SetReadDeadline(time.Time) error  { return nil }
func (p *pipeConn) SetWriteDeadline(time.Time) error { return nil }

type pipeAddr struct{}

func (pipeAddr) Network() string { return "ssh" }
func (pipeAddr) String() string  { return "ssh" }

// Bridge connects stdin/stdout to hutchd's socket; it is what `hutchd api` runs.
func Bridge(socketPath string) error {
	conn, err := net.Dial("unix", socketPath)
	switch {
	case errors.Is(err, os.ErrNotExist) || errors.Is(err, syscall.ECONNREFUSED):
		return errors.New("not running here: sudo systemctl start hutchd (journalctl -u hutchd says why it stopped)")
	case errors.Is(err, os.ErrPermission):
		return errors.New("this user can't reach hutchd yet: it's in the hutch group from its next login (or run hutch setup again)")
	case err != nil:
		return err
	}
	defer conn.Close()
	done := make(chan struct{}, 2)
	go func() { io.Copy(conn, os.Stdin); done <- struct{}{} }()
	go func() { io.Copy(os.Stdout, conn); done <- struct{}{} }()
	<-done
	return nil
}
