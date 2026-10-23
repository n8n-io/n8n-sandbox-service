package archivestore

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

// S3 keeps archives in an S3-compatible bucket. Runners get presigned URLs;
// only the API holds the credentials.
type S3 struct {
	endpoint  *url.URL // e.g. https://s3.eu-west-1.amazonaws.com; path-style addressing
	bucket    string
	region    string
	accessKey string
	secretKey string
	client    *http.Client
	now       func() time.Time
}

// NewS3 returns a path-style S3 store.
func NewS3(endpoint, bucket, region, accessKey, secretKey string) (*S3, error) {
	u, err := url.Parse(strings.TrimRight(endpoint, "/"))
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		return nil, fmt.Errorf("S3 endpoint %q must be an absolute http(s) URL", endpoint)
	}
	if bucket == "" || region == "" || accessKey == "" || secretKey == "" {
		return nil, fmt.Errorf("S3 archive store needs a bucket, a region and credentials")
	}
	return &S3{endpoint: u, bucket: bucket, region: region, accessKey: accessKey, secretKey: secretKey,
		client: &http.Client{Timeout: time.Minute, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }},
		now:    time.Now}, nil
}

func (s *S3) objectPath(object string) string {
	return "/" + s.bucket + "/" + object
}

// presign returns a SigV4 query-signed URL for method on object. A PUT also
// signs If-None-Match: *, so its URL writes the object once and never
// replaces it.
func (s *S3) presign(method, object string, expires time.Duration) (string, error) {
	if !ValidObject(object) {
		return "", fmt.Errorf("invalid archive object %q", object)
	}
	return s.presignPath(method, s.objectPath(object), expires, method == http.MethodPut), nil
}

// presignPath signs method on an already-built path; ifNoneMatch adds the
// If-None-Match: * header to what is signed.
func (s *S3) presignPath(method, path string, expires time.Duration, ifNoneMatch bool) string {
	now := s.now().UTC()
	date := now.Format("20060102")
	scope := date + "/" + s.region + "/s3/aws4_request"
	q := url.Values{}
	q.Set("X-Amz-Algorithm", "AWS4-HMAC-SHA256")
	q.Set("X-Amz-Credential", s.accessKey+"/"+scope)
	q.Set("X-Amz-Date", now.Format("20060102T150405Z"))
	q.Set("X-Amz-Expires", strconv.Itoa(int(expires/time.Second)))
	headers, signed := "host:"+s.endpoint.Host+"\n", "host"
	if ifNoneMatch {
		headers, signed = headers+"if-none-match:*\n", signed+";if-none-match"
	}
	q.Set("X-Amz-SignedHeaders", signed)
	canonical := strings.Join([]string{method, uriEncode(path), canonicalQuery(q), headers, signed, "UNSIGNED-PAYLOAD"}, "\n")
	toSign := strings.Join([]string{"AWS4-HMAC-SHA256", now.Format("20060102T150405Z"), scope, hexSHA256(canonical)}, "\n")
	q.Set("X-Amz-Signature", hex.EncodeToString(hmacSHA256(s.signingKey(date), toSign)))
	return s.endpoint.Scheme + "://" + s.endpoint.Host + uriEncode(path) + "?" + canonicalQuery(q)
}

func (s *S3) signingKey(date string) []byte {
	k := hmacSHA256([]byte("AWS4"+s.secretKey), date)
	k = hmacSHA256(k, s.region)
	k = hmacSHA256(k, "s3")
	return hmacSHA256(k, "aws4_request")
}

func (s *S3) UploadURL(object string) (string, error) {
	return s.presign(http.MethodPut, object, urlLifetime)
}
func (s *S3) DownloadURL(object string) (string, error) {
	return s.presign(http.MethodGet, object, urlLifetime)
}

func (s *S3) do(ctx context.Context, method, object string) (*http.Response, error) {
	u, err := s.presign(method, object, time.Minute)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, method, u, nil)
	if err != nil {
		return nil, err
	}
	return s.client.Do(req)
}

func (s *S3) Size(ctx context.Context, object string) (int64, error) {
	resp, err := s.do(ctx, http.MethodHead, object)
	if err != nil {
		return 0, withoutURL(err)
	}
	resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK:
		return resp.ContentLength, nil
	case http.StatusNotFound:
		return 0, ErrNotFound
	}
	return 0, fmt.Errorf("S3 HEAD answered %d", resp.StatusCode)
}

func (s *S3) Delete(ctx context.Context, object string) error {
	if !ValidObject(object) {
		return nil
	}
	resp, err := s.do(ctx, http.MethodDelete, object)
	if err != nil {
		return withoutURL(err)
	}
	resp.Body.Close()
	if resp.StatusCode/100 != 2 && resp.StatusCode != http.StatusNotFound {
		return fmt.Errorf("S3 DELETE answered %d", resp.StatusCode)
	}
	return nil
}

func hmacSHA256(key []byte, data string) []byte {
	m := hmac.New(sha256.New, key)
	m.Write([]byte(data))
	return m.Sum(nil)
}

func hexSHA256(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

// uriEncode encodes per SigV4: everything but unreserved characters and '/'.
func uriEncode(p string) string {
	var b strings.Builder
	for i := 0; i < len(p); i++ {
		c := p[i]
		if (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '-' || c == '_' || c == '.' || c == '~' || c == '/' {
			b.WriteByte(c)
		} else {
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String()
}

func canonicalQuery(q url.Values) string {
	keys := make([]string, 0, len(q))
	for k := range q {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, uriEncode(k)+"="+strings.ReplaceAll(uriEncode(q.Get(k)), "/", "%2F"))
	}
	return strings.Join(parts, "&")
}

// withoutURL drops the presigned URL, and so the access key id, a transport
// error carries.
func withoutURL(err error) error {
	var ue *url.Error
	if errors.As(err, &ue) {
		return fmt.Errorf("S3 %s: %w", ue.Op, ue.Err)
	}
	return err
}
