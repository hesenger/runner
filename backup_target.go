package main

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

// backupTarget stores backups. Runner only ever writes new objects: it never
// lists, reads or deletes them, so its credentials can be write-only.
type backupTarget interface {
	Put(ctx context.Context, name string, body []byte) error
}

// newBackupTarget returns the target of t, using the download client, whose
// timeout is downloadTimeout.
func newBackupTarget(t BackupTarget, clients *Clients) (backupTarget, error) {
	switch t.Kind {
	case "dir":
		return dirTarget{path: t.Path}, nil
	case "s3":
		endpoint, err := parseEndpoint(t.Endpoint)
		if err != nil {
			return nil, err
		}
		return &s3Target{
			client: clients.Download, endpoint: endpoint, region: t.Region, bucket: t.Bucket,
			accessKeyID: t.AccessKeyID, secretAccessKey: t.SecretAccessKey, now: time.Now,
		}, nil
	}
	return nil, fmt.Errorf("unknown backup target kind %q", t.Kind)
}

// contextWithTimeout bounds one backup write; the client's own timeout
// bounds the request too.
func contextWithTimeout(clients *Clients) (context.Context, context.CancelFunc) {
	timeout := clients.Download.Timeout
	if timeout <= 0 {
		timeout = 10 * time.Minute
	}
	return context.WithTimeout(context.Background(), timeout)
}

// dirTarget writes backups to a local directory, such as a mounted disk.
type dirTarget struct {
	path string
}

// Put writes body to <path>/<name>.part, syncs it and renames it, so a
// backup is either complete or absent.
func (d dirTarget) Put(ctx context.Context, name string, body []byte) error {
	final := filepath.Join(d.path, filepath.FromSlash(name))
	dir := filepath.Dir(final)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	part := final + ".part"
	f, err := os.OpenFile(part, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	_, err = f.Write(body)
	if err == nil {
		err = f.Sync()
	}
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err == nil {
		err = os.Rename(part, final)
	}
	if err != nil {
		os.Remove(part)
		return err
	}
	// make the rename durable too
	if df, err := os.Open(dir); err == nil {
		df.Sync()
		df.Close()
	}
	return nil
}

// parseEndpoint checks an S3 endpoint URL such as
// https://<account-id>.r2.cloudflarestorage.com.
func parseEndpoint(s string) (*url.URL, error) {
	u, err := url.Parse(s)
	if err != nil {
		return nil, fmt.Errorf("must be a URL: %v", err)
	}
	if (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" || u.RawQuery != "" || u.User != nil {
		return nil, fmt.Errorf("must be a URL like https://<account-id>.r2.cloudflarestorage.com, got %q", s)
	}
	return u, nil
}

// s3Target writes backups to an S3-compatible bucket with one signed PUT
// per backup, addressed path-style: <endpoint>/<bucket>/<name>.
type s3Target struct {
	client                       *http.Client
	endpoint                     *url.URL
	region, bucket               string
	accessKeyID, secretAccessKey string
	now                          func() time.Time
}

func (s *s3Target) Put(ctx context.Context, name string, body []byte) error {
	u := *s.endpoint
	u.Path = strings.TrimSuffix(u.Path, "/") + "/" + s.bucket + "/" + name
	u.RawPath = ""
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, u.String(), bytes.NewReader(body))
	if err != nil {
		return err
	}
	sum := sha256.Sum256(body)
	payloadHash := hex.EncodeToString(sum[:])
	req.Header.Set("Content-Type", "application/octet-stream")
	req.Header.Set("X-Amz-Content-Sha256", payloadHash)
	signV4(req, payloadHash, s.accessKeyID, s.secretAccessKey, s.region, "s3", s.now())

	resp, err := s.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("PUT %s/%s: %s: %s", s.bucket, name, resp.Status, strings.TrimSpace(string(msg)))
	}
	io.Copy(io.Discard, resp.Body)
	return nil
}

// signV4 signs req with AWS Signature Version 4: it sets X-Amz-Date and
// Authorization. Every header set on req is signed, plus Host. payloadHash
// is the hex SHA-256 of the body.
func signV4(req *http.Request, payloadHash, accessKeyID, secretAccessKey, region, service string, t time.Time) {
	t = t.UTC()
	amzDate := t.Format("20060102T150405Z")
	day := t.Format("20060102")
	req.Header.Set("X-Amz-Date", amzDate)

	host := req.Host
	if host == "" {
		host = req.URL.Host
	}
	headers := map[string]string{"host": host}
	for name, values := range req.Header {
		key := strings.ToLower(name)
		if key == "authorization" {
			continue
		}
		trimmed := make([]string, len(values))
		for i, v := range values {
			trimmed[i] = strings.Join(strings.Fields(v), " ")
		}
		headers[key] = strings.Join(trimmed, ",")
	}
	names := make([]string, 0, len(headers))
	for name := range headers {
		names = append(names, name)
	}
	slices.Sort(names)
	var canonicalHeaders strings.Builder
	for _, name := range names {
		canonicalHeaders.WriteString(name + ":" + headers[name] + "\n")
	}
	signedHeaders := strings.Join(names, ";")

	canonicalRequest := strings.Join([]string{
		req.Method,
		canonicalURI(req.URL),
		canonicalQuery(req.URL),
		canonicalHeaders.String(),
		signedHeaders,
		payloadHash,
	}, "\n")
	scope := day + "/" + region + "/" + service + "/aws4_request"
	crHash := sha256.Sum256([]byte(canonicalRequest))
	stringToSign := "AWS4-HMAC-SHA256\n" + amzDate + "\n" + scope + "\n" + hex.EncodeToString(crHash[:])

	key := hmacSHA256([]byte("AWS4"+secretAccessKey), day)
	key = hmacSHA256(key, region)
	key = hmacSHA256(key, service)
	key = hmacSHA256(key, "aws4_request")
	signature := hex.EncodeToString(hmacSHA256(key, stringToSign))

	req.Header.Set("Authorization", "AWS4-HMAC-SHA256 Credential="+accessKeyID+"/"+scope+
		", SignedHeaders="+signedHeaders+", Signature="+signature)
}

func hmacSHA256(key []byte, data string) []byte {
	h := hmac.New(sha256.New, key)
	h.Write([]byte(data))
	return h.Sum(nil)
}

// uriEncode percent-encodes s as SigV4 wants: everything but unreserved
// characters, and '/' too unless keepSlash.
func uriEncode(s string, keepSlash bool) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case 'A' <= c && c <= 'Z', 'a' <= c && c <= 'z', '0' <= c && c <= '9',
			c == '-', c == '_', c == '.', c == '~', c == '/' && keepSlash:
			b.WriteByte(c)
		default:
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String()
}

// canonicalURI is the path of u, encoded once, as S3 expects.
func canonicalURI(u *url.URL) string {
	if u.Path == "" {
		return "/"
	}
	return uriEncode(u.Path, true)
}

// canonicalQuery is the query of u with sorted, encoded parameters.
func canonicalQuery(u *url.URL) string {
	q := u.Query()
	var parts []string
	for key, values := range q {
		for _, v := range values {
			parts = append(parts, uriEncode(key, false)+"="+uriEncode(v, false))
		}
	}
	slices.Sort(parts)
	return strings.Join(parts, "&")
}
