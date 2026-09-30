package warden

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// VaultStatus is what the vault itself says, independent of the primary.
type VaultStatus struct {
	LatestBackup time.Time `json:"latestBackup"`         // newest base backup (backup.info written at completion)
	LatestWAL    time.Time `json:"latestWAL"`            // newest archived WAL segment
	ObjectLock   string    `json:"objectLock,omitempty"` // e.g. "compliance 30 days", "" when not enabled
	Err          string    `json:"error,omitempty"`
}

// VaultOf inspects a database's vault with the ObjectStore and credentials c can read: the app's own
// keys, at the site that is primary.
func VaultOf(ctx context.Context, c client.Reader, ns, db, serverName string) VaultStatus {
	os := &unstructured.Unstructured{}
	os.SetGroupVersionKind(gvkObjectStore)
	if err := c.Get(ctx, types.NamespacedName{Namespace: ns, Name: db + "-vault"}, os); err != nil {
		return VaultStatus{Err: fmt.Sprintf("objectstore: %v", err)}
	}
	cfg, _, _ := unstructured.NestedMap(os.Object, "spec", "configuration")
	endpoint, _ := cfg["endpointURL"].(string)
	dest, _ := cfg["destinationPath"].(string)
	keyID, err := secretValue(ctx, c, ns, cfg, "accessKeyId")
	if err != nil {
		return VaultStatus{Err: err.Error()}
	}
	key, err := secretValue(ctx, c, ns, cfg, "secretAccessKey")
	if err != nil {
		return VaultStatus{Err: err.Error()}
	}
	bucket, prefix := splitS3(dest)
	return InspectS3(ctx, S3{Endpoint: endpoint, KeyID: keyID, Key: key}, bucket, prefix+serverName+"/")
}

func secretValue(ctx context.Context, c client.Reader, ns string, cfg map[string]any, field string) (string, error) {
	ref, _, _ := unstructured.NestedMap(cfg, "s3Credentials", field)
	name, _ := ref["name"].(string)
	k, _ := ref["key"].(string)
	sec := &corev1.Secret{}
	if err := c.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, sec); err != nil {
		return "", fmt.Errorf("vault secret %s: %w", name, err)
	}
	v, ok := sec.Data[k]
	if !ok {
		return "", fmt.Errorf("vault secret %s has no key %s", name, k)
	}
	return string(v), nil
}

// splitS3 turns s3://bucket/some/prefix/ into ("bucket", "some/prefix/").
func splitS3(dest string) (string, string) {
	dest = strings.TrimPrefix(dest, "s3://")
	bucket, prefix, _ := strings.Cut(dest, "/")
	if prefix != "" && !strings.HasSuffix(prefix, "/") {
		prefix += "/"
	}
	return bucket, prefix
}

// b2Inspect authorizes, reads the bucket's Object Lock configuration, and finds
// the newest backup.info and WAL object under prefix.
func doJSON(req *http.Request, out any) error {
	req.Header.Set("Accept", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		var e struct{ Code, Message string }
		_ = json.NewDecoder(resp.Body).Decode(&e)
		return fmt.Errorf("%s: %s %s", resp.Status, e.Code, e.Message)
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// Vault is what a project needs to archive: a bucket with Object Lock and a key that
// reaches only that bucket.
type Vault struct{ KeyID, Key, Bucket, Endpoint string }

// CreateVault makes a project's vault on B2 with an account key: a private bucket
// with Object Lock and a 30-day compliance default retention, and a key restricted
// to it. The account key needs listBuckets, writeBuckets, writeBucketRetentions,
// listKeys and writeKeys; it is used once and never stored with the project.
func CreateVault(ctx context.Context, accountKeyID, accountKey, bucket string) (Vault, error) {
	var auth struct {
		AccountID string `json:"accountId"`
		Token     string `json:"authorizationToken"`
		APIInfo   struct {
			StorageAPI struct {
				APIURL   string `json:"apiUrl"`
				S3APIURL string `json:"s3ApiUrl"`
			} `json:"storageApi"`
		} `json:"apiInfo"`
	}
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "https://api.backblazeb2.com/b2api/v3/b2_authorize_account", nil)
	req.Header.Set("Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte(accountKeyID+":"+accountKey)))
	if err := doJSON(req, &auth); err != nil {
		return Vault{}, fmt.Errorf("b2 authorize: %w", err)
	}
	api := auth.APIInfo.StorageAPI.APIURL + "/b2api/v3/"
	call := func(name string, body any, out any) error {
		b, _ := json.Marshal(body)
		r, _ := http.NewRequestWithContext(ctx, http.MethodPost, api+name, strings.NewReader(string(b)))
		r.Header.Set("Authorization", auth.Token)
		return doJSON(r, out)
	}
	var bk struct {
		ID string `json:"bucketId"`
	}
	if err := call("b2_create_bucket", map[string]any{"accountId": auth.AccountID, "bucketName": bucket, "bucketType": "allPrivate", "fileLockEnabled": true}, &bk); err != nil {
		return Vault{}, fmt.Errorf("b2 create bucket: %w", err)
	}
	if err := call("b2_update_bucket", map[string]any{"accountId": auth.AccountID, "bucketId": bk.ID,
		"defaultRetention": map[string]any{"mode": "compliance", "period": map[string]any{"duration": 30, "unit": "days"}}}, &struct{}{}); err != nil {
		return Vault{}, fmt.Errorf("b2 object lock: %w", err)
	}
	var k struct {
		KeyID string `json:"applicationKeyId"`
		Key   string `json:"applicationKey"`
	}
	caps := []string{"listBuckets", "listFiles", "readFiles", "writeFiles", "deleteFiles", "readBucketRetentions", "readFileRetentions", "writeFileRetentions"}
	if err := call("b2_create_key", map[string]any{"accountId": auth.AccountID, "capabilities": caps, "keyName": bucket, "bucketId": bk.ID}, &k); err != nil {
		return Vault{}, fmt.Errorf("b2 create key: %w", err)
	}
	return Vault{KeyID: k.KeyID, Key: k.Key, Bucket: bucket, Endpoint: auth.APIInfo.StorageAPI.S3APIURL}, nil
}

// VaultName is a bucket name for a project: globally unique, 6 to 63 characters of
// letters, digits and hyphens, never starting with b2-.
func VaultName(fabric, project string) string {
	clean := func(s string) string {
		return strings.Trim(strings.Map(func(r rune) rune {
			if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' {
				return r
			}
			return '-'
		}, strings.ToLower(s)), "-")
	}
	n := "wcl-" + clean(fabric) + "-" + clean(project) + "-" + randHex(3)
	if len(n) > 63 {
		n = n[:57] + "-" + randHex(2)
	}
	return n
}

func randHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
