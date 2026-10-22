// The S3 target follows illisis-minimap s3/publisher.ts (signed path-style PUTs, results
// sorted by status). Blue Oak Model License 1.0.0: https://blueoakcouncil.org/license/1.0.0

package publish

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// S3Config configures an S3-compatible target: AWS S3, Cloudflare R2, MinIO.
type S3Config struct {
	// Endpoint is the service URL, for example https://<account>.r2.cloudflarestorage.com
	// or http://127.0.0.1:9000. Its path is ignored.
	Endpoint string
	Bucket   string
	// Prefix is prepended to every key ("map" puts catalog.json at map/catalog.json).
	Prefix string
	Credentials
	// VirtualHost addresses the bucket as <bucket>.<endpoint host> instead of
	// <endpoint>/<bucket> (path style, the default, which R2 and MinIO accept).
	VirtualHost bool
	// Client is the HTTP client (default: one with a 60 s timeout and 64 idle
	// connections).
	Client *http.Client
	// Now is the signing clock (tests).
	Now func() time.Time
}

// S3Target uploads objects with signed PUTs.
type S3Target struct {
	cfg    S3Config
	origin *url.URL
}

// NewS3Target checks cfg and returns the target.
func NewS3Target(cfg S3Config) (*S3Target, error) {
	u, err := url.Parse(cfg.Endpoint)
	if err != nil {
		return nil, fmt.Errorf("s3: bad endpoint: %w", err)
	}
	if (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, fmt.Errorf("s3: endpoint %q is not an http(s) URL", cfg.Endpoint)
	}
	if cfg.Bucket == "" || strings.ContainsAny(cfg.Bucket, "/?#") {
		return nil, fmt.Errorf("s3: bad bucket %q", cfg.Bucket)
	}
	if cfg.AccessKeyID == "" || cfg.SecretAccessKey == "" {
		return nil, errors.New("s3: access key id and secret access key are required")
	}
	cfg.Prefix = strings.Trim(cfg.Prefix, "/")
	if cfg.Prefix != "" && !validKey(cfg.Prefix) {
		return nil, fmt.Errorf("s3: bad prefix %q", cfg.Prefix)
	}
	if cfg.Region == "" {
		cfg.Region = "us-east-1"
	}
	if cfg.Client == nil {
		// Enough idle connections for the publisher's workers (the default transport
		// keeps 2 per host).
		tr := http.DefaultTransport.(*http.Transport).Clone()
		tr.MaxIdleConns = 64
		tr.MaxIdleConnsPerHost = 64
		cfg.Client = &http.Client{Timeout: 60 * time.Second, Transport: tr}
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	return &S3Target{cfg: cfg, origin: &url.URL{Scheme: u.Scheme, Host: u.Host}}, nil
}

// ID implements Target.
func (s *S3Target) ID() string {
	id := "s3:" + s.origin.String() + "/" + s.cfg.Bucket
	if s.cfg.Prefix != "" {
		id += "/" + s.cfg.Prefix
	}
	return id
}

// objectKey is the bucket key of a published key.
func (s *S3Target) objectKey(key string) string {
	if s.cfg.Prefix == "" {
		return key
	}
	return s.cfg.Prefix + "/" + key
}

// request builds the signed PUT of key. The signed host is the endpoint's (with its
// port), which is what net/http sends.
func (s *S3Target) request(ctx context.Context, key string, body []byte, m Meta) (*http.Request, error) {
	host := s.origin.Host
	path := "/" + s.cfg.Bucket + "/" + s.objectKey(key)
	if s.cfg.VirtualHost {
		host = s.cfg.Bucket + "." + s.origin.Host
		path = "/" + s.objectKey(key)
	}
	path = uriEncodePath(path)
	headers := map[string]string{
		"host":                 host,
		"x-amz-content-sha256": sha256Hex(body),
		"x-amz-date":           amzDate(s.cfg.Now()),
	}
	if m.ContentType != "" {
		headers["content-type"] = m.ContentType
	}
	if m.CacheControl != "" {
		headers["cache-control"] = m.CacheControl
	}
	if m.ContentEncoding != "" {
		headers["content-encoding"] = m.ContentEncoding
	}
	auth := authorization(s.cfg.Credentials, http.MethodPut, path, headers)
	u := s.origin.Scheme + "://" + host + path
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, u, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	for n, v := range headers {
		if n != "host" {
			req.Header.Set(n, v)
		}
	}
	req.Header.Set("Authorization", auth)
	req.ContentLength = int64(len(body))
	return req, nil
}

// Put implements Target.
func (s *S3Target) Put(ctx context.Context, key string, body []byte, m Meta) error {
	if !validKey(key) {
		return &PutError{Key: key, Status: 400, Body: "bad key"}
	}
	req, err := s.request(ctx, key, body, m)
	if err != nil {
		return err
	}
	res, err := s.cfg.Client.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	msg, _ := io.ReadAll(io.LimitReader(res.Body, 512))
	if res.StatusCode >= 200 && res.StatusCode < 300 {
		return nil
	}
	return &PutError{Key: key, Status: res.StatusCode, Body: strings.TrimSpace(string(msg))}
}
