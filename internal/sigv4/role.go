package sigv4

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Role reads the instance role's temporary credentials from EC2 instance
// metadata (IMDSv2), renewing them before they expire.
type Role struct {
	mu      sync.Mutex
	creds   Credentials
	expires time.Time
	client  http.Client
}

func (r *Role) Credentials(ctx context.Context) (Credentials, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.creds.AccessKey != "" && time.Until(r.expires) > 5*time.Minute {
		return r.creds, nil
	}
	r.client.Timeout = 3 * time.Second
	req, _ := http.NewRequestWithContext(ctx, http.MethodPut, "http://169.254.169.254/latest/api/token", nil)
	req.Header.Set("X-aws-ec2-metadata-token-ttl-seconds", "300")
	resp, err := r.client.Do(req)
	if err != nil {
		return Credentials{}, fmt.Errorf("instance role: %w", err)
	}
	token, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	get := func(path string) ([]byte, error) {
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "http://169.254.169.254/latest/meta-data/iam/security-credentials/"+path, nil)
		req.Header.Set("X-aws-ec2-metadata-token", string(token))
		resp, err := r.client.Do(req)
		if err != nil {
			return nil, err
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("instance role: %s (does the instance have one?)", resp.Status)
		}
		return io.ReadAll(resp.Body)
	}
	name, err := get("")
	if err != nil {
		return Credentials{}, err
	}
	data, err := get(strings.TrimSpace(strings.SplitN(string(name), "\n", 2)[0]))
	if err != nil {
		return Credentials{}, err
	}
	var c struct {
		AccessKeyId, SecretAccessKey, Token string
		Expiration                          time.Time
	}
	if err := json.Unmarshal(data, &c); err != nil {
		return Credentials{}, fmt.Errorf("instance role: %w", err)
	}
	r.creds = Credentials{AccessKey: c.AccessKeyId, SecretKey: c.SecretAccessKey, Token: c.Token}
	r.expires = c.Expiration
	return r.creds, nil
}
