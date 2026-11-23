package acyclic

import (
	"encoding/hex"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"
)

var exampleCreds = AWSCredentials{AccessKeyID: "AKIDEXAMPLE", SecretAccessKey: "wJalrXUtnFEMI/K7MDENG+bPxRfiCYEXAMPLEKEY"}

// AWS documentation: "Examples of how to derive a signing key for Signature Version 4".
func TestSigV4SigningKeyAWSExample(t *testing.T) {
	got := hex.EncodeToString(signingKey("wJalrXUtnFEMI/K7MDENG+bPxRfiCYEXAMPLEKEY", "20120215", "us-east-1", "iam"))
	if want := "f4780e2d9f65fa895f9c67b32ce1baf0b0d8a43505a000a1a9e090d414db404d"; got != want {
		t.Fatalf("signing key %s", got)
	}
}

// AWS SigV4 test suite: get-vanilla and the IAM ListUsers walkthrough.
func TestSigV4AWSExamples(t *testing.T) {
	cases := []struct {
		name, method, rawURL, region, service, want string
		headers                                     map[string]string
	}{
		{
			name: "get-vanilla", method: "GET", rawURL: "https://example.amazonaws.com/", region: "us-east-1", service: "service",
			headers: map[string]string{"host": "example.amazonaws.com", "x-amz-date": "20150830T123600Z"},
			want:    "AWS4-HMAC-SHA256 Credential=AKIDEXAMPLE/20150830/us-east-1/service/aws4_request, SignedHeaders=host;x-amz-date, Signature=5fa00fa31553b73ebf1942676e86291e8372ff2a2260956d9b8aae1d763fbf31",
		},
		{
			name: "iam ListUsers", method: "GET", rawURL: "https://iam.amazonaws.com/?Action=ListUsers&Version=2010-05-08", region: "us-east-1", service: "iam",
			headers: map[string]string{"content-type": "application/x-www-form-urlencoded; charset=utf-8", "host": "iam.amazonaws.com", "x-amz-date": "20150830T123600Z"},
			want:    "AWS4-HMAC-SHA256 Credential=AKIDEXAMPLE/20150830/us-east-1/iam/aws4_request, SignedHeaders=content-type;host;x-amz-date, Signature=5d672d79c15b13162d9279b0855cfba6789a8edb4c82c400e06b5924a6f2b5d7",
		},
	}
	for _, c := range cases {
		u, _ := url.Parse(c.rawURL)
		got := signCore(sigV4Request{method: c.method, u: u, headers: c.headers, payloadHash: sha256Hex(nil)}, "20150830T123600Z", c.region, c.service, exampleCreds)
		if got != c.want {
			t.Errorf("%s:\n got %s\nwant %s", c.name, got, c.want)
		}
	}
}

// Non-S3 services canonicalize the already-encoded path once more.
func TestSigV4BedrockPathDoubleEncoding(t *testing.T) {
	m := Bedrock(BedrockConfig{Model: "us.anthropic.claude-sonnet-4-5-20250929-v1:0", Credentials: exampleCreds}).(*bedrockModel)
	req, _ := http.NewRequest("POST", m.url(), strings.NewReader("{}"))
	if req.URL.EscapedPath() != "/model/us.anthropic.claude-sonnet-4-5-20250929-v1%3A0/converse" {
		t.Fatalf("wire path %s", req.URL.EscapedPath())
	}
	req.Header.Set("Content-Type", "application/json")
	if err := SignV4(req, []byte("{}"), "us-east-1", "bedrock", AWSCredentials{AccessKeyID: "AKID", SecretAccessKey: "secret", SessionToken: "tok"}, time.Date(2026, 11, 23, 12, 0, 0, 0, time.UTC)); err != nil {
		t.Fatal(err)
	}
	h := map[string]string{}
	for k := range req.Header {
		h[strings.ToLower(k)] = req.Header.Get(k)
	}
	h["host"] = req.URL.Host
	delete(h, "authorization")
	canonical, signed := canonicalRequest(sigV4Request{method: "POST", u: req.URL, headers: h, payloadHash: sha256Hex([]byte("{}"))})
	if !strings.Contains(canonical, "\n/model/us.anthropic.claude-sonnet-4-5-20250929-v1%253A0/converse\n") {
		t.Fatalf("canonical:\n%s", canonical)
	}
	if signed != "content-type;host;x-amz-content-sha256;x-amz-date;x-amz-security-token" {
		t.Fatal(signed)
	}
	auth := req.Header.Get("Authorization")
	if !strings.HasPrefix(auth, "AWS4-HMAC-SHA256 Credential=AKID/20261123/us-east-1/bedrock/aws4_request, SignedHeaders="+signed+", Signature=") {
		t.Fatal(auth)
	}
	// Deterministic: same input, same signature.
	req2, _ := http.NewRequest("POST", m.url(), strings.NewReader("{}"))
	req2.Header.Set("Content-Type", "application/json")
	_ = SignV4(req2, []byte("{}"), "us-east-1", "bedrock", AWSCredentials{AccessKeyID: "AKID", SecretAccessKey: "secret", SessionToken: "tok"}, time.Date(2026, 11, 23, 12, 0, 0, 0, time.UTC))
	if req2.Header.Get("Authorization") != auth {
		t.Fatal("signature not deterministic")
	}
}
