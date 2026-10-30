package keys

import (
	"bufio"
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"log"
	"math/big"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// NewCA makes the machine's CA: the proxy signs a certificate for each login
// host with it, and boxes trust it.
func NewCA() (cert, key []byte, err error) {
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, err
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(time.Now().UnixNano()),
		Subject:               pkix.Name{CommonName: "Hutch keys proxy"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().AddDate(20, 0, 0),
		IsCA:                  true,
		BasicConstraintsValid: true,
		MaxPathLenZero:        true,
		KeyUsage:              x509.KeyUsageCertSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &k.PublicKey, k)
	if err != nil {
		return nil, nil, err
	}
	kb, err := x509.MarshalECPrivateKey(k)
	if err != nil {
		return nil, nil, err
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kb}), nil
}

// Proxy serves every box's HTTPS_PROXY.
type Proxy struct {
	// Refreshed stores a refresh's new real values and placeholders before the box
	// hears of them; the server then pushes the new table with Set.
	Refreshed func(ctx context.Context, login string, changes []Change) error

	mu        sync.Mutex
	table     Table
	ca        *tls.Certificate
	caCert    *x509.Certificate
	certs     map[string]*tls.Certificate
	listeners map[string]net.Listener // by netns path
	refreshes map[string]*refreshState
	transport *http.Transport
}

// refreshState is a login's last refresh, answered again within the reuse window.
type refreshState struct {
	sync.Mutex
	at      time.Time
	status  int
	header  http.Header
	body    []byte
	refresh string // the real refresh token it left current
}

func NewProxy() *Proxy {
	p := &Proxy{certs: map[string]*tls.Certificate{}, listeners: map[string]net.Listener{}, refreshes: map[string]*refreshState{}}
	p.transport = &http.Transport{
		DialContext:         dialer(nil),
		ForceAttemptHTTP2:   true,
		TLSHandshakeTimeout: 10 * time.Second,
		IdleConnTimeout:     90 * time.Second,
	}
	return p
}

// Set replaces the table.
func (p *Proxy) Set(t Table) error {
	if len(t.IdentitySeed) != 0 && len(t.IdentitySeed) != ed25519.SeedSize {
		return fmt.Errorf("keys proxy: invalid identity seed")
	}
	ca, err := tls.X509KeyPair(t.CACert, t.CAKey)
	if err != nil {
		return fmt.Errorf("keys proxy CA: %w", err)
	}
	caCert, err := x509.ParseCertificate(ca.Certificate[0])
	if err != nil {
		return err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.caCert == nil || !p.caCert.Equal(caCert) {
		p.certs = map[string]*tls.Certificate{}
	}
	p.table, p.ca, p.caCert = t, &ca, caCert
	return nil
}

// Open serves the proxy of box, whose network namespace is ns. allow lists the
// private targets (host:port) the box may reach.
func (p *Proxy) Open(box, ns string, allow []string) error {
	l, err := listenIn(ns, net.JoinHostPort(Gateway, strconv.Itoa(Port)))
	if err != nil {
		return fmt.Errorf("keys proxy for %s: %w", box, err)
	}
	p.mu.Lock()
	if old := p.listeners[ns]; old != nil {
		old.Close()
	}
	p.listeners[ns] = l
	p.mu.Unlock()
	srv := &http.Server{Handler: &boxHandler{p: p, box: box, allow: allow}, ReadHeaderTimeout: time.Minute}
	go srv.Serve(l)
	return nil
}

// Close stops the proxy of the box in ns.
func (p *Proxy) Close(ns string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if l := p.listeners[ns]; l != nil {
		l.Close()
		delete(p.listeners, ns)
	}
}

// logins returns the logins box may use, and the reuse window.
func (p *Proxy) logins(box string) ([]Login, time.Duration) {
	p.mu.Lock()
	defer p.mu.Unlock()
	var out []Login
	for _, l := range p.table.Logins {
		if Granted(p.table.Grants[box], l.Adapter) {
			out = append(out, l)
		}
	}
	return out, p.table.Reuse
}

type boxHandler struct {
	p     *Proxy
	box   string
	allow []string
}

func (h *boxHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodConnect {
		http.Error(w, "the Hutch keys proxy takes HTTPS (CONNECT) only", http.StatusMethodNotAllowed)
		return
	}
	host, port, err := net.SplitHostPort(r.Host)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	host = strings.ToLower(host)
	logins, _ := h.p.logins(h.box)
	var mine []Login
	for _, l := range logins {
		if port == "443" && l.ForHost(host) {
			mine = append(mine, l)
		}
	}
	var up net.Conn
	if len(mine) == 0 && !(port == "443" && h.p.identifies(h.box, host)) {
		if up, err = dialer(h.allow)(r.Context(), "tcp", r.Host); err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
	}
	conn, buf, err := http.NewResponseController(w).Hijack()
	if err != nil {
		if up != nil {
			up.Close()
		}
		return
	}
	conn.Write([]byte("HTTP/1.1 200 Connection established\r\n\r\n"))
	in := buffered(conn, buf)
	if up != nil {
		tunnel(in, up)
		return
	}
	h.p.intercept(in, h.box, host, r.Host, h.allow)
}

// buffered keeps what the server read past the CONNECT request.
func buffered(conn net.Conn, buf *bufio.ReadWriter) net.Conn {
	n := buf.Reader.Buffered()
	if n == 0 {
		return conn
	}
	b, _ := buf.Reader.Peek(n)
	return &prefixConn{Conn: conn, r: io.MultiReader(bytes.NewReader(bytes.Clone(b)), conn)}
}

func tunnel(conn, up net.Conn) {
	defer conn.Close()
	defer up.Close()
	go func() {
		io.Copy(up, conn)
		if c, ok := up.(interface{ CloseWrite() error }); ok {
			c.CloseWrite()
		}
	}()
	io.Copy(conn, up)
}

// intercept terminates the box's TLS for a login host and serves its requests.
func (p *Proxy) intercept(conn net.Conn, box, host, hostport string, allow []string) {
	tc := tls.Server(conn, &tls.Config{
		NextProtos:     []string{"http/1.1"},
		GetCertificate: func(*tls.ClientHelloInfo) (*tls.Certificate, error) { return p.cert(host) },
	})
	if err := tc.Handshake(); err != nil {
		log.Printf("keys proxy: %s: TLS with %s: %v (does the box trust the machine's CA?)", box, host, err)
		conn.Close()
		return
	}
	transport := p.transport.Clone()
	transport.DialContext = dialer(allow)
	defer transport.CloseIdleConnections()
	rp := &httputil.ReverseProxy{
		Rewrite: func(r *httputil.ProxyRequest) {
			r.Out.URL.Scheme, r.Out.URL.Host, r.Out.Host = "https", hostport, r.In.Host
		},
		Transport:     transport,
		FlushInterval: -1,
		ErrorLog:      log.New(io.Discard, "", 0),
	}
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// A keep-alive connection must see rotations and revoked grants too.
		current, reuse := p.logins(box)
		var logins []Login
		for _, l := range current {
			if l.ForHost(host) {
				logins = append(logins, l)
			}
		}
		if err := p.addIdentity(r.Header, box, host, time.Now()); err != nil {
			http.Error(w, "could not sign the box's identity", http.StatusBadGateway)
			return
		}
		if l, body, ok := refreshRequest(r, logins); ok {
			p.refresh(w, r, box, hostport, l, body, reuse, transport)
			return
		}
		swapHeaders(r.Header, logins)
		rp.ServeHTTP(w, r)
	})
	srv := &http.Server{Handler: h, ReadHeaderTimeout: time.Minute}
	srv.Serve(&oneListener{c: tc, done: make(chan struct{})})
}

// cert returns a certificate for host signed by the machine's CA.
func (p *Proxy) cert(host string) (*tls.Certificate, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if c := p.certs[host]; c != nil && time.Until(c.Leaf.NotAfter) > time.Hour {
		return c, nil
	}
	if p.ca == nil {
		return nil, errors.New("no CA yet")
	}
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
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
	der, err := x509.CreateCertificate(rand.Reader, tmpl, p.caCert, &k.PublicKey, p.ca.PrivateKey)
	if err != nil {
		return nil, err
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, err
	}
	c := &tls.Certificate{Certificate: [][]byte{der}, PrivateKey: k, Leaf: leaf}
	p.certs[host] = c
	return c, nil
}

// realOf maps each of logins' placeholders to its real value.
func realOf(logins []Login) map[string]string {
	m := map[string]string{}
	for _, l := range logins {
		for _, s := range l.Secrets {
			for _, ph := range s.Placeholders {
				m[ph] = s.Real
			}
		}
	}
	return m
}

// swapHeaders replaces placeholders of logins in request headers, including
// inside Basic auth (git's), with their real values.
func swapHeaders(h http.Header, logins []Login) {
	swaps := realOf(logins)
	if len(swaps) == 0 {
		return
	}
	for k, vs := range h {
		for i, v := range vs {
			v = string(Replace([]byte(v), swaps))
			if b, ok := strings.CutPrefix(v, "Basic "); ok {
				if raw, err := base64.StdEncoding.DecodeString(b); err == nil {
					v = "Basic " + base64.StdEncoding.EncodeToString(Replace(raw, swaps))
				}
			}
			h[k][i] = v
		}
	}
}

// refreshRequest recognises an OAuth 2 refresh (grant_type=refresh_token) whose
// refresh token is a placeholder of one of logins. It leaves r's body readable.
func refreshRequest(r *http.Request, logins []Login) (Login, []byte, bool) {
	ct := r.Header.Get("Content-Type")
	if r.Method != http.MethodPost || !strings.Contains(ct, "json") && !strings.Contains(ct, "form") {
		return Login{}, nil, false
	}
	const limit = 1 << 20
	body, err := io.ReadAll(io.LimitReader(r.Body, limit+1))
	r.Body = io.NopCloser(io.MultiReader(bytes.NewReader(body), r.Body))
	if err != nil || len(body) > limit {
		return Login{}, nil, false
	}
	fields := map[string]string{}
	if strings.Contains(ct, "json") {
		var m map[string]any
		json.Unmarshal(body, &m)
		for k, v := range m {
			if s, ok := v.(string); ok {
				fields[k] = s
			}
		}
	} else if q, err := url.ParseQuery(string(body)); err == nil {
		for k := range q {
			fields[k] = q.Get(k)
		}
	}
	if fields["grant_type"] != "refresh_token" {
		return Login{}, nil, false
	}
	for _, l := range logins {
		if _, ok := realOf([]Login{l})[fields["refresh_token"]]; ok && l.Refreshes() {
			return l, body, true
		}
	}
	return Login{}, nil, false
}

// refresh handles a box's token refresh for login l, one at a time per login:
// within reuse of the last one it answers from that; otherwise it refreshes the
// real login upstream, has the new values stored, and answers with placeholders.
func (p *Proxy) refresh(w http.ResponseWriter, r *http.Request, box, hostport string, l Login, body []byte, reuse time.Duration, transport *http.Transport) {
	p.mu.Lock()
	st := p.refreshes[l.Name]
	if st == nil {
		st = &refreshState{}
		p.refreshes[l.Name] = st
	}
	p.mu.Unlock()
	st.Lock()
	defer st.Unlock()

	// Another box's refresh may have changed the login while this one waited.
	for _, x := range p.allLogins() {
		if x.Name == l.Name {
			l = x
		}
	}
	current := ""
	for _, s := range l.Secrets {
		if s.Key == Norm("refresh_token") {
			current = s.Real
		}
	}
	if st.body != nil && time.Since(st.at) < reuse && st.refresh == current {
		writeCached(w, st)
		return
	}
	// The body's placeholders become real values, as sent (JSON or form-encoded).
	swaps := map[string]string{}
	for ph, v := range realOf([]Login{l}) {
		swaps[ph] = v
		swaps[url.QueryEscape(ph)] = url.QueryEscape(v)
	}
	out, err := http.NewRequestWithContext(r.Context(), http.MethodPost, "https://"+hostport+r.URL.RequestURI(), bytes.NewReader(Replace(body, swaps)))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	out.Header = r.Header.Clone()
	out.Header.Del("Accept-Encoding")
	out.Header.Del("Content-Length")
	swapHeaders(out.Header, []Login{l})
	resp, err := transport.RoundTrip(out)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	var m map[string]any
	if resp.StatusCode != http.StatusOK || json.Unmarshal(data, &m) != nil {
		// A refused refresh (a revoked login) reaches the box as it is, and the
		// agent asks to log in again.
		copyHeader(w.Header(), resp.Header)
		w.WriteHeader(resp.StatusCode)
		w.Write(data)
		return
	}
	changes := Placeholdered(m, l)
	if len(changes) > 0 {
		if err := p.Refreshed(r.Context(), l.Name, changes); err != nil {
			log.Printf("keys proxy: store %s's refresh: %v", l.Name, err)
			http.Error(w, "the machine couldn't store the refreshed login: "+err.Error(), http.StatusBadGateway)
			return
		}
	}
	log.Printf("keys proxy: refreshed %s's login for %s", l.Name, box)
	data, _ = json.Marshal(m)
	st.at, st.status, st.body, st.refresh = time.Now(), resp.StatusCode, data, current
	for _, c := range changes {
		if c.Key == Norm("refresh_token") {
			st.refresh = c.New
		}
	}
	st.header = http.Header{"Content-Type": {resp.Header.Get("Content-Type")}}
	writeCached(w, st)
}

func (p *Proxy) allLogins() []Login {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.table.Logins
}

// Placeholdered puts placeholders in a token response in place of every token in
// it, and returns how login l's secrets change.
func Placeholdered(m map[string]any, l Login) []Change {
	var changes []Change
	for k, v := range m {
		nw, ok := v.(string)
		if !ok || nw == "" || !strings.HasSuffix(Norm(k), "token") {
			continue
		}
		c := Change{Key: Norm(k), New: nw}
		for _, s := range l.Secrets {
			if s.Key == c.Key {
				c.Old = s.Real
				if len(s.Placeholders) > 0 {
					c.Placeholder = s.Placeholders[0]
				}
			}
		}
		// A JWT's placeholder carries its claims, so a new JWT gets a new one;
		// other values keep theirs, so every box's copy stays current.
		if c.Placeholder == "" || c.Old != nw && IsJWT(nw) {
			c.Placeholder = Placeholder(nw)
		}
		m[k] = c.Placeholder
		if c.Old != nw {
			changes = append(changes, c)
		}
	}
	return changes
}

func writeCached(w http.ResponseWriter, st *refreshState) {
	copyHeader(w.Header(), st.header)
	w.WriteHeader(st.status)
	w.Write(st.body)
}

func copyHeader(dst, src http.Header) {
	for k, vs := range src {
		if k == "Content-Length" || k == "Content-Encoding" || k == "Transfer-Encoding" {
			continue
		}
		dst[k] = append([]string(nil), vs...)
	}
}

// dialer connects for a box: never to the machine itself, private or link-local
// networks, or loopback, unless allow names the target (host:port).
func dialer(allow []string) func(ctx context.Context, network, addr string) (net.Conn, error) {
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		permitted := map[string]bool{}
		for _, a := range allow {
			host, port, err := net.SplitHostPort(a)
			if err != nil {
				continue
			}
			ips, _ := net.DefaultResolver.LookupIPAddr(ctx, host)
			for _, ip := range ips {
				permitted[net.JoinHostPort(ip.IP.String(), port)] = true
			}
		}
		d := &net.Dialer{Timeout: 30 * time.Second, Control: func(_, address string, _ syscall.RawConn) error {
			host, _, err := net.SplitHostPort(address)
			if err != nil {
				return err
			}
			if ip := net.ParseIP(host); ip == nil || NeverAllowed(ip) || Blocked(ip) && !permitted[address] {
				return fmt.Errorf("%s is not reachable from a box", address)
			}
			return nil
		}}
		return d.DialContext(ctx, network, addr)
	}
}

var private = func() []*net.IPNet {
	var out []*net.IPNet
	for _, c := range []string{"10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16", "100.64.0.0/10", "169.254.0.0/16", "fc00::/7"} {
		_, n, _ := net.ParseCIDR(c)
		out = append(out, n)
	}
	return out
}()

// Blocked reports whether ip is the machine's own, private, or otherwise not for
// a box to reach.
func Blocked(ip net.IP) bool {
	if NeverAllowed(ip) {
		return true
	}
	for _, n := range private {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}

// NeverAllowed cannot be overridden by network.allow: the machine itself,
// metadata, loopback, and the networks used to reach other boxes.
func NeverAllowed(ip net.IP) bool {
	if ip.IsLoopback() || ip.IsUnspecified() || ip.IsMulticast() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() {
		return true
	}
	if v4 := ip.To4(); v4 != nil && v4[0] == 10 && (v4[1] == 11 || v4[1] == 12) {
		return true
	}
	addrs, _ := net.InterfaceAddrs()
	for _, a := range addrs {
		if n, ok := a.(*net.IPNet); ok && n.IP.Equal(ip) {
			return true
		}
	}
	return false
}

type prefixConn struct {
	net.Conn
	r io.Reader
}

func (c *prefixConn) Read(b []byte) (int, error) { return c.r.Read(b) }

// oneListener yields one connection, then waits for it to close.
type oneListener struct {
	c    net.Conn
	once sync.Once
	done chan struct{}
}

func (l *oneListener) Accept() (net.Conn, error) {
	var c net.Conn
	l.once.Do(func() { c = &closeConn{Conn: l.c, done: l.done} })
	if c != nil {
		return c, nil
	}
	<-l.done
	return nil, net.ErrClosed
}

func (l *oneListener) Close() error   { return nil }
func (l *oneListener) Addr() net.Addr { return l.c.LocalAddr() }

type closeConn struct {
	net.Conn
	done chan struct{}
	once sync.Once
}

func (c *closeConn) Close() error {
	c.once.Do(func() { close(c.done) })
	return c.Conn.Close()
}
