package keys

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"fmt"
	"math/big"
	"net"
	"net/http"
	"slices"
	"time"
)

// Lead does for a worker's proxy what needs what only the lead holds: the CA's
// key, the identity seed, and the one place a login refreshes.
type Lead interface {
	Cert(ctx context.Context, host string, pub []byte) ([]byte, error)
	Identity(ctx context.Context, box, host string) (Token, error)
	Refresh(ctx context.Context, c RefreshCall) (RefreshAnswer, error)
}

type Token struct {
	Token   string    `json:"token"`
	Expires time.Time `json:"expires"`
}

// RefreshCall is a box's token refresh, as the box sent it.
type RefreshCall struct {
	Box      string      `json:"box"`
	Login    string      `json:"login"`
	HostPort string      `json:"host_port"`
	URI      string      `json:"uri"`
	Header   http.Header `json:"header"`
	Body     []byte      `json:"body"`
}

// RefreshAnswer is what the box gets back, with placeholders only, and how the
// login changed.
type RefreshAnswer struct {
	Status  int         `json:"status"`
	Header  http.Header `json:"header"`
	Body    []byte      `json:"body"`
	Changes []Change    `json:"changes,omitempty"`
}

// SignCert signs a certificate for host and the public key pub (PKIX DER) with
// the machine's CA.
func (p *Proxy) SignCert(host string, pub []byte) ([]byte, error) {
	p.mu.Lock()
	ca, caCert := p.ca, p.caCert
	p.mu.Unlock()
	if ca == nil {
		return nil, errors.New("no CA yet")
	}
	key, err := x509.ParsePKIXPublicKey(pub)
	if err != nil {
		return nil, err
	}
	serial, _ := rand.Int(rand.Reader, big.NewInt(1<<62))
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: host},
		DNSNames:     []string{host},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(7 * 24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	if ip := net.ParseIP(host); ip != nil {
		tmpl.DNSNames, tmpl.IPAddresses = nil, []net.IP{ip}
	}
	return x509.CreateCertificate(rand.Reader, tmpl, caCert, key, ca.PrivateKey)
}

// SignIdentity signs box's identity for host.
func (p *Proxy) SignIdentity(box, host string) (Token, error) {
	p.mu.Lock()
	seed, project := p.table.IdentitySeed, p.table.Projects[box]
	p.mu.Unlock()
	now := time.Now()
	token, err := identityToken(seed, box, project, host, now)
	return Token{Token: token, Expires: now.Add(identityTTL)}, err
}

// Refresh performs a box's token refresh that a worker passed on, if it's one
// of a login the box is granted, to that login's token URL.
func (p *Proxy) Refresh(ctx context.Context, c RefreshCall) (RefreshAnswer, error) {
	host, port, err := net.SplitHostPort(c.HostPort)
	if err != nil {
		return RefreshAnswer{}, err
	}
	r, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://"+c.HostPort+c.URI, bytes.NewReader(c.Body))
	if err != nil {
		return RefreshAnswer{}, err
	}
	r.Header = c.Header
	logins, reuse := p.logins(c.Box)
	logins = slices.DeleteFunc(logins, func(l Login) bool { return l.Name != c.Login || !l.ForTarget(host, port) })
	l, body, ok := refreshRequest(r, host, port, logins)
	if !ok {
		return RefreshAnswer{}, fmt.Errorf("not a refresh of a login %s may use", c.Box)
	}
	return p.refreshed(ctx, c.Box, c.HostPort, l, r, body, reuse, p.transport)
}

// Intercepts reports whether a proxy with table t terminates TLS for host, as a
// login's host or an identity host.
func (t Table) Intercepts(host string) bool {
	if identityHost(t.IdentityHosts, host) {
		return true
	}
	for _, l := range t.Logins {
		for _, target := range l.Hosts {
			h, _, err := net.SplitHostPort(target)
			if err != nil {
				h = target
			}
			if matchesHost(host, h) {
				return true
			}
		}
	}
	return false
}

// Identifies reports whether box's requests to host carry its identity.
func (t Table) Identifies(box, host string) bool {
	_, exists := t.Projects[box]
	return exists && identityHost(t.IdentityHosts, host)
}
