package acyclic

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"math"
	"strconv"
	"strings"
	"time"
	"unicode/utf16"
)

// Helpers for walking decoded JSON (map[string]any / []any / float64) with
// the same truthiness and string rules as the JavaScript reference SDK.

func asMap(v any) map[string]any {
	m, _ := v.(map[string]any)
	return m
}

func asArr(v any) []any {
	switch a := v.(type) {
	case []any:
		return a
	case []map[string]any:
		out := make([]any, len(a))
		for i, x := range a {
			out[i] = x
		}
		return out
	}
	return nil
}

// get walks nested objects: get(r, "period", "start").
func get(v any, keys ...string) any {
	for _, k := range keys {
		m := asMap(v)
		if m == nil {
			return nil
		}
		v = m[k]
	}
	return v
}

// first returns the first non-nil value (JavaScript ??).
func first(vs ...any) any {
	for _, v := range vs {
		if v != nil {
			return v
		}
	}
	return nil
}

// firstTruthy returns the first truthy value, else the last one (JavaScript ||).
func firstTruthy(vs ...any) any {
	for _, v := range vs {
		if truthy(v) {
			return v
		}
	}
	if len(vs) == 0 {
		return nil
	}
	return vs[len(vs)-1]
}

func truthy(v any) bool {
	switch x := v.(type) {
	case nil:
		return false
	case bool:
		return x
	case string:
		return x != ""
	case float64:
		return x != 0 && !math.IsNaN(x)
	case int:
		return x != 0
	case int64:
		return x != 0
	case json.Number:
		f, err := x.Float64()
		return err == nil && f != 0
	}
	return true
}

func str(v any) string {
	s, _ := v.(string)
	return s
}

// jsString converts a value the way JavaScript template literals do.
func jsString(v any) string {
	switch x := v.(type) {
	case nil:
		return "undefined"
	case string:
		return x
	case bool:
		if x {
			return "true"
		}
		return "false"
	case float64:
		return jsNumber(x)
	case float32:
		return jsNumber(float64(x))
	case int:
		return strconv.Itoa(x)
	case int64:
		return strconv.FormatInt(x, 10)
	case json.Number:
		if f, err := x.Float64(); err == nil {
			return jsNumber(f)
		}
		return x.String()
	case []any:
		parts := make([]string, len(x))
		for i, e := range x {
			if e != nil {
				parts[i] = jsString(e)
			}
		}
		return strings.Join(parts, ",")
	case map[string]any:
		return "[object Object]"
	}
	b, _ := json.Marshal(v)
	return string(b)
}

// jsNumber formats a float64 as JavaScript's Number.prototype.toString does.
func jsNumber(f float64) string {
	switch {
	case math.IsNaN(f):
		return "NaN"
	case math.IsInf(f, 1):
		return "Infinity"
	case math.IsInf(f, -1):
		return "-Infinity"
	case f == 0:
		return "0"
	}
	a := math.Abs(f)
	if a >= 1e-6 && a < 1e21 {
		return strconv.FormatFloat(f, 'f', -1, 64)
	}
	s := strconv.FormatFloat(f, 'e', -1, 64) // 1e-07, 1.5e+21
	mant, exp, _ := strings.Cut(s, "e")
	sign := exp[:1]
	digits := strings.TrimLeft(exp[1:], "0")
	if digits == "" {
		digits = "0"
	}
	return mant + "e" + sign + digits
}

// toNumber reads a numeric argument (JSON float64, Go ints, numeric strings).
func toNumber(v any) (float64, bool) {
	switch x := v.(type) {
	case float64:
		return x, true
	case float32:
		return float64(x), true
	case int:
		return float64(x), true
	case int32:
		return float64(x), true
	case int64:
		return float64(x), true
	case uint:
		return float64(x), true
	case json.Number:
		f, err := x.Float64()
		return f, err == nil
	case string:
		f, err := strconv.ParseFloat(strings.TrimSpace(x), 64)
		return f, err == nil
	case bool:
		if x {
			return 1, true
		}
		return 0, true
	}
	return 0, false
}

// encodeURIComponent matches JavaScript's: everything except
// A-Z a-z 0-9 - _ . ! ~ * ' ( ) is percent-encoded as UTF-8.
func encodeURIComponent(s string) string {
	return percentEncode(s, "-_.!~*'()")
}

// percentEncode escapes every byte except ASCII letters, digits and keep.
func percentEncode(s, keep string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || strings.IndexByte(keep, c) >= 0 {
			b.WriteByte(c)
			continue
		}
		b.WriteByte('%')
		b.WriteString(strings.ToUpper(hex.EncodeToString([]byte{c})))
	}
	return b.String()
}

func randomID(prefix string) string {
	b := make([]byte, 12)
	_, _ = rand.Read(b)
	return prefix + hex.EncodeToString(b)
}

// base64ToText decodes standard base64 (whitespace and missing padding
// tolerated) into a string, as atob + TextDecoder do.
func base64ToText(s string) (string, error) {
	s = strings.Map(func(r rune) rune {
		if r == ' ' || r == '\t' || r == '\n' || r == '\r' || r == '\f' {
			return -1
		}
		return r
	}, s)
	b, err := base64.RawStdEncoding.DecodeString(strings.TrimRight(s, "="))
	if err != nil {
		return "", err
	}
	return strings.ToValidUTF8(string(b), "�"), nil
}

// utf16Len is JavaScript's String.prototype.length.
func utf16Len(s string) int { return len(utf16.Encode([]rune(s))) }

// marshalJSON is JSON.stringify: no HTML escaping, no trailing newline.
func marshalJSON(v any) string {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return "null"
	}
	return strings.TrimSuffix(buf.String(), "\n")
}

func isoNow() string { return time.Now().UTC().Format("2006-01-02T15:04:05.000Z") }

func today() string { return time.Now().UTC().Format("2006-01-02") }

func sleepCtx(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
