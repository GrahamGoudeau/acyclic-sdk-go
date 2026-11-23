package acyclic

// AWS Signature Version 4, enough for Bedrock JSON POSTs.

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
)

// AWSCredentials are static or temporary AWS credentials.
type AWSCredentials struct {
	AccessKeyID     string
	SecretAccessKey string
	SessionToken    string
}

func sha256Hex(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

func hmacSHA256(key []byte, data string) []byte {
	m := hmac.New(sha256.New, key)
	m.Write([]byte(data))
	return m.Sum(nil)
}

// encodeSegment is RFC 3986 encoding: only A-Z a-z 0-9 - _ . ~ stay.
func encodeSegment(s string) string { return percentEncode(s, "-_.~") }

// sigV4Request is the input to the signing core.
type sigV4Request struct {
	method      string
	u           *url.URL
	headers     map[string]string // lowercase names, signed as given
	payloadHash string
}

// canonicalRequest builds the SigV4 canonical request. Non-S3 services
// canonicalize the path as sent, encoded once more (so a model id's %3A
// becomes %253A).
func canonicalRequest(r sigV4Request) (canonical, signedHeaders string) {
	path := r.u.EscapedPath()
	segs := strings.Split(path, "/")
	for i, s := range segs {
		segs[i] = encodeSegment(s)
	}
	canonicalURI := strings.Join(segs, "/")
	if canonicalURI == "" {
		canonicalURI = "/"
	}
	type pair struct{ k, v string }
	var q []pair
	for k, vs := range r.u.Query() {
		for _, v := range vs {
			q = append(q, pair{encodeURIComponent(k), encodeURIComponent(v)})
		}
	}
	sort.SliceStable(q, func(i, j int) bool {
		if q[i].k != q[j].k {
			return q[i].k < q[j].k
		}
		return q[i].v < q[j].v
	})
	qs := make([]string, len(q))
	for i, p := range q {
		qs[i] = p.k + "=" + p.v
	}
	names := make([]string, 0, len(r.headers))
	for n := range r.headers {
		names = append(names, n)
	}
	sort.Strings(names)
	var hb strings.Builder
	for _, n := range names {
		hb.WriteString(n + ":" + r.headers[n] + "\n")
	}
	signedHeaders = strings.Join(names, ";")
	canonical = strings.Join([]string{r.method, canonicalURI, strings.Join(qs, "&"), hb.String(), signedHeaders, r.payloadHash}, "\n")
	return canonical, signedHeaders
}

func signingKey(secret, day, region, service string) []byte {
	k := hmacSHA256([]byte("AWS4"+secret), day)
	k = hmacSHA256(k, region)
	k = hmacSHA256(k, service)
	return hmacSHA256(k, "aws4_request")
}

// signCore returns the Authorization header value for a request whose
// headers already include host and x-amz-date.
func signCore(r sigV4Request, amzDate, region, service string, creds AWSCredentials) string {
	canonical, signed := canonicalRequest(r)
	day := amzDate[:8]
	scope := day + "/" + region + "/" + service + "/aws4_request"
	toSign := strings.Join([]string{"AWS4-HMAC-SHA256", amzDate, scope, sha256Hex([]byte(canonical))}, "\n")
	sig := hex.EncodeToString(hmacSHA256(signingKey(creds.SecretAccessKey, day, region, service), toSign))
	return "AWS4-HMAC-SHA256 Credential=" + creds.AccessKeyID + "/" + scope + ", SignedHeaders=" + signed + ", Signature=" + sig
}

// SignV4 signs req in place for region and service: it sets x-amz-date,
// x-amz-content-sha256, x-amz-security-token (when a session token is set)
// and Authorization. Every header already on req is signed.
func SignV4(req *http.Request, body []byte, region, service string, creds AWSCredentials, now time.Time) error {
	if creds.AccessKeyID == "" || creds.SecretAccessKey == "" {
		return errors.New("AWS credentials are required")
	}
	amzDate := now.UTC().Format("20060102T150405Z")
	payloadHash := sha256Hex(body)
	h := map[string]string{}
	for k, vs := range req.Header {
		h[strings.ToLower(k)] = strings.TrimSpace(strings.Join(vs, ","))
	}
	h["host"] = req.URL.Host
	h["x-amz-date"] = amzDate
	h["x-amz-content-sha256"] = payloadHash
	if creds.SessionToken != "" {
		h["x-amz-security-token"] = creds.SessionToken
	}
	auth := signCore(sigV4Request{method: req.Method, u: req.URL, headers: h, payloadHash: payloadHash}, amzDate, region, service, creds)
	req.Header.Set("X-Amz-Date", amzDate)
	req.Header.Set("X-Amz-Content-Sha256", payloadHash)
	if creds.SessionToken != "" {
		req.Header.Set("X-Amz-Security-Token", creds.SessionToken)
	}
	req.Header.Set("Authorization", auth)
	return nil
}
