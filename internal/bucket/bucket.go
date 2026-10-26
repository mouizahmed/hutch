// Package bucket talks to the one S3-compatible bucket that holds all durable state.
package bucket

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"
)

// Config locates the bucket. An empty Endpoint means AWS S3 in Region.
type Config struct {
	Endpoint  string `json:"endpoint,omitempty"`
	Region    string `json:"region"`
	Name      string `json:"name"`
	AccessKey string `json:"access_key"`
	SecretKey string `json:"secret_key"`
}

var (
	ErrNotFound = errors.New("not found")
	// ErrExists is returned by a conditional Put when the key is already there.
	ErrExists = errors.New("already exists")
)

type Object struct {
	Key      string
	Size     int64
	Modified time.Time
}

// Bucket is the operations Hutch needs: whole-object reads and writes, conditional
// creates, listing, and deletes.
type Bucket interface {
	// Put writes an object; with ifAbsent it fails with ErrExists instead of replacing one.
	Put(ctx context.Context, key string, data []byte, ifAbsent bool) error
	Get(ctx context.Context, key string) ([]byte, error)
	Head(ctx context.Context, key string) (Object, error)
	Delete(ctx context.Context, key string) error
	List(ctx context.Context, prefix string) ([]Object, error)
}

type S3 struct {
	cfg    Config
	client *http.Client
}

func New(cfg Config) (*S3, error) {
	if cfg.Name == "" || cfg.Region == "" || cfg.AccessKey == "" || cfg.SecretKey == "" {
		return nil, errors.New("bucket: name, region, and credentials are required")
	}
	if cfg.Endpoint != "" {
		u, err := url.Parse(cfg.Endpoint)
		if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
			return nil, fmt.Errorf("bucket endpoint %q: want https://host[:port]", cfg.Endpoint)
		}
	}
	return &S3{cfg: cfg, client: &http.Client{Timeout: 5 * time.Minute}}, nil
}

// url addresses AWS buckets virtual-hosted and others path-style, which every
// S3-compatible server supports.
func (s *S3) url(key string, query url.Values) *url.URL {
	var u *url.URL
	if s.cfg.Endpoint == "" {
		u = &url.URL{Scheme: "https", Host: fmt.Sprintf("%s.s3.%s.amazonaws.com", s.cfg.Name, s.cfg.Region), Path: "/" + key}
	} else {
		u, _ = url.Parse(strings.TrimRight(s.cfg.Endpoint, "/"))
		u.Path += "/" + s.cfg.Name + "/" + key
	}
	u.RawPath = escapePath(u.Path)
	u.RawQuery = canonicalQuery(query)
	return u
}

func (s *S3) do(ctx context.Context, method, key string, query url.Values, body []byte, header http.Header) (*http.Response, error) {
	u := s.url(key, query)
	req, err := http.NewRequestWithContext(ctx, method, u.String(), bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.ContentLength = int64(len(body))
	for k, v := range header {
		req.Header[k] = v
	}
	s.sign(req, u, body, time.Now().UTC())
	resp, err := s.client.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode/100 != 2 {
		defer resp.Body.Close()
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		switch resp.StatusCode {
		case http.StatusNotFound:
			return nil, fmt.Errorf("%s: %w", key, ErrNotFound)
		case http.StatusPreconditionFailed, http.StatusConflict:
			if header.Get("If-None-Match") != "" {
				return nil, fmt.Errorf("%s: %w", key, ErrExists)
			}
		}
		return nil, fmt.Errorf("bucket %s %s: %s: %s", method, key, resp.Status, errorMessage(msg))
	}
	return resp, nil
}

func errorMessage(body []byte) string {
	var e struct {
		Code    string `xml:"Code"`
		Message string `xml:"Message"`
	}
	if xml.Unmarshal(body, &e) == nil && e.Code != "" {
		return e.Code + ": " + e.Message
	}
	return strings.TrimSpace(string(body))
}

func (s *S3) Put(ctx context.Context, key string, data []byte, ifAbsent bool) error {
	h := http.Header{}
	if ifAbsent {
		h.Set("If-None-Match", "*")
	}
	resp, err := s.do(ctx, http.MethodPut, key, nil, data, h)
	if err != nil {
		return err
	}
	resp.Body.Close()
	return nil
}

func (s *S3) Get(ctx context.Context, key string) ([]byte, error) {
	resp, err := s.do(ctx, http.MethodGet, key, nil, nil, nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	return io.ReadAll(resp.Body)
}

func (s *S3) Head(ctx context.Context, key string) (Object, error) {
	resp, err := s.do(ctx, http.MethodHead, key, nil, nil, nil)
	if err != nil {
		return Object{}, err
	}
	resp.Body.Close()
	mod, _ := http.ParseTime(resp.Header.Get("Last-Modified"))
	return Object{Key: key, Size: resp.ContentLength, Modified: mod}, nil
}

func (s *S3) Delete(ctx context.Context, key string) error {
	resp, err := s.do(ctx, http.MethodDelete, key, nil, nil, nil)
	if errors.Is(err, ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	resp.Body.Close()
	return nil
}

func (s *S3) List(ctx context.Context, prefix string) ([]Object, error) {
	var out []Object
	token := ""
	for {
		q := url.Values{"list-type": {"2"}, "prefix": {prefix}}
		if token != "" {
			q.Set("continuation-token", token)
		}
		resp, err := s.do(ctx, http.MethodGet, "", q, nil, nil)
		if err != nil {
			return nil, err
		}
		var page struct {
			Contents []struct {
				Key          string    `xml:"Key"`
				Size         int64     `xml:"Size"`
				LastModified time.Time `xml:"LastModified"`
			} `xml:"Contents"`
			IsTruncated           bool   `xml:"IsTruncated"`
			NextContinuationToken string `xml:"NextContinuationToken"`
		}
		err = xml.NewDecoder(resp.Body).Decode(&page)
		resp.Body.Close()
		if err != nil {
			return nil, fmt.Errorf("bucket list: %w", err)
		}
		for _, c := range page.Contents {
			out = append(out, Object{Key: c.Key, Size: c.Size, Modified: c.LastModified})
		}
		if !page.IsTruncated || page.NextContinuationToken == "" {
			return out, nil
		}
		token = page.NextContinuationToken
	}
}

// sign adds an AWS Signature Version 4 to req.
func (s *S3) sign(req *http.Request, u *url.URL, body []byte, now time.Time) {
	payload := sha256.Sum256(body)
	payloadHex := hex.EncodeToString(payload[:])
	amzDate := now.Format("20060102T150405Z")
	date := now.Format("20060102")
	req.Header.Set("X-Amz-Date", amzDate)
	req.Header.Set("X-Amz-Content-Sha256", payloadHex)

	signed := []string{"host", "x-amz-content-sha256", "x-amz-date"}
	headers := map[string]string{"host": u.Host, "x-amz-content-sha256": payloadHex, "x-amz-date": amzDate}
	if v := req.Header.Get("If-None-Match"); v != "" {
		signed = append(signed, "if-none-match")
		headers["if-none-match"] = v
	}
	sort.Strings(signed)
	var canonHeaders strings.Builder
	for _, h := range signed {
		canonHeaders.WriteString(h + ":" + headers[h] + "\n")
	}
	canonical := strings.Join([]string{req.Method, u.EscapedPath(), u.RawQuery, canonHeaders.String(), strings.Join(signed, ";"), payloadHex}, "\n")
	scope := date + "/" + s.cfg.Region + "/s3/aws4_request"
	sum := sha256.Sum256([]byte(canonical))
	toSign := "AWS4-HMAC-SHA256\n" + amzDate + "\n" + scope + "\n" + hex.EncodeToString(sum[:])

	key := s.signingKey(date)
	sig := hex.EncodeToString(hmacSHA256(key, toSign))
	req.Header.Set("Authorization", fmt.Sprintf("AWS4-HMAC-SHA256 Credential=%s/%s, SignedHeaders=%s, Signature=%s",
		s.cfg.AccessKey, scope, strings.Join(signed, ";"), sig))
}

var keyCache sync.Map // date+secret → signing key

func (s *S3) signingKey(date string) []byte {
	ck := date + "/" + s.cfg.Region + "/" + s.cfg.SecretKey
	if k, ok := keyCache.Load(ck); ok {
		return k.([]byte)
	}
	k := hmacSHA256([]byte("AWS4"+s.cfg.SecretKey), date)
	k = hmacSHA256(k, s.cfg.Region)
	k = hmacSHA256(k, "s3")
	k = hmacSHA256(k, "aws4_request")
	keyCache.Store(ck, k)
	return k
}

func hmacSHA256(key []byte, data string) []byte {
	h := hmac.New(sha256.New, key)
	h.Write([]byte(data))
	return h.Sum(nil)
}

// escapePath encodes each path segment as SigV4 requires: everything but
// unreserved characters.
func escapePath(p string) string {
	segs := strings.Split(p, "/")
	for i, s := range segs {
		segs[i] = escape(s)
	}
	return strings.Join(segs, "/")
}

func canonicalQuery(q url.Values) string {
	if len(q) == 0 {
		return ""
	}
	keys := make([]string, 0, len(q))
	for k := range q {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var parts []string
	for _, k := range keys {
		vs := append([]string(nil), q[k]...)
		sort.Strings(vs)
		for _, v := range vs {
			parts = append(parts, escape(k)+"="+escape(v))
		}
	}
	return strings.Join(parts, "&")
}

func escape(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if 'A' <= c && c <= 'Z' || 'a' <= c && c <= 'z' || '0' <= c && c <= '9' || c == '-' || c == '_' || c == '.' || c == '~' {
			b.WriteByte(c)
		} else {
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String()
}
