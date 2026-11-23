package acyclic

// Minimal FHIR R4 client: auth, retries, paging, OperationOutcome errors,
// and telemetry that never carries query strings, bodies or ids.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// FHIRError is a non-2xx FHIR response. The message names the status, the
// resource type and the OperationOutcome issue code only; diagnostics can
// echo search values, so they stay on Outcome for your own handling and are
// never logged by the SDK.
type FHIRError struct {
	Status       int
	ResourceType string
	Code         string         // OperationOutcome issue[0].code, if any
	Outcome      map[string]any // the OperationOutcome, if the server sent one
	Type         string         // "OperationOutcome:{code}" or the status
}

func (e *FHIRError) Error() string {
	if e.Code != "" {
		return fmt.Sprintf("FHIR %d on %s (%s)", e.Status, e.ResourceType, e.Code)
	}
	return fmt.Sprintf("FHIR %d on %s", e.Status, e.ResourceType)
}

func (e *FHIRError) ErrorType() string { return e.Type }

func (e *FHIRError) ErrorCode() string {
	if e.Code != "" {
		return e.Code
	}
	return e.Type
}

func newFHIRError(status int, resourceType string, outcome map[string]any) *FHIRError {
	code := str(get(index0(get(outcome, "issue")), "code"))
	t := strconv.Itoa(status)
	if code != "" {
		t = "OperationOutcome:" + code
	}
	return &FHIRError{Status: status, ResourceType: resourceType, Code: code, Outcome: outcome, Type: t}
}

// TransportError is a network failure. Go's *url.Error text includes the
// full request URL (query string and all), so the SDK drops it and keeps
// only the underlying cause.
type TransportError struct {
	ResourceType string
	Err          error
}

func (e *TransportError) Error() string {
	return fmt.Sprintf("transport error on %s: %v", e.ResourceType, e.Err)
}
func (e *TransportError) Unwrap() error     { return e.Err }
func (e *TransportError) ErrorType() string { return "transport" }
func (e *TransportError) ErrorCode() string { return "transport" }

func transportErr(resourceType string, err error) error {
	var ue *url.Error
	if errors.As(err, &ue) {
		err = ue.Err
	}
	return &TransportError{ResourceType: resourceType, Err: err}
}

// errorType is the telemetry error.type of an error (JavaScript e.type ?? "error").
func errorType(err error) string {
	var t interface{ ErrorType() string }
	if errors.As(err, &t) {
		if s := t.ErrorType(); s != "" {
			return s
		}
	}
	return "error"
}

// errorCode is the tool result code of an error (JavaScript e.code ?? e.type).
func errorCode(err error) string {
	var c interface{ ErrorCode() string }
	if errors.As(err, &c) {
		return c.ErrorCode()
	}
	var t interface{ ErrorType() string }
	if errors.As(err, &t) {
		return t.ErrorType()
	}
	return ""
}

// InspectEvent is what the Inspect hook sees for every FHIR response. It
// carries raw data (URLs with search values, bodies). It is a local
// debugging hook: your code, your rules. The SDK never logs it.
type InspectEvent struct {
	Method       string
	URL          string
	Status       int
	Duration     time.Duration
	RequestBody  any
	ResponseBody string
	Location     string
}

// Params are FHIR search parameters. A value is a string or a []string
// (repeated parameter); nil values are skipped.
type Params map[string]any

// RequestOptions tune one FHIRClient.Do call.
type RequestOptions struct {
	Params       Params
	Body         any    // JSON-encoded as application/fhir+json
	Accept       string // default application/fhir+json
	ResourceType string // for telemetry and errors; default: first path segment
	Raw          bool   // do not parse a successful body as JSON
}

// Response is one FHIR response.
type Response struct {
	Status      int
	ContentType string
	Location    string
	Text        string
	JSON        map[string]any // parsed body unless Raw; empty object when not JSON
}

// FHIRClient talks to one FHIR R4 base URL with one set of credentials.
type FHIRClient struct {
	Vendor     string
	BaseURL    string // without a trailing slash
	MaxRetries int    // GET retries on 429/503/transport errors; default 3

	base    *url.URL
	client  *http.Client
	tel     guard
	inspect func(InspectEvent)
	headers map[string]string
	tokens  *tokenSource
	sleep   func(ctx context.Context, d time.Duration) error // tests
}

// FHIRClientConfig configures NewFHIRClient. Connect fills it from Config.
type FHIRClientConfig struct {
	Vendor       string
	BaseURL      string
	TokenURL     string
	Auth         string // private_key_jwt | client_secret_basic
	ClientID     string
	ClientSecret string
	PrivateKey   any // crypto.Signer or a PEM string
	KeyID        string
	Alg          string
	Scopes       []string
	Telemetry    Telemetry
	HTTPClient   *http.Client
	Inspect      func(InspectEvent)
	Headers      map[string]string
	MaxRetries   int
}

// NewFHIRClient builds a client. Most code uses Connect instead.
func NewFHIRClient(c FHIRClientConfig) (*FHIRClient, error) {
	baseURL := strings.TrimRight(c.BaseURL, "/")
	base, err := url.Parse(baseURL + "/")
	if err != nil || base.Scheme == "" || base.Host == "" {
		return nil, errors.New("baseUrl must be an absolute URL")
	}
	hc := c.HTTPClient
	if hc == nil {
		hc = &http.Client{Timeout: 60 * time.Second}
	}
	retries := c.MaxRetries
	if retries == 0 {
		retries = 3
	}
	alg := c.Alg
	if alg == "" {
		alg = "RS384"
	}
	tel := guardOf(c.Telemetry)
	tc := tokenConfig{
		method: c.Auth, clientID: c.ClientID, clientSecret: c.ClientSecret, alg: alg, kid: c.KeyID,
		tokenURL: c.TokenURL, scopes: c.Scopes, client: hc, tel: tel, vendor: c.Vendor,
	}
	switch k := c.PrivateKey.(type) {
	case nil:
	case string:
		if k != "" {
			tc.key, tc.keyErr = ParsePrivateKey(k, alg)
		}
	default:
		tc.key, tc.keyErr = checkKey(k, alg)
	}
	return &FHIRClient{
		Vendor: c.Vendor, BaseURL: baseURL, MaxRetries: retries,
		base: base, client: hc, tel: tel, inspect: c.Inspect, headers: c.Headers,
		tokens: &tokenSource{cfg: tc}, sleep: sleepCtx,
	}, nil
}

func sameOrigin(a, b *url.URL) bool {
	return strings.EqualFold(a.Scheme, b.Scheme) && strings.EqualFold(a.Hostname(), b.Hostname()) && effectivePort(a) == effectivePort(b)
}

func effectivePort(u *url.URL) string {
	if p := u.Port(); p != "" {
		return p
	}
	switch strings.ToLower(u.Scheme) {
	case "https":
		return "443"
	case "http":
		return "80"
	}
	return ""
}

// Do sends one request. path is "Patient/123", "Observation", or an absolute
// URL on the same origin as BaseURL (next links, Binary URLs). Credentials
// are never sent to another origin.
func (c *FHIRClient) Do(ctx context.Context, method, path string, o RequestOptions) (*Response, error) {
	var u *url.URL
	var err error
	if strings.HasPrefix(path, "http") {
		u, err = url.Parse(path)
	} else {
		var rel *url.URL
		rel, err = url.Parse(path)
		if err == nil {
			u = c.base.ResolveReference(rel)
		}
	}
	if err != nil {
		return nil, errors.New("invalid FHIR request path")
	}
	if !sameOrigin(u, c.base) {
		return nil, fmt.Errorf("refusing to send credentials to %s://%s", u.Scheme, u.Host)
	}
	if len(o.Params) > 0 {
		q := u.Query()
		keys := make([]string, 0, len(o.Params))
		for k := range o.Params {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			switch v := o.Params[k].(type) {
			case nil:
			case []string:
				for _, one := range v {
					q.Add(k, one)
				}
			case []any:
				for _, one := range v {
					if one != nil {
						q.Add(k, jsString(one))
					}
				}
			default:
				q.Add(k, jsString(v))
			}
		}
		u.RawQuery = q.Encode()
	}
	resourceType := o.ResourceType
	if resourceType == "" {
		resourceType = path
		if i := strings.IndexAny(path, "/?"); i >= 0 {
			resourceType = path[:i]
		}
	}
	accept := o.Accept
	if accept == "" {
		accept = "application/fhir+json"
	}
	ctx, span := c.tel.startSpan(ctx, method+" "+resourceType, map[string]any{
		"http.request.method": method, "server.address": u.Hostname(), "url.template": c.template(u),
		"fhir.resource_type": resourceType, "acyclic.vendor": c.Vendor,
	})
	res, err := c.send(ctx, method, u, o, accept, resourceType, span)
	if err != nil {
		span.setAttributes(map[string]any{"error.type": errorType(err)})
		span.end(err)
		return nil, err
	}
	span.end(nil)
	return res, nil
}

func (c *FHIRClient) send(ctx context.Context, method string, u *url.URL, o RequestOptions, accept, resourceType string, span guardSpan) (*Response, error) {
	var body []byte
	if o.Body != nil {
		body = []byte(marshalJSON(o.Body))
	}
	retries := 0
	refreshed, forceNext := false, false
	for {
		token, err := c.tokens.get(ctx, forceNext)
		if err != nil {
			return nil, err
		}
		if forceNext {
			forceNext, refreshed = false, true
		}
		var rd io.Reader
		if body != nil {
			rd = bytes.NewReader(body)
		}
		req, err := http.NewRequestWithContext(ctx, method, u.String(), rd)
		if err != nil {
			return nil, errors.New("could not build FHIR request")
		}
		req.Header.Set("Accept", accept)
		req.Header.Set("Authorization", "Bearer "+token)
		for k, v := range c.headers {
			req.Header.Set(k, v)
		}
		if body != nil {
			req.Header.Set("Content-Type", "application/fhir+json")
		}
		started := time.Now()
		res, err := c.client.Do(req)
		if err != nil {
			if retries < c.MaxRetries && method == "GET" && ctx.Err() == nil {
				retries++
				if serr := c.sleep(ctx, backoff(retries)); serr != nil {
					return nil, serr
				}
				continue
			}
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			return nil, transportErr(resourceType, err)
		}
		if res.StatusCode == 401 && !refreshed {
			drain(res)
			forceNext = true
			continue
		}
		if (res.StatusCode == 429 || res.StatusCode == 503) && retries < c.MaxRetries && method == "GET" {
			retries++
			c.tel.log(ctx, "warn", "fhir.retry", map[string]any{"http.response.status_code": res.StatusCode, "acyclic.retry_count": retries, "fhir.resource_type": resourceType})
			wait, ok := retryAfter(res.Header.Get("Retry-After"))
			if !ok {
				wait = backoff(retries)
			}
			drain(res)
			if serr := c.sleep(ctx, wait); serr != nil {
				return nil, serr
			}
			continue
		}
		raw, rerr := io.ReadAll(res.Body)
		res.Body.Close()
		if rerr != nil {
			return nil, transportErr(resourceType, rerr)
		}
		text := string(raw)
		location := res.Header.Get("Location")
		if c.inspect != nil {
			ev := InspectEvent{Method: method, URL: u.String(), Status: res.StatusCode, Duration: time.Since(started), ResponseBody: text, Location: location}
			if o.Body != nil {
				ev.RequestBody = o.Body
			}
			func() { defer func() { _ = recover() }(); c.inspect(ev) }()
		}
		attrs := map[string]any{"http.response.status_code": res.StatusCode}
		if retries > 0 {
			attrs["acyclic.retry_count"] = retries
		}
		span.setAttributes(attrs)
		ok := res.StatusCode >= 200 && res.StatusCode <= 299
		out := &Response{Status: res.StatusCode, ContentType: res.Header.Get("Content-Type"), Location: location, Text: text, JSON: map[string]any{}}
		if !o.Raw || !ok {
			var v any
			if text != "" && json.Unmarshal(raw, &v) == nil {
				if m, isMap := v.(map[string]any); isMap {
					out.JSON = m
				}
			}
		}
		if !ok {
			var outcome map[string]any
			if out.JSON["resourceType"] == "OperationOutcome" {
				outcome = out.JSON
			}
			return nil, newFHIRError(res.StatusCode, resourceType, outcome)
		}
		return out, nil
	}
}

func drain(res *http.Response) {
	_, _ = io.Copy(io.Discard, io.LimitReader(res.Body, 1<<16))
	res.Body.Close()
}

// Read fetches type/id.
func (c *FHIRClient) Read(ctx context.Context, resourceType, id string) (map[string]any, error) {
	res, err := c.Do(ctx, "GET", resourceType+"/"+encodeURIComponent(id), RequestOptions{ResourceType: resourceType})
	if err != nil {
		return nil, err
	}
	return res.JSON, nil
}

// Create POSTs a resource. POSTs are never retried.
func (c *FHIRClient) Create(ctx context.Context, resourceType string, resource any) (*Response, error) {
	return c.Do(ctx, "POST", resourceType, RequestOptions{Body: resource, ResourceType: resourceType})
}

// SearchResult is the outcome of a paged search.
type SearchResult struct {
	Resources []map[string]any
	Truncated bool // stopped at limit while more remained
}

// Search follows Bundle next links (same origin only) until limit resources,
// at most 100 pages (then Truncated is true).
// Entries with search.mode=outcome, OperationOutcomes and resources of other
// types (_include results) are dropped.
func (c *FHIRClient) Search(ctx context.Context, resourceType string, params Params, limit int) (*SearchResult, error) {
	resources := []map[string]any{}
	page, err := c.Do(ctx, "GET", resourceType, RequestOptions{Params: params, ResourceType: resourceType})
	if err != nil {
		return nil, err
	}
	for pages := 1; ; pages++ {
		for _, e := range asArr(page.JSON["entry"]) {
			r := asMap(get(e, "resource"))
			if get(e, "search", "mode") == "outcome" || get(r, "resourceType") == "OperationOutcome" {
				continue
			}
			if r == nil || r["resourceType"] != resourceType {
				continue
			}
			resources = append(resources, r)
		}
		var next string
		for _, l := range asArr(page.JSON["link"]) {
			if get(l, "relation") == "next" {
				next = str(get(l, "url"))
				break
			}
		}
		if pages >= maxPages { // a server looping its next link
			if len(resources) > limit {
				resources = resources[:max(limit, 0)]
			}
			return &SearchResult{Resources: resources, Truncated: true}, nil
		}
		if len(resources) >= limit || next == "" {
			truncated := len(resources) > limit || (len(resources) >= limit && next != "")
			if len(resources) > limit {
				resources = resources[:max(limit, 0)]
			}
			return &SearchResult{Resources: resources, Truncated: truncated}, nil
		}
		page, err = c.Do(ctx, "GET", next, RequestOptions{ResourceType: resourceType})
		if err != nil {
			return nil, err
		}
	}
}

var (
	reTypeSeg = regexp.MustCompile(`^[A-Z][A-Za-z]+$`)
	reOpSeg   = regexp.MustCompile(`^\$[a-z-]+$`)
	// fixedSegs are path words that are never ids.
	fixedSegs = map[string]bool{"_history": true, "_search": true, "v1": true, "patients": true, "appointments": true}
)

// maxPages stops Search when a server loops its next link.
const maxPages = 100

// template renders "/api/FHIR/R4/Patient/{id}": never ids, never a query.
// Resource types, $operations and a few fixed path words are kept; every
// other segment could be an id, even an all-lowercase one. The base path
// is kept only for URLs under it.
func (c *FHIRClient) template(u *url.URL) string {
	basePath := strings.TrimSuffix(c.base.EscapedPath(), "/")
	rest := u.EscapedPath()
	inBase := strings.HasPrefix(rest, basePath+"/")
	if inBase {
		rest = rest[len(basePath):]
	}
	var t []string
	for _, p := range strings.Split(rest, "/") {
		if p == "" {
			continue
		}
		if reTypeSeg.MatchString(p) || reOpSeg.MatchString(p) || fixedSegs[p] {
			t = append(t, p)
		} else {
			t = append(t, "{id}")
		}
	}
	prefix := ""
	if inBase {
		prefix = basePath
	}
	return prefix + "/" + strings.Join(t, "/")
}

func retryAfter(h string) (time.Duration, bool) {
	if h == "" {
		return 0, false
	}
	if s, err := strconv.ParseFloat(strings.TrimSpace(h), 64); err == nil {
		d := time.Duration(s * float64(time.Second))
		if d > 30*time.Second {
			d = 30 * time.Second
		}
		return d, true
	}
	if t, err := http.ParseTime(h); err == nil {
		d := time.Until(t)
		if d < 0 {
			d = 0
		}
		if d > 30*time.Second {
			d = 30 * time.Second
		}
		return d, true
	}
	return 0, false
}

// backoff is exponential with full jitter: 0..(250ms * 2^n), capped at 8s.
func backoff(n int) time.Duration {
	ceil := 250 * float64(int(1)<<n)
	if ceil > 8000 {
		ceil = 8000
	}
	return time.Duration(rand.Float64() * ceil * float64(time.Millisecond))
}
