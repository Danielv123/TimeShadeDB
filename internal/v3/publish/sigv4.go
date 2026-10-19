// AWS Signature Version 4 for S3 PUTs, without an SDK. Ported from illisis-minimap
// s3/sigv4.ts. Blue Oak Model License 1.0.0: https://blueoakcouncil.org/license/1.0.0

package publish

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"sort"
	"strings"
	"time"
)

// Credentials sign S3 requests.
type Credentials struct {
	AccessKeyID     string
	SecretAccessKey string
	// Region is the signing region: "auto" for Cloudflare R2, the bucket's region for
	// AWS, usually "us-east-1" for MinIO.
	Region string
}

func sha256Hex(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

// amzDate formats t as x-amz-date wants it: 20130524T000000Z.
func amzDate(t time.Time) string { return t.UTC().Format("20060102T150405Z") }

func hmacSHA256(key []byte, data string) []byte {
	m := hmac.New(sha256.New, key)
	m.Write([]byte(data))
	return m.Sum(nil)
}

// authorization returns the Authorization header of one S3 request without a query
// string. path is the canonical (already URI-encoded) path. headers maps lower-case
// names to values; every one is signed, and it must hold x-amz-date and
// x-amz-content-sha256 (the hex SHA-256 of the body).
func authorization(c Credentials, method, path string, headers map[string]string) string {
	names := make([]string, 0, len(headers))
	for n := range headers {
		names = append(names, n)
	}
	sort.Strings(names)
	date := headers["x-amz-date"]
	day := date[:min(8, len(date))]
	scope := day + "/" + c.Region + "/s3/aws4_request"
	var req strings.Builder
	req.WriteString(method + "\n" + path + "\n\n")
	for _, n := range names {
		req.WriteString(n + ":" + strings.TrimSpace(headers[n]) + "\n")
	}
	signed := strings.Join(names, ";")
	req.WriteString("\n" + signed + "\n" + headers["x-amz-content-sha256"])
	key := hmacSHA256([]byte("AWS4"+c.SecretAccessKey), day)
	for _, part := range []string{c.Region, "s3", "aws4_request"} {
		key = hmacSHA256(key, part)
	}
	toSign := "AWS4-HMAC-SHA256\n" + date + "\n" + scope + "\n" + sha256Hex([]byte(req.String()))
	sig := hex.EncodeToString(hmacSHA256(key, toSign))
	return "AWS4-HMAC-SHA256 Credential=" + c.AccessKeyID + "/" + scope + ",SignedHeaders=" + signed + ",Signature=" + sig
}

// uriEncodePath encodes a path for the canonical request and the request line: every
// byte except the unreserved characters (A-Z a-z 0-9 - . _ ~) and "/" becomes %XX.
func uriEncodePath(p string) string {
	const hexDigits = "0123456789ABCDEF"
	var b strings.Builder
	for i := 0; i < len(p); i++ {
		c := p[i]
		if (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '-' || c == '.' || c == '_' || c == '~' || c == '/' {
			b.WriteByte(c)
			continue
		}
		b.WriteByte('%')
		b.WriteByte(hexDigits[c>>4])
		b.WriteByte(hexDigits[c&15])
	}
	return b.String()
}
