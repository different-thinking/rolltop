// File overview: Table-driven tests for OIDC id_token validation — signature,
// issuer/audience/expiry, alg confusion, and nonce — against a JWKS served from
// a local test server.

package main

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func b64url(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

// encodeExponent renders an RSA public exponent as the big-endian bytes a JWK
// carries (65537 -> "AQAB").
func encodeExponent(e int) []byte {
	var out []byte
	for e > 0 {
		out = append([]byte{byte(e & 0xff)}, out...)
		e >>= 8
	}
	if len(out) == 0 {
		out = []byte{0}
	}
	return out
}

// jwksServer serves a single RSA verification key under the given kid.
func jwksServer(t *testing.T, kid string, pub *rsa.PublicKey) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"keys": []map[string]any{{
				"kty": "RSA",
				"kid": kid,
				"use": "sig",
				"alg": "RS256",
				"n":   b64url(pub.N.Bytes()),
				"e":   b64url(encodeExponent(pub.E)),
			}},
		})
	}))
	t.Cleanup(srv.Close)
	return srv
}

// signToken builds a JWT with the given header alg/kid and claims. When key is
// nil the signature bytes are left empty, which is enough for cases rejected
// before the signature is checked (a non-RS256 alg).
func signToken(t *testing.T, alg, kid string, claims map[string]any, key *rsa.PrivateKey) string {
	t.Helper()
	header := map[string]any{"alg": alg, "typ": "JWT"}
	if kid != "" {
		header["kid"] = kid
	}
	hb, err := json.Marshal(header)
	if err != nil {
		t.Fatal(err)
	}
	cb, err := json.Marshal(claims)
	if err != nil {
		t.Fatal(err)
	}
	signingInput := b64url(hb) + "." + b64url(cb)
	sig := []byte{}
	if key != nil {
		sum := sha256.Sum256([]byte(signingInput))
		sig, err = rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, sum[:])
		if err != nil {
			t.Fatal(err)
		}
	}
	return signingInput + "." + b64url(sig)
}

func TestValidateIDToken(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	wrongKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	const kid = "test-key-1"
	srv := jwksServer(t, kid, &key.PublicKey)

	const (
		issuer   = "https://issuer.test"
		clientID = "client-123"
		nonce    = "nonce-abc"
	)
	baseClaims := func() map[string]any {
		return map[string]any{
			"iss":            issuer,
			"sub":            "user-1",
			"aud":            clientID,
			"exp":            time.Now().Add(time.Hour).Unix(),
			"nonce":          nonce,
			"email":          "user@example.test",
			"email_verified": true,
		}
	}
	with := func(mutate func(map[string]any)) map[string]any {
		c := baseClaims()
		mutate(c)
		return c
	}

	cases := []struct {
		name    string
		token   func() string
		wantErr bool
	}{
		{
			name:    "valid token",
			token:   func() string { return signToken(t, "RS256", kid, baseClaims(), key) },
			wantErr: false,
		},
		{
			name: "audience as array containing the client is accepted",
			token: func() string {
				return signToken(t, "RS256", kid, with(func(c map[string]any) { c["aud"] = []string{"other", clientID} }), key)
			},
			wantErr: false,
		},
		{
			name:    "signature from a different key is rejected",
			token:   func() string { return signToken(t, "RS256", kid, baseClaims(), wrongKey) },
			wantErr: true,
		},
		{
			name: "wrong issuer is rejected",
			token: func() string {
				return signToken(t, "RS256", kid, with(func(c map[string]any) { c["iss"] = "https://evil.test" }), key)
			},
			wantErr: true,
		},
		{
			name: "wrong audience is rejected",
			token: func() string {
				return signToken(t, "RS256", kid, with(func(c map[string]any) { c["aud"] = "someone-else" }), key)
			},
			wantErr: true,
		},
		{
			name: "audience array without the client is rejected",
			token: func() string {
				return signToken(t, "RS256", kid, with(func(c map[string]any) { c["aud"] = []string{"a", "b"} }), key)
			},
			wantErr: true,
		},
		{
			name: "expired token is rejected",
			token: func() string {
				return signToken(t, "RS256", kid, with(func(c map[string]any) { c["exp"] = time.Now().Add(-15 * time.Minute).Unix() }), key)
			},
			wantErr: true,
		},
		{
			name: "missing exp is rejected",
			token: func() string {
				return signToken(t, "RS256", kid, with(func(c map[string]any) { delete(c, "exp") }), key)
			},
			wantErr: true,
		},
		{
			name:    "alg confusion HS256 is rejected before signature check",
			token:   func() string { return signToken(t, "HS256", kid, baseClaims(), nil) },
			wantErr: true,
		},
		{
			name:    "alg none is rejected",
			token:   func() string { return signToken(t, "none", kid, baseClaims(), nil) },
			wantErr: true,
		},
		{
			name: "wrong nonce is rejected",
			token: func() string {
				return signToken(t, "RS256", kid, with(func(c map[string]any) { c["nonce"] = "different" }), key)
			},
			wantErr: true,
		},
		{
			name:    "malformed token is rejected",
			token:   func() string { return "not.a-jwt" },
			wantErr: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			claims, err := validateIDToken(context.Background(), tc.token(), srv.URL, issuer, clientID, nonce)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("validateIDToken accepted a token it should reject; claims=%+v", claims)
				}
				return
			}
			if err != nil {
				t.Fatalf("validateIDToken rejected a valid token: %v", err)
			}
			if claims.Email != "user@example.test" {
				t.Fatalf("claims.Email = %q, want the token's email", claims.Email)
			}
		})
	}
}

func TestAudienceContains(t *testing.T) {
	cases := []struct {
		name     string
		raw      string
		clientID string
		want     bool
	}{
		{name: "string match", raw: `"client-1"`, clientID: "client-1", want: true},
		{name: "string mismatch", raw: `"client-1"`, clientID: "client-2", want: false},
		{name: "array contains", raw: `["a","client-1","b"]`, clientID: "client-1", want: true},
		{name: "array missing", raw: `["a","b"]`, clientID: "client-1", want: false},
		{name: "empty", raw: `""`, clientID: "client-1", want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := audienceContains(json.RawMessage(tc.raw), tc.clientID); got != tc.want {
				t.Fatalf("audienceContains(%s, %q) = %v, want %v", tc.raw, tc.clientID, got, tc.want)
			}
		})
	}
}
