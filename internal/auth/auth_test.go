package auth

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func b64(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

func sign(t *testing.T, key *rsa.PrivateKey, kid string, claims map[string]any) string {
	t.Helper()
	h, _ := json.Marshal(map[string]string{"alg": "RS256", "kid": kid, "typ": "JWT"})
	c, _ := json.Marshal(claims)
	in := b64(h) + "." + b64(c)
	sum := sha256.Sum256([]byte(in))
	sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, sum[:])
	if err != nil {
		t.Fatal(err)
	}
	return in + "." + b64(sig)
}

func setup(t *testing.T) (*Verifier, *rsa.PrivateKey) {
	key, _ := rsa.GenerateKey(rand.Reader, 2048)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"keys": []map[string]string{{
			"kid": "k1", "kty": "RSA", "alg": "RS256",
			"n": b64(key.N.Bytes()), "e": b64(big.NewInt(int64(key.E)).Bytes()),
		}}})
	}))
	t.Cleanup(srv.Close)
	return &Verifier{TeamDomain: "team.cloudflareaccess.com", AUD: "aud1", CertsURL: srv.URL}, key
}

func good() map[string]any {
	return map[string]any{
		"aud": []string{"aud1"}, "iss": "https://team.cloudflareaccess.com",
		"exp": time.Now().Add(time.Hour).Unix(), "email": "Mom@Example.com",
	}
}

func TestValidToken(t *testing.T) {
	v, key := setup(t)
	email, err := v.Email(sign(t, key, "k1", good()))
	if err != nil || email != "mom@example.com" {
		t.Fatalf("got %q, %v", email, err)
	}
}

func TestRejects(t *testing.T) {
	v, key := setup(t)
	other, _ := rsa.GenerateKey(rand.Reader, 2048)
	mod := func(k string, val any) map[string]any { c := good(); c[k] = val; return c }
	cases := map[string]string{
		"wrong key":      sign(t, other, "k1", good()),
		"unknown kid":    sign(t, key, "k2", good()),
		"expired":        sign(t, key, "k1", mod("exp", time.Now().Add(-time.Hour).Unix())),
		"wrong audience": sign(t, key, "k1", mod("aud", "someone-else")),
		"wrong issuer":   sign(t, key, "k1", mod("iss", "https://evil.cloudflareaccess.com")),
		"no email":       sign(t, key, "k1", mod("email", "")),
		"garbage":        "a.b.c",
		"empty":          "",
	}
	for name, tok := range cases {
		if _, err := v.Email(tok); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestAlgNone(t *testing.T) {
	v, _ := setup(t)
	h, _ := json.Marshal(map[string]string{"alg": "none", "kid": "k1"})
	c, _ := json.Marshal(good())
	if _, err := v.Email(b64(h) + "." + b64(c) + "."); err == nil {
		t.Fatal("alg none accepted")
	}
}
