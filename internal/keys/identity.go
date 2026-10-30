package keys

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// IdentityHeader is added outside the box, only toward an explicitly named host.
const IdentityHeader = "X-Hutch-Identity"

const identityTTL = 5 * time.Minute

// PublicIdentity is the machine's issuer and public JSON Web Key Set. Services
// pin these once; neither a signing key nor a public listener is needed in a box.
type PublicIdentity struct {
	Issuer string `json:"issuer"`
	Keys   []JWK  `json:"keys"`
}

type JWK struct {
	Kty string `json:"kty"`
	Crv string `json:"crv"`
	Alg string `json:"alg"`
	Use string `json:"use"`
	Kid string `json:"kid"`
	X   string `json:"x"`
}

func IdentityPublic(seed []byte) (PublicIdentity, error) {
	if len(seed) != ed25519.SeedSize {
		return PublicIdentity{}, fmt.Errorf("invalid box identity signing seed")
	}
	pub := ed25519.NewKeyFromSeed(seed).Public().(ed25519.PublicKey)
	id := sha256.Sum256(pub)
	kid := base64.RawURLEncoding.EncodeToString(id[:])
	return PublicIdentity{Issuer: "urn:hutch:" + kid, Keys: []JWK{{Kty: "OKP", Crv: "Ed25519", Alg: "EdDSA", Use: "sig", Kid: kid, X: base64.RawURLEncoding.EncodeToString(pub)}}}, nil
}

func identityToken(seed []byte, box, project, host string, now time.Time) (string, error) {
	pub, err := IdentityPublic(seed)
	if err != nil {
		return "", err
	}
	nonce := make([]byte, 16)
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	header, _ := json.Marshal(map[string]string{"alg": "EdDSA", "typ": "JWT", "kid": pub.Keys[0].Kid})
	claims, _ := json.Marshal(map[string]any{
		"iss": pub.Issuer, "sub": box, "project": project, "aud": host,
		"iat": now.Unix(), "nbf": now.Unix(), "exp": now.Add(identityTTL).Unix(),
		"jti": base64.RawURLEncoding.EncodeToString(nonce),
	})
	body := base64.RawURLEncoding.EncodeToString(header) + "." + base64.RawURLEncoding.EncodeToString(claims)
	sig := ed25519.Sign(ed25519.NewKeyFromSeed(seed), []byte(body))
	return body + "." + base64.RawURLEncoding.EncodeToString(sig), nil
}

func (p *Proxy) identifies(box, host string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	_, exists := p.table.Projects[box]
	return exists && len(p.table.IdentitySeed) == ed25519.SeedSize && identityHost(p.table.IdentityHosts, host)
}

func identityHost(hosts []string, host string) bool {
	for _, allowed := range hosts {
		if strings.EqualFold(strings.TrimSuffix(allowed, "."), strings.TrimSuffix(host, ".")) {
			return true
		}
	}
	return false
}

// addIdentity removes a box's attempted identity before signing its actual one.
// It never replaces the service's own Authorization header.
func (p *Proxy) addIdentity(h http.Header, box, host string, now time.Time) error {
	h.Del(IdentityHeader)
	p.mu.Lock()
	seed, project := p.table.IdentitySeed, p.table.Projects[box]
	_, exists := p.table.Projects[box]
	enabled := exists && len(seed) == ed25519.SeedSize && identityHost(p.table.IdentityHosts, host)
	p.mu.Unlock()
	if !enabled {
		return nil
	}
	token, err := identityToken(seed, box, project, host, now)
	if err == nil {
		h.Set(IdentityHeader, token)
	}
	return err
}
