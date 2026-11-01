// Package link is the worker's side of the worker link: enrolling with the lead
// and dialing it with the certificate it signed.
package link

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/mouizahmed/hutch/internal/pki"
	"github.com/mouizahmed/hutch/internal/wire"
)

// EnrollRequest is all a worker without a certificate may send: the secret of a
// one-time token, its name, and a CSR for its key.
type EnrollRequest struct {
	Secret string `json:"secret"`
	Name   string `json:"name"`
	CSR    []byte `json:"csr"`
}

type EnrollResponse struct {
	WorkerID string `json:"worker_id,omitempty"`
	Cert     []byte `json:"cert,omitempty"`
	CA       []byte `json:"ca,omitempty"`
	Error    string `json:"error,omitempty"`
}

// Dir is a worker's enrollment: its key and certificate, the CA, the lead's
// address, and the highest term it has seen.
type Dir string

func (d Dir) path(name string) string { return filepath.Join(string(d), name) }

// Enrolled reports whether this machine is a worker of another machine's lead.
func (d Dir) Enrolled() bool {
	_, err := os.Stat(d.path("lead"))
	return err == nil
}

// Lead is the lead's link address, host:port.
func (d Dir) Lead() string {
	b, _ := os.ReadFile(d.path("lead"))
	return strings.TrimSpace(string(b))
}

func (d Dir) Name() string {
	b, _ := os.ReadFile(d.path("name"))
	return strings.TrimSpace(string(b))
}

// Enroll exchanges a token for a certificate. The token carries the CA's
// fingerprint, so the worker knows it is talking to the right lead before
// sending anything.
func Enroll(ctx context.Context, d Dir, lead, token, name string) (string, error) {
	fp, secret, ok := strings.Cut(token, ".")
	if !ok || len(fp) != 64 {
		return "", errors.New("malformed token")
	}
	host, _, err := net.SplitHostPort(lead)
	if err != nil {
		return "", err
	}
	keyPEM, csr, err := pki.NewWorkerKey(name)
	if err != nil {
		return "", err
	}
	conf := &tls.Config{
		MinVersion:         tls.VersionTLS13,
		InsecureSkipVerify: true, // verified below against the pinned CA
		VerifyConnection: func(cs tls.ConnectionState) error {
			certs := cs.PeerCertificates
			if len(certs) < 2 {
				return errors.New("the lead did not send its CA certificate")
			}
			ca := certs[len(certs)-1]
			if pki.Fingerprint(ca) != fp {
				return errors.New("the lead's CA does not match the token")
			}
			pool := x509.NewCertPool()
			pool.AddCert(ca)
			_, err := certs[0].Verify(x509.VerifyOptions{Roots: pool, DNSName: host})
			return err
		},
	}
	dialer := tls.Dialer{Config: conf, NetDialer: &net.Dialer{Timeout: 10 * time.Second}}
	conn, err := dialer.DialContext(ctx, "tcp", lead)
	if err != nil {
		return "", fmt.Errorf("can't reach the lead at %s (its firewall or security group must allow this machine): %w", lead, err)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(30 * time.Second))
	if err := wire.Write(conn, EnrollRequest{Secret: secret, Name: name, CSR: csr}); err != nil {
		return "", err
	}
	var resp EnrollResponse
	if err := wire.Read(conn, &resp); err != nil {
		return "", err
	}
	if resp.Error != "" {
		return "", errors.New(resp.Error)
	}
	if err := os.MkdirAll(string(d), 0o700); err != nil {
		return "", err
	}
	// lead goes last: it marks the enrollment complete.
	for _, f := range []struct {
		name string
		data []byte
	}{{"key.pem", keyPEM}, {"cert.pem", resp.Cert}, {"ca.pem", resp.CA}, {"name", []byte(name + "\n")}, {"lead", []byte(lead + "\n")}} {
		if err := os.WriteFile(d.path(f.name), f.data, 0o600); err != nil {
			return "", err
		}
	}
	return resp.WorkerID, nil
}

// Dial connects to the lead with the worker's certificate.
func Dial(ctx context.Context, d Dir) (net.Conn, error) {
	cert, err := tls.LoadX509KeyPair(d.path("cert.pem"), d.path("key.pem"))
	if err != nil {
		return nil, err
	}
	caPEM, err := os.ReadFile(d.path("ca.pem"))
	if err != nil {
		return nil, err
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		return nil, errors.New("invalid ca.pem")
	}
	b, err := os.ReadFile(d.path("lead"))
	if err != nil {
		return nil, err
	}
	lead := strings.TrimSpace(string(b))
	host, _, err := net.SplitHostPort(lead)
	if err != nil {
		return nil, err
	}
	conf := &tls.Config{Certificates: []tls.Certificate{cert}, RootCAs: pool, ServerName: host, MinVersion: tls.VersionTLS13}
	dialer := tls.Dialer{Config: conf, NetDialer: &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 10 * time.Second}}
	return dialer.DialContext(ctx, "tcp", lead)
}

// CheckTerm refuses a lead whose term is below the highest this worker has
// seen, and records a higher one.
func CheckTerm(d Dir, term int64) error {
	b, _ := os.ReadFile(d.path("term"))
	seen, _ := strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64)
	if term < seen {
		return fmt.Errorf("refusing a lead at term %d: this worker has seen term %d", term, seen)
	}
	if term == seen {
		return nil
	}
	tmp := d.path("term.tmp")
	if err := os.WriteFile(tmp, []byte(strconv.FormatInt(term, 10)+"\n"), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, d.path("term"))
}
