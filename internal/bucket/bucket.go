// Package bucket talks to the one S3-compatible bucket that holds all durable state.
package bucket

import (
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/mouizahmed/hutch/internal/sigv4"
)

// Config locates the bucket. An empty Endpoint means AWS S3 in Region.
type Config struct {
	Endpoint  string `json:"endpoint,omitempty"`
	Region    string `json:"region"`
	Name      string `json:"name"`
	AccessKey string `json:"access_key,omitempty"`
	SecretKey string `json:"secret_key,omitempty"`
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
	u.RawPath = sigv4.EscapePath(u.Path)
	u.RawQuery = sigv4.Query(query)
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
	creds := sigv4.Credentials{AccessKey: s.cfg.AccessKey, SecretKey: s.cfg.SecretKey}
	sigv4.Sign(req, body, "s3", s.cfg.Region, creds, time.Now())
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

// Create makes the bucket; S3 blocks public access to new buckets by default.
func (s *S3) Create(ctx context.Context) error {
	var body []byte
	if s.cfg.Region != "us-east-1" {
		body = []byte(`<CreateBucketConfiguration><LocationConstraint>` + s.cfg.Region + `</LocationConstraint></CreateBucketConfiguration>`)
	}
	resp, err := s.do(ctx, http.MethodPut, "", nil, body, nil)
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
