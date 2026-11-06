// Package auth checks who is signed in. Cloudflare Access signs every request it lets through.
//
// Access puts a signed JWT in the Cf-Access-Jwt-Assertion header (and the CF_Authorization cookie).
// We check it against the team's public keys and the application's AUD tag, then use its email.
// The app only listens on 127.0.0.1, so the tunnel is the only way in, but we still verify the
// token so a mistake in the network setup can't let unsigned requests through.
package auth

import (
	"crypto"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Verifier checks Cloudflare Access tokens.
type Verifier struct {
	TeamDomain string
	AUD        string
	Client     *http.Client
	Now        func() time.Time
	// CertsURL overrides https://<team>/cdn-cgi/access/certs (for tests).
	CertsURL string

	mu      sync.Mutex
	keys    map[string]*rsa.PublicKey
	fetched time.Time
}

// ErrNoToken means the request carried no Access token.
var ErrNoToken = errors.New("no sign-in token")

// Token returns the Access token in a request, if any.
func Token(r *http.Request) string {
	if t := r.Header.Get("Cf-Access-Jwt-Assertion"); t != "" {
		return t
	}
	if c, err := r.Cookie("CF_Authorization"); err == nil {
		return c.Value
	}
	return ""
}

type header struct {
	Alg string `json:"alg"`
	Kid string `json:"kid"`
}

type claims struct {
	Aud   audience `json:"aud"`
	Iss   string   `json:"iss"`
	Exp   int64    `json:"exp"`
	Nbf   int64    `json:"nbf"`
	Email string   `json:"email"`
}

// audience accepts a string or a list of strings.
type audience []string

func (a *audience) UnmarshalJSON(b []byte) error {
	var one string
	if json.Unmarshal(b, &one) == nil {
		*a = audience{one}
		return nil
	}
	var many []string
	if err := json.Unmarshal(b, &many); err != nil {
		return err
	}
	*a = many
	return nil
}

func (v *Verifier) now() time.Time {
	if v.Now != nil {
		return v.Now()
	}
	return time.Now()
}

// Email verifies a token and returns the lowercased email inside it.
func (v *Verifier) Email(token string) (string, error) {
	if token == "" {
		return "", ErrNoToken
	}
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return "", errors.New("malformed token")
	}
	var h header
	if err := decodePart(parts[0], &h); err != nil {
		return "", fmt.Errorf("bad header: %w", err)
	}
	if h.Alg != "RS256" {
		return "", fmt.Errorf("unexpected algorithm %q", h.Alg)
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return "", errors.New("bad signature encoding")
	}
	key, err := v.key(h.Kid)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if err := rsa.VerifyPKCS1v15(key, crypto.SHA256, sum[:], sig); err != nil {
		return "", errors.New("bad signature")
	}
	var c claims
	if err := decodePart(parts[1], &c); err != nil {
		return "", fmt.Errorf("bad claims: %w", err)
	}
	now := v.now().Unix()
	const leeway = 60
	if c.Exp == 0 || now > c.Exp+leeway {
		return "", errors.New("token expired")
	}
	if c.Nbf != 0 && now+leeway < c.Nbf {
		return "", errors.New("token not valid yet")
	}
	if c.Iss != "https://"+v.TeamDomain {
		return "", errors.New("wrong issuer")
	}
	okAud := false
	for _, a := range c.Aud {
		if a == v.AUD {
			okAud = true
		}
	}
	if !okAud {
		return "", errors.New("wrong audience")
	}
	if c.Email == "" {
		return "", errors.New("token has no email")
	}
	return strings.ToLower(c.Email), nil
}

func decodePart(s string, into any) error {
	b, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, into)
}

// key returns the public key for kid, fetching the team's keys at most once a minute when unknown.
func (v *Verifier) key(kid string) (*rsa.PublicKey, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if k, ok := v.keys[kid]; ok && v.now().Sub(v.fetched) < time.Hour {
		return k, nil
	}
	if v.now().Sub(v.fetched) > time.Minute || v.keys == nil {
		if err := v.fetch(); err != nil {
			if k, ok := v.keys[kid]; ok {
				return k, nil // keep using a known key if Cloudflare is briefly unreachable
			}
			return nil, fmt.Errorf("can't load Cloudflare's keys: %w", err)
		}
	}
	if k, ok := v.keys[kid]; ok {
		return k, nil
	}
	return nil, errors.New("unknown signing key")
}

func (v *Verifier) fetch() error {
	url := v.CertsURL
	if url == "" {
		url = "https://" + v.TeamDomain + "/cdn-cgi/access/certs"
	}
	client := v.Client
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}
	resp, err := client.Get(url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("status %d", resp.StatusCode)
	}
	var set struct {
		Keys []struct {
			Kid string `json:"kid"`
			Kty string `json:"kty"`
			N   string `json:"n"`
			E   string `json:"e"`
		} `json:"keys"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&set); err != nil {
		return err
	}
	keys := map[string]*rsa.PublicKey{}
	for _, k := range set.Keys {
		if k.Kty != "RSA" {
			continue
		}
		n, err1 := base64.RawURLEncoding.DecodeString(k.N)
		e, err2 := base64.RawURLEncoding.DecodeString(k.E)
		if err1 != nil || err2 != nil {
			continue
		}
		keys[k.Kid] = &rsa.PublicKey{N: new(big.Int).SetBytes(n), E: int(new(big.Int).SetBytes(e).Int64())}
	}
	if len(keys) == 0 {
		return errors.New("no RSA keys")
	}
	v.keys, v.fetched = keys, v.now()
	return nil
}
