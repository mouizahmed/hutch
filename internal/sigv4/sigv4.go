// Package sigv4 signs requests to AWS and S3-compatible APIs (Signature Version 4).
package sigv4

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"
)

type Credentials struct {
	AccessKey string `json:"access_key"`
	SecretKey string `json:"secret_key"`
	// Token is set for temporary credentials, like an instance role's.
	Token string `json:"token,omitempty"`
}

// Sign adds the date, payload hash, and authorization headers to req. The URL's
// path must already be escaped as the service expects (EscapedPath is signed), and
// its query in canonical order (see Query).
func Sign(req *http.Request, body []byte, service, region string, c Credentials, now time.Time) {
	payload := sha256.Sum256(body)
	payloadHex := hex.EncodeToString(payload[:])
	amzDate := now.UTC().Format("20060102T150405Z")
	date := amzDate[:8]
	req.Header.Set("X-Amz-Date", amzDate)
	req.Header.Set("X-Amz-Content-Sha256", payloadHex)
	if c.Token != "" {
		req.Header.Set("X-Amz-Security-Token", c.Token)
	}

	headers := map[string]string{"host": req.URL.Host}
	for k, v := range req.Header {
		lk := strings.ToLower(k)
		if lk == "content-type" || lk == "if-none-match" || strings.HasPrefix(lk, "x-amz-") {
			headers[lk] = strings.TrimSpace(strings.Join(v, ","))
		}
	}
	signed := make([]string, 0, len(headers))
	for h := range headers {
		signed = append(signed, h)
	}
	sort.Strings(signed)
	var canonHeaders strings.Builder
	for _, h := range signed {
		canonHeaders.WriteString(h + ":" + headers[h] + "\n")
	}
	path := req.URL.EscapedPath()
	if path == "" {
		path = "/"
	}
	canonical := strings.Join([]string{req.Method, path, req.URL.RawQuery, canonHeaders.String(), strings.Join(signed, ";"), payloadHex}, "\n")
	scope := date + "/" + region + "/" + service + "/aws4_request"
	sum := sha256.Sum256([]byte(canonical))
	toSign := "AWS4-HMAC-SHA256\n" + amzDate + "\n" + scope + "\n" + hex.EncodeToString(sum[:])
	sig := hex.EncodeToString(hmacSHA256(signingKey(c.SecretKey, date, region, service), toSign))
	req.Header.Set("Authorization", fmt.Sprintf("AWS4-HMAC-SHA256 Credential=%s/%s, SignedHeaders=%s, Signature=%s",
		c.AccessKey, scope, strings.Join(signed, ";"), sig))
}

var keyCache sync.Map

func signingKey(secret, date, region, service string) []byte {
	ck := strings.Join([]string{date, region, service, secret}, "/")
	if k, ok := keyCache.Load(ck); ok {
		return k.([]byte)
	}
	k := hmacSHA256([]byte("AWS4"+secret), date)
	k = hmacSHA256(k, region)
	k = hmacSHA256(k, service)
	k = hmacSHA256(k, "aws4_request")
	keyCache.Store(ck, k)
	return k
}

func hmacSHA256(key []byte, data string) []byte {
	h := hmac.New(sha256.New, key)
	h.Write([]byte(data))
	return h.Sum(nil)
}

// Query encodes parameters in SigV4's canonical form: sorted, every byte but the
// unreserved ones escaped.
func Query(q map[string][]string) string {
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
			parts = append(parts, Escape(k)+"="+Escape(v))
		}
	}
	return strings.Join(parts, "&")
}

// EscapePath escapes each segment of a path.
func EscapePath(p string) string {
	segs := strings.Split(p, "/")
	for i, s := range segs {
		segs[i] = Escape(s)
	}
	return strings.Join(segs, "/")
}

func Escape(s string) string {
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
