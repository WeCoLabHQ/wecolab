package warden

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/xml"
	"fmt"
	"net/http"
	"net/url"
	"path"
	"sort"
	"strings"
	"time"
)

// S3 is an S3-compatible object store: B2, R2, AWS, MinIO. Only what the vault check needs: listing
// and the bucket's Object Lock, signed with AWS Signature Version 4.
type S3 struct {
	Endpoint string // https://s3.eu-central-003.backblazeb2.com
	KeyID    string
	Key      string
	Dev      bool         // set only from the fabric's explicit development setting
	http     *http.Client // package-private fixture transport; production uses guarded clients
}

// Region is read from the endpoint's host where providers put it; anything else signs as us-east-1.
func (s S3) Region() string {
	u, err := url.Parse(s.Endpoint)
	if err != nil {
		return "us-east-1"
	}
	parts := strings.Split(u.Hostname(), ".")
	if len(parts) >= 4 && parts[0] == "s3" && (strings.HasSuffix(u.Hostname(), ".backblazeb2.com") || strings.HasSuffix(u.Hostname(), ".amazonaws.com")) {
		return parts[1]
	}
	return "us-east-1"
}

// get signs and sends a path-style bucket request.
func (s S3) get(ctx context.Context, bucket string, q url.Values) ([]byte, error) {
	return s.getPath(ctx, bucket, "", q, 16<<20)
}

func (s S3) getObject(ctx context.Context, bucket, key string) ([]byte, error) {
	if key == "" || strings.HasPrefix(key, "/") || path.Clean(key) != key || strings.Contains(key, "\\") {
		return nil, fmt.Errorf("invalid archive object key")
	}
	return s.getPath(ctx, bucket, key, nil, 1<<20)
}

func (s S3) getPath(ctx context.Context, bucket, key string, q url.Values, limit int64) ([]byte, error) {
	u, err := url.Parse(s.Endpoint)
	if err != nil || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" ||
		(u.Scheme != "https" && !(s.Dev && u.Scheme == "http")) {
		return nil, fmt.Errorf("vault endpoint must be an HTTPS URL without credentials, query or fragment")
	}
	if bucket == "" || strings.ContainsAny(bucket, "/\\") || bucket == "." || bucket == ".." {
		return nil, fmt.Errorf("invalid vault bucket")
	}
	u.Path = strings.TrimRight(u.Path, "/") + "/" + bucket
	if key != "" {
		u.Path += "/" + key
	}
	u.RawPath, u.RawQuery = "", canonicalQuery(q)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, err
	}
	sign(req, s.KeyID, s.Key, s.Region(), time.Now().UTC())
	hc := productionS3Client
	if s.Dev {
		hc = developmentS3Client
	}
	if s.http != nil {
		hc = s.http
	}
	res, err := hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	b, err := readLimited(res.Body, limit)
	if err != nil {
		return nil, fmt.Errorf("read vault response: %w", err)
	}
	if res.StatusCode != http.StatusOK {
		var e struct{ Code, Message string }
		_ = xml.Unmarshal(b, &e)
		return nil, fmt.Errorf("%s: %s %s", res.Status, e.Code, e.Message)
	}
	return b, nil
}

const emptySHA256 = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"

// sign adds Signature Version 4 headers to a request with no body. Every header already on the
// request is signed, with host.
func sign(req *http.Request, keyID, secret, region string, now time.Time) {
	amz := now.Format("20060102T150405Z")
	day := now.Format("20060102")
	req.Header.Set("x-amz-date", amz)
	if req.Header.Get("x-amz-content-sha256") == "" {
		req.Header.Set("x-amz-content-sha256", emptySHA256)
	}
	headers := map[string]string{"host": req.URL.Host}
	for k, v := range req.Header {
		headers[strings.ToLower(k)] = strings.TrimSpace(strings.Join(v, ","))
	}
	names := make([]string, 0, len(headers))
	for k := range headers {
		names = append(names, k)
	}
	sort.Strings(names)
	var canon strings.Builder
	for _, k := range names {
		canon.WriteString(k + ":" + headers[k] + "\n")
	}
	signed := strings.Join(names, ";")
	path := req.URL.EscapedPath()
	if path == "" {
		path = "/"
	}
	creq := strings.Join([]string{req.Method, path, canonicalQuery(req.URL.Query()), canon.String(), signed, req.Header.Get("x-amz-content-sha256")}, "\n")
	scope := day + "/" + region + "/s3/aws4_request"
	sum := sha256.Sum256([]byte(creq))
	sts := "AWS4-HMAC-SHA256\n" + amz + "\n" + scope + "\n" + hex.EncodeToString(sum[:])
	k := hmacSHA256([]byte("AWS4"+secret), day)
	k = hmacSHA256(k, region)
	k = hmacSHA256(k, "s3")
	k = hmacSHA256(k, "aws4_request")
	req.Header.Set("Authorization", fmt.Sprintf("AWS4-HMAC-SHA256 Credential=%s/%s,SignedHeaders=%s,Signature=%s",
		keyID, scope, signed, hex.EncodeToString(hmacSHA256(k, sts))))
}

func hmacSHA256(key []byte, data string) []byte {
	m := hmac.New(sha256.New, key)
	m.Write([]byte(data))
	return m.Sum(nil)
}

// canonicalQuery sorts and encodes a query as Signature Version 4 requires: RFC 3986, spaces as %20.
func canonicalQuery(q url.Values) string {
	keys := make([]string, 0, len(q))
	for k := range q {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := []string{}
	for _, k := range keys {
		vs := append([]string(nil), q[k]...)
		sort.Strings(vs)
		for _, v := range vs {
			parts = append(parts, uriEncode(k)+"="+uriEncode(v))
		}
	}
	return strings.Join(parts, "&")
}

func uriEncode(s string) string {
	var b strings.Builder
	for _, c := range []byte(s) {
		if c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-' || c == '_' || c == '.' || c == '~' {
			b.WriteByte(c)
		} else {
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String()
}

// InspectS3 reports validated completed backup metadata, not object upload times.
func InspectS3(ctx context.Context, s S3, bucket, prefix string) VaultStatus {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	st := VaultStatus{}
	observed := time.Now()
	if prefix == "" || !strings.HasSuffix(prefix, "/") {
		return VaultStatus{Err: "archive prefix must end with /"}
	}
	if b, err := s.get(ctx, bucket, url.Values{"object-lock": {""}}); err == nil {
		var lock struct {
			Rule struct {
				DefaultRetention struct {
					Mode  string
					Days  int
					Years int
				}
			}
		}
		if xml.Unmarshal(b, &lock) == nil && lock.Rule.DefaultRetention.Mode != "" {
			r := lock.Rule.DefaultRetention
			if r.Years > 0 {
				st.ObjectLock = fmt.Sprintf("%s %d years", strings.ToLower(r.Mode), r.Years)
			} else {
				st.ObjectLock = fmt.Sprintf("%s %d days", strings.ToLower(r.Mode), r.Days)
			}
		}
	}
	token := ""
	seen := map[string]bool{}
	for {
		q := url.Values{"list-type": {"2"}, "prefix": {prefix}, "max-keys": {"1000"}}
		if token != "" {
			q.Set("continuation-token", token)
		}
		b, err := s.get(ctx, bucket, q)
		if err != nil {
			return VaultStatus{Err: fmt.Sprintf("list %s/%s: %v", bucket, prefix, err)}
		}
		var page struct {
			Contents []struct {
				Key          string
				LastModified time.Time
			}
			IsTruncated           bool
			NextContinuationToken string
		}
		if err := xml.Unmarshal(b, &page); err != nil {
			return VaultStatus{Err: fmt.Sprintf("list %s: %v", bucket, err)}
		}
		for _, o := range page.Contents {
			if !strings.HasPrefix(o.Key, prefix) {
				return VaultStatus{Err: "listing returned an object outside the archive"}
			}
			switch {
			case strings.HasSuffix(o.Key, "/backup.info"):
				data, err := s.getObject(ctx, bucket, o.Key)
				if err != nil {
					return VaultStatus{Err: fmt.Sprintf("read backup metadata: %v", err)}
				}
				evidence, err := backupMetadata(data, o.Key, prefix, observed)
				if err != nil {
					return VaultStatus{Err: fmt.Sprintf("backup %s: %v", o.Key, err)}
				}
				if evidence != nil && evidence.CompletedAt.After(st.LatestBackup) {
					st.BackupEvidence, st.LatestBackup = evidence, evidence.CompletedAt
				}
			case strings.HasPrefix(o.Key, prefix+"wals/") && o.LastModified.After(st.LatestWAL):
				st.LatestWAL = o.LastModified
			}
		}
		if !page.IsTruncated {
			return st
		}
		token = page.NextContinuationToken
		if token == "" || seen[token] {
			return VaultStatus{Err: "invalid listing continuation token"}
		}
		seen[token] = true
	}
}
