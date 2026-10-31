package keys

import (
	"bytes"
	"compress/gzip"
	"compress/zlib"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// scrubber replaces real values with their placeholders in what reaches a box.
type scrubber struct {
	reals map[string]string // real → placeholder
}

// scrubberOf scrubs every real value in logins.
func scrubberOf(logins []Login) scrubber {
	s := scrubber{reals: map[string]string{}}
	s.include(logins)
	return s
}

func (s scrubber) include(logins []Login) {
	for _, l := range logins {
		for _, sec := range l.Secrets {
			if sec.Real != "" && len(sec.Placeholders) > 0 {
				s.reals[sec.Real] = sec.Placeholders[0]
			}
		}
	}
}

// sentHeaders remembers the actual auth representations sent upstream, including
// Basic's encoded user/password. The provider may echo those as one value.
func (s scrubber) sentHeaders(before, after http.Header) {
	for name, values := range after {
		old := before.Values(name)
		for i, value := range values {
			if i >= len(old) || value == old[i] || value == "" {
				continue
			}
			s.reals[value] = old[i]
			scheme, encoded, _ := strings.Cut(value, " ")
			oldScheme, oldEncoded, _ := strings.Cut(old[i], " ")
			if strings.EqualFold(scheme, "Basic") && strings.EqualFold(oldScheme, "Basic") {
				s.reals[encoded] = oldEncoded
				raw, err := base64.StdEncoding.DecodeString(encoded)
				oldRaw, oldErr := base64.StdEncoding.DecodeString(oldEncoded)
				if err == nil && oldErr == nil && len(raw) > 0 {
					s.reals[string(raw)] = string(oldRaw)
				}
			}
		}
	}
}

func (s scrubber) bytes(b []byte) []byte { return Replace(b, s.reals) }

func (s scrubber) header(h http.Header) {
	for k, vs := range h {
		for i, v := range vs {
			h[k][i] = string(s.bytes([]byte(v)))
		}
	}
}

// held is how many bytes at the end of b could be the start of a real value, so
// they wait for what follows instead of reaching the box half-checked.
func (s scrubber) held(b []byte) int {
	hold := 0
	for real := range s.reals {
		for i := max(0, len(b)-len(real)+1); i < len(b)-hold; i++ {
			if b[i] == real[0] && bytes.HasPrefix([]byte(real), b[i:]) {
				hold = len(b) - i
				break
			}
		}
	}
	return hold
}

// scrubReader streams r with real values replaced, without delaying bytes that
// can't be part of one.
type scrubReader struct {
	r       io.Reader
	s       scrubber
	pending []byte
	out     []byte
	err     error
	done    func()
}

func (s scrubber) reader(r io.Reader) *scrubReader { return &scrubReader{r: r, s: s} }

func (sr *scrubReader) Read(p []byte) (int, error) {
	for len(sr.out) == 0 {
		if sr.err != nil {
			if len(sr.pending) > 0 {
				sr.out, sr.pending = sr.s.bytes(sr.pending), nil
				continue
			}
			return 0, sr.err
		}
		buf := make([]byte, 32<<10)
		n, err := sr.r.Read(buf)
		sr.err = err
		if err != nil && sr.done != nil {
			sr.done()
			sr.done = nil
		}
		sr.pending = sr.s.bytes(append(sr.pending, buf[:n]...))
		hold := 0
		if err == nil {
			hold = sr.s.held(sr.pending)
		}
		cut := len(sr.pending) - hold
		sr.out = append(sr.out, sr.pending[:cut]...)
		sr.pending = append([]byte(nil), sr.pending[cut:]...)
	}
	n := copy(p, sr.out)
	sr.out = sr.out[n:]
	return n, nil
}

// scrubBody is a response body with real values replaced. An upgraded connection
// (a WebSocket) stays writable.
type scrubBody struct {
	*scrubReader
	c io.Closer
	w io.Writer
}

func (b scrubBody) Close() error { return b.c.Close() }

func (b scrubBody) Write(p []byte) (int, error) {
	if b.w == nil {
		return 0, io.ErrClosedPipe
	}
	return b.w.Write(p)
}

// response replaces known real values in a response's headers, decoded body and
// trailers. Unsupported encodings fail closed rather than bypassing the filter.
func (s scrubber) response(resp *http.Response) error {
	body, err := decodedResponse(resp)
	if err != nil {
		return err
	}
	s.header(resp.Header)
	s.header(resp.Trailer)
	filtered := scrubBody{scrubReader: s.reader(body), c: body}
	// Go fills response trailers only when the body reaches EOF.
	filtered.done = func() { s.header(resp.Trailer) }
	if w, ok := body.(io.Writer); ok {
		filtered.w = w
	}
	resp.Body = filtered
	resp.ContentLength = -1
	resp.Header.Del("Content-Length")
	return nil
}

type decodedBody struct {
	io.ReadCloser
	source io.Closer
}

func (b decodedBody) Close() error { return errors.Join(b.ReadCloser.Close(), b.source.Close()) }

func decodedResponse(resp *http.Response) (io.ReadCloser, error) {
	var decoder io.ReadCloser
	var err error
	switch strings.ToLower(strings.TrimSpace(resp.Header.Get("Content-Encoding"))) {
	case "", "identity":
		return resp.Body, nil
	case "gzip", "x-gzip":
		decoder, err = gzip.NewReader(resp.Body)
	case "deflate":
		decoder, err = zlib.NewReader(resp.Body)
	default:
		return nil, fmt.Errorf("keys proxy: unsupported upstream content encoding")
	}
	if err != nil {
		return nil, fmt.Errorf("keys proxy: invalid encoded response")
	}
	resp.Header.Del("Content-Encoding")
	resp.Header.Del("Content-Length")
	resp.ContentLength = -1
	return decodedBody{ReadCloser: decoder, source: resp.Body}, nil
}
