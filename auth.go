package acyclic

// SMART Backend Services (private_key_jwt, RFC 7523) and client_secret_basic
// token clients. Bring your own credentials: nothing is stored or persisted;
// tokens live in memory only.

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha512"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"math"
	"math/big"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// AuthError is a failed token request. Code is the OAuth error code only;
// error_description is dropped because it can echo request data.
type AuthError struct {
	Status int
	Code   string
}

func (e *AuthError) Error() string {
	if e.Code != "" {
		return fmt.Sprintf("token request failed: %d %s", e.Status, e.Code)
	}
	return fmt.Sprintf("token request failed: %d", e.Status)
}

func (e *AuthError) ErrorCode() string { return e.Code }

// ParsePrivateKey parses a PEM private key for alg RS384 or ES384: PKCS#8
// ("PRIVATE KEY"), PKCS#1 ("RSA PRIVATE KEY") or SEC1 ("EC PRIVATE KEY").
func ParsePrivateKey(pemText, alg string) (crypto.Signer, error) {
	if alg == "" {
		alg = "RS384"
	}
	if alg != "RS384" && alg != "ES384" {
		return nil, fmt.Errorf("unsupported alg %s; use RS384 or ES384", alg)
	}
	block, _ := pem.Decode([]byte(strings.TrimSpace(pemText)))
	if block == nil {
		return nil, errors.New("privateKey is not a PEM block")
	}
	var key any
	var err error
	switch block.Type {
	case "RSA PRIVATE KEY":
		key, err = x509.ParsePKCS1PrivateKey(block.Bytes)
	case "EC PRIVATE KEY":
		key, err = x509.ParseECPrivateKey(block.Bytes)
	default:
		key, err = x509.ParsePKCS8PrivateKey(block.Bytes)
	}
	if err != nil {
		return nil, fmt.Errorf("parse private key: %w", err)
	}
	return checkKey(key, alg)
}

func checkKey(key any, alg string) (crypto.Signer, error) {
	switch k := key.(type) {
	case *rsa.PrivateKey:
		if alg != "RS384" {
			return nil, fmt.Errorf("alg %s needs an EC P-384 key, got RSA", alg)
		}
		return k, nil
	case *ecdsa.PrivateKey:
		if alg != "ES384" {
			return nil, fmt.Errorf("alg %s needs an RSA key, got EC", alg)
		}
		if k.Curve != elliptic.P384() {
			return nil, errors.New("ES384 needs a P-384 key")
		}
		return k, nil
	}
	return nil, fmt.Errorf("unsupported private key type %T", key)
}

func b64url(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

// ClientAssertion returns a signed RFC 7523 client assertion as SMART Backend
// Services requires: iss = sub = clientID, aud = tokenURL, 240 s lifetime.
func ClientAssertion(clientID, tokenURL string, key crypto.Signer, alg, kid string) (string, error) {
	if alg == "" {
		alg = "RS384"
	}
	now := time.Now().Unix()
	header := struct {
		Alg string `json:"alg"`
		Typ string `json:"typ"`
		Kid string `json:"kid,omitempty"`
	}{alg, "JWT", kid}
	claims := struct {
		Iss string `json:"iss"`
		Sub string `json:"sub"`
		Aud string `json:"aud"`
		Jti string `json:"jti"`
		Iat int64  `json:"iat"`
		Nbf int64  `json:"nbf"`
		Exp int64  `json:"exp"`
	}{clientID, clientID, tokenURL, randomID(""), now, now, now + 240}
	input := b64url([]byte(marshalJSON(header))) + "." + b64url([]byte(marshalJSON(claims)))
	digest := sha512.Sum384([]byte(input))
	var sig []byte
	switch k := key.(type) {
	case *rsa.PrivateKey:
		if alg != "RS384" {
			return "", fmt.Errorf("alg %s does not match an RSA key", alg)
		}
		s, err := rsa.SignPKCS1v15(rand.Reader, k, crypto.SHA384, digest[:])
		if err != nil {
			return "", err
		}
		sig = s
	case *ecdsa.PrivateKey:
		if alg != "ES384" {
			return "", fmt.Errorf("alg %s does not match an EC key", alg)
		}
		r, s, err := ecdsa.Sign(rand.Reader, k, digest[:])
		if err != nil {
			return "", err
		}
		sig = make([]byte, 96) // JWS wants raw r || s, 48 bytes each
		r.FillBytes(sig[:48])
		s.FillBytes(sig[48:])
	default:
		return "", fmt.Errorf("unsupported key type %T", key)
	}
	return input + "." + b64url(sig), nil
}

// KeyPair is a freshly generated signing key with its public JWK, for the
// sandbox or for writing a JWKS file to register with a vendor.
type KeyPair struct {
	PrivateKey    crypto.Signer
	PrivateKeyPEM string // PKCS#8
	PublicJWK     map[string]any
	KeyID         string
	Alg           string
}

// GenerateKeyPair makes an RS384 (RSA 2048) or ES384 (P-384) key pair.
func GenerateKeyPair(alg string) (*KeyPair, error) {
	if alg == "" {
		alg = "RS384"
	}
	var key crypto.Signer
	var err error
	switch alg {
	case "RS384":
		key, err = rsa.GenerateKey(rand.Reader, 2048)
	case "ES384":
		key, err = ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
	default:
		return nil, fmt.Errorf("unsupported alg %s; use RS384 or ES384", alg)
	}
	if err != nil {
		return nil, err
	}
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, err
	}
	kid := randomID("")[:16]
	return &KeyPair{
		PrivateKey:    key,
		PrivateKeyPEM: string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})),
		PublicJWK:     PublicJWK(key.Public(), alg, kid),
		KeyID:         kid,
		Alg:           alg,
	}, nil
}

// PublicJWK renders an RSA or P-384 public key as a JWK with use "sig".
func PublicJWK(pub crypto.PublicKey, alg, kid string) map[string]any {
	j := map[string]any{"alg": alg, "use": "sig"}
	if kid != "" {
		j["kid"] = kid
	}
	switch k := pub.(type) {
	case *rsa.PublicKey:
		j["kty"] = "RSA"
		j["n"] = b64url(k.N.Bytes())
		j["e"] = b64url(big.NewInt(int64(k.E)).Bytes())
	case *ecdsa.PublicKey:
		size := (k.Curve.Params().BitSize + 7) / 8
		x, y := make([]byte, size), make([]byte, size)
		k.X.FillBytes(x)
		k.Y.FillBytes(y)
		j["kty"], j["crv"], j["x"], j["y"] = "EC", "P-384", b64url(x), b64url(y)
	}
	return j
}

// --- token source ---

type tokenConfig struct {
	method       string // private_key_jwt | client_secret_basic
	clientID     string
	clientSecret string
	key          crypto.Signer
	keyErr       error
	alg          string
	kid          string
	tokenURL     string
	scopes       []string
	client       *http.Client
	tel          guard
	vendor       string
}

type tokenCall struct {
	done  chan struct{}
	token string
	err   error
}

// tokenSource caches one token in memory, refreshes it when less than 60 s
// remain, and lets only one refresh run at a time (single flight).
type tokenSource struct {
	cfg       tokenConfig
	mu        sync.Mutex
	token     string
	expiresAt time.Time
	inflight  *tokenCall
}

func (ts *tokenSource) get(ctx context.Context, force bool) (string, error) {
	ts.mu.Lock()
	if !force && ts.token != "" && time.Until(ts.expiresAt) > 60*time.Second {
		t := ts.token
		ts.mu.Unlock()
		return t, nil
	}
	call := ts.inflight
	if call == nil {
		call = &tokenCall{done: make(chan struct{})}
		ts.inflight = call
		// The shared request outlives any one caller's cancellation.
		go func(c context.Context) {
			tok, exp, err := ts.request(c)
			ts.mu.Lock()
			if err == nil {
				ts.token, ts.expiresAt = tok, exp
			}
			ts.inflight = nil
			ts.mu.Unlock()
			call.token, call.err = tok, err
			close(call.done)
		}(context.WithoutCancel(ctx))
	}
	ts.mu.Unlock()
	select {
	case <-call.done:
		return call.token, call.err
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

// expiresIn reads expires_in as a number or a numeric string, as
// JavaScript's Number(x) || 300 does.
func expiresIn(v any) float64 {
	var f float64
	switch x := v.(type) {
	case float64:
		f = x
	case string:
		s := strings.TrimSpace(x)
		if s == "" {
			return 300
		}
		p, err := strconv.ParseFloat(s, 64)
		if err != nil {
			return 300
		}
		f = p
	case bool:
		if x {
			f = 1
		}
	}
	if f == 0 || math.IsNaN(f) || math.IsInf(f, 0) {
		return 300
	}
	return f
}

func (ts *tokenSource) request(ctx context.Context) (string, time.Time, error) {
	c := ts.cfg
	form := url.Values{"grant_type": {"client_credentials"}}
	if len(c.scopes) > 0 {
		form.Set("scope", strings.Join(c.scopes, " "))
	}
	headers := http.Header{}
	headers.Set("Content-Type", "application/x-www-form-urlencoded")
	headers.Set("Accept", "application/json")
	if c.method == "client_secret_basic" {
		if c.clientSecret == "" {
			return "", time.Time{}, errors.New("clientSecret is required for client_secret_basic")
		}
		creds := encodeURIComponent(c.clientID) + ":" + encodeURIComponent(c.clientSecret)
		headers.Set("Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte(creds)))
	} else {
		if c.keyErr != nil {
			return "", time.Time{}, c.keyErr
		}
		if c.key == nil {
			return "", time.Time{}, errors.New("privateKey is required for private_key_jwt")
		}
		a, err := ClientAssertion(c.clientID, c.tokenURL, c.key, c.alg, c.kid)
		if err != nil {
			return "", time.Time{}, err
		}
		form.Set("client_assertion_type", "urn:ietf:params:oauth:client-assertion-type:jwt-bearer")
		form.Set("client_assertion", a)
	}

	u, err := url.Parse(c.tokenURL)
	if err != nil {
		return "", time.Time{}, errors.New("tokenUrl is not a valid URL")
	}
	sctx, span := c.tel.startSpan(ctx, "POST token", map[string]any{
		"http.request.method": "POST", "server.address": u.Hostname(), "acyclic.vendor": c.vendor,
	})
	tok, exp, err := ts.post(sctx, u, headers, form, span)
	if err != nil {
		span.setAttributes(map[string]any{"error.type": errorType(err)})
		span.end(err)
		return "", time.Time{}, err
	}
	span.end(nil)
	c.tel.log(sctx, "info", "token.refresh", map[string]any{"acyclic.vendor": c.vendor})
	return tok, exp, nil
}

func (ts *tokenSource) post(ctx context.Context, u *url.URL, headers http.Header, form url.Values, span guardSpan) (string, time.Time, error) {
	req, err := http.NewRequestWithContext(ctx, "POST", u.String(), strings.NewReader(form.Encode()))
	if err != nil {
		return "", time.Time{}, err
	}
	req.Header = headers
	res, err := ts.cfg.client.Do(req)
	if err != nil {
		return "", time.Time{}, transportErr("token", err)
	}
	defer res.Body.Close()
	span.setAttributes(map[string]any{"http.response.status_code": res.StatusCode})
	raw, _ := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	var body map[string]any
	_ = json.Unmarshal(raw, &body)
	token := str(body["access_token"])
	if res.StatusCode < 200 || res.StatusCode > 299 || token == "" {
		return "", time.Time{}, &AuthError{Status: res.StatusCode, Code: str(body["error"])}
	}
	ttl := expiresIn(body["expires_in"])
	return token, time.Now().Add(time.Duration(ttl * float64(time.Second))), nil
}
