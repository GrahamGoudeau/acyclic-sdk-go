// Package acyclic is an EHR agent SDK: SMART Backend Services auth, a FHIR
// R4 client with vendor profiles (Epic, Oracle Health, athenaOne, MEDITECH,
// eClinicalWorks), thirteen patient-bound tools that return small
// projections, an agent loop with hard budgets, and telemetry that never
// carries patient data. Standard library only.
package acyclic

import (
	"context"
	"crypto"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"
)

// Config configures Connect.
type Config struct {
	// Vendor is one of epic, oracle, athena, meditech, ecw, generic.
	Vendor string
	// Profile overrides Vendor with your own profile.
	Profile *Vendor
	// BaseURL is the FHIR R4 base.
	BaseURL string
	// TokenURL defaults to token_endpoint from {BaseURL}/.well-known/smart-configuration.
	TokenURL string
	ClientID string
	// PrivateKeyPEM is for private_key_jwt: PKCS#8 PEM, or PKCS#1 (RSA) / SEC1 (EC).
	PrivateKeyPEM string
	// PrivateKey is an in-memory alternative to PrivateKeyPEM.
	PrivateKey crypto.Signer
	KeyID      string
	// Alg is RS384 (default) or ES384.
	Alg string
	// ClientSecret is for client_secret_basic.
	ClientSecret string
	// Auth overrides the vendor's auth method.
	Auth string
	// PracticeID is the athenaOne practice id.
	PracticeID string
	// Scopes default to the vendor's read scopes (plus note write if Write).
	Scopes []string
	Write  bool
	// Telemetry defaults to NoTelemetry.
	Telemetry Telemetry
	// HTTPClient defaults to a client with a 60 s timeout.
	HTTPClient *http.Client
	// Inspect sees every raw FHIR response. Local debugging only.
	Inspect func(InspectEvent)
	// Headers are added to every FHIR request.
	Headers map[string]string
}

// EHR is a connection to one FHIR server with one set of credentials.
type EHR struct {
	Vendor     *Vendor
	FHIR       *FHIRClient
	ClientID   string
	PracticeID string
	Telemetry  Telemetry
	TokenURL   string
}

// Connect validates the config, discovers the token endpoint if needed and
// returns a connection. No token is fetched until the first request.
func Connect(ctx context.Context, cfg Config) (*EHR, error) {
	vendor := cfg.Profile
	if vendor == nil {
		vendor = Vendors[cfg.Vendor]
	}
	if vendor == nil {
		ids := make([]string, 0, len(Vendors))
		for k := range Vendors {
			ids = append(ids, k)
		}
		sort.Strings(ids)
		return nil, fmt.Errorf("unknown vendor %s; one of %s", cfg.Vendor, strings.Join(ids, ", "))
	}
	if cfg.BaseURL == "" {
		return nil, errors.New("baseUrl is required")
	}
	if cfg.ClientID == "" {
		return nil, errors.New("clientId is required")
	}
	hc := cfg.HTTPClient
	if hc == nil {
		hc = &http.Client{Timeout: 60 * time.Second}
	}
	tokenURL := cfg.TokenURL
	if tokenURL == "" {
		var err error
		if tokenURL, err = discover(ctx, hc, cfg.BaseURL); err != nil {
			return nil, err
		}
	}
	auth := cfg.Auth
	if auth == "" {
		auth = vendor.Auth
	}
	scopes := cfg.Scopes
	if scopes == nil && vendor.Scopes != nil {
		scopes = vendor.Scopes(cfg.Write)
	}
	var key any
	if cfg.PrivateKey != nil {
		key = cfg.PrivateKey
	} else if cfg.PrivateKeyPEM != "" {
		key = cfg.PrivateKeyPEM
	}
	fhir, err := NewFHIRClient(FHIRClientConfig{
		Vendor: vendor.Name, BaseURL: cfg.BaseURL, TokenURL: tokenURL, Auth: auth,
		ClientID: cfg.ClientID, ClientSecret: cfg.ClientSecret, PrivateKey: key, KeyID: cfg.KeyID, Alg: cfg.Alg,
		Scopes: scopes, Telemetry: cfg.Telemetry, HTTPClient: hc, Inspect: cfg.Inspect, Headers: cfg.Headers,
	})
	if err != nil {
		return nil, err
	}
	tel := cfg.Telemetry
	if tel == nil {
		tel = NoTelemetry
	}
	return &EHR{Vendor: vendor, FHIR: fhir, ClientID: cfg.ClientID, PracticeID: cfg.PracticeID, Telemetry: tel, TokenURL: tokenURL}, nil
}

func discover(ctx context.Context, hc *http.Client, baseURL string) (string, error) {
	fail := errors.New("no tokenUrl given and smart-configuration has no token_endpoint")
	req, err := http.NewRequestWithContext(ctx, "GET", strings.TrimSuffix(baseURL, "/")+"/.well-known/smart-configuration", nil)
	if err != nil {
		return "", fail
	}
	req.Header.Set("Accept", "application/json")
	res, err := hc.Do(req)
	if err != nil {
		return "", transportErr("smart-configuration", err)
	}
	defer res.Body.Close()
	var j map[string]any
	if res.StatusCode >= 200 && res.StatusCode <= 299 {
		b, _ := io.ReadAll(io.LimitReader(res.Body, 1<<20))
		_ = json.Unmarshal(b, &j)
	}
	if t := str(j["token_endpoint"]); t != "" {
		return t, nil
	}
	return "", fail
}
