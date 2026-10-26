// Package client connects the CLI to the server, locally or over SSH.
package client

import (
	"io"
	"log"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/hashicorp/yamux"
)

// DefaultDataDir is where `hutch server` keeps its state for the current user.
func DefaultDataDir() string {
	if d := os.Getenv("XDG_STATE_HOME"); d != "" {
		return filepath.Join(d, "hutch", "server")
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".local", "state", "hutch", "server")
}

// Dial connects to a server on this machine through its socket, or, when sshTarget
// is set, by running `hutch api` on that host over SSH.
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

// SSHKeyPath is the key hutch server init makes for the servers it creates.
func SSHKeyPath() string {
	if d := os.Getenv("XDG_CONFIG_HOME"); d != "" {
		return filepath.Join(d, "hutch", "ssh", "id_ed25519")
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".config", "hutch", "ssh", "id_ed25519")
}

// SSHArgs are the ssh options every connection to a server uses: Hutch's own key
// and known hosts, when hutch server init made them, besides the user's.
func SSHArgs() []string {
	args := []string{"-o", "BatchMode=yes"}
	key := SSHKeyPath()
	if _, err := os.Stat(key); err == nil {
		home, _ := os.UserHomeDir()
		args = append(args, "-i", key, "-o", "UserKnownHostsFile="+filepath.Join(home, ".ssh", "known_hosts")+" "+filepath.Join(filepath.Dir(key), "known_hosts"))
	}
	return args
}

func sshConn(target string) (net.Conn, error) {
	cmd := exec.Command("ssh", append(SSHArgs(), target, "hutch", "api")...)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return &pipeConn{r: stdout, w: stdin, cmd: cmd}, nil
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

// Bridge connects stdin/stdout to the server socket; it is what `hutch api` runs.
func Bridge(socketPath string) error {
	conn, err := net.Dial("unix", socketPath)
	if err != nil {
		return err
	}
	defer conn.Close()
	done := make(chan struct{}, 2)
	go func() { io.Copy(conn, os.Stdin); done <- struct{}{} }()
	go func() { io.Copy(os.Stdout, conn); done <- struct{}{} }()
	<-done
	return nil
}
