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

// b2API is Backblaze B2's native API; tests point it elsewhere.
var b2API = "https://api.backblazeb2.com"

// b2 is a signed-in B2 session of an account key.
type b2 struct {
	account, token, api, s3 string
	ctx                     context.Context
}

func b2Login(ctx context.Context, keyID, key string) (*b2, error) {
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
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, b2API+"/b2api/v3/b2_authorize_account", nil)
	req.Header.Set("Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte(keyID+":"+key)))
	if err := doJSON(req, &auth); err != nil {
		return nil, fmt.Errorf("b2 authorize: %w", err)
	}
	return &b2{account: auth.AccountID, token: auth.Token, api: auth.APIInfo.StorageAPI.APIURL + "/b2api/v3/", s3: auth.APIInfo.StorageAPI.S3APIURL, ctx: ctx}, nil
}

func (b *b2) call(name string, body map[string]any, out any) error {
	if _, ok := body["accountId"]; !ok && name != "b2_delete_key" {
		body["accountId"] = b.account
	}
	j, _ := json.Marshal(body)
	r, _ := http.NewRequestWithContext(b.ctx, http.MethodPost, b.api+name, strings.NewReader(string(j)))
	r.Header.Set("Authorization", b.token)
	if err := doJSON(r, out); err != nil {
		return fmt.Errorf("%s: %w", name, err)
	}
	return nil
}

// vaultKey makes a key that reaches only the bucket: what a project's databases archive with.
func (b *b2) vaultKey(bucket, bucketID string) (Vault, error) {
	var k struct {
		KeyID string `json:"applicationKeyId"`
		Key   string `json:"applicationKey"`
	}
	caps := []string{"listBuckets", "listFiles", "readFiles", "writeFiles", "deleteFiles", "readBucketRetentions", "readFileRetentions", "writeFileRetentions"}
	if err := b.call("b2_create_key", map[string]any{"capabilities": caps, "keyName": bucket, "bucketId": bucketID}, &k); err != nil {
		return Vault{}, err
	}
	return Vault{KeyID: k.KeyID, Key: k.Key, Bucket: bucket, Endpoint: b.s3}, nil
}

// CreateVault makes a project's vault on B2 with an account key: a private bucket
// with Object Lock and a 30-day compliance default retention, and a key restricted
// to it. The account key needs listBuckets, writeBuckets, writeBucketRetentions,
// listKeys and writeKeys (deleteKeys to rotate); it is used once and never stored with the project.
func CreateVault(ctx context.Context, accountKeyID, accountKey, bucket string) (Vault, error) {
	b, err := b2Login(ctx, accountKeyID, accountKey)
	if err != nil {
		return Vault{}, err
	}
	var bk struct {
		ID string `json:"bucketId"`
	}
	if err := b.call("b2_create_bucket", map[string]any{"bucketName": bucket, "bucketType": "allPrivate", "fileLockEnabled": true}, &bk); err != nil {
		return Vault{}, err
	}
	if err := b.call("b2_update_bucket", map[string]any{"bucketId": bk.ID,
		"defaultRetention": map[string]any{"mode": "compliance", "period": map[string]any{"duration": 30, "unit": "days"}}}, &struct{}{}); err != nil {
		return Vault{}, err
	}
	return b.vaultKey(bucket, bk.ID)
}

// NewVaultKey makes another key for a vault the account key's account holds. The old one keeps working
// until DeleteVaultKey: every site must have the new one first.
func NewVaultKey(ctx context.Context, accountKeyID, accountKey, bucket string) (Vault, error) {
	b, err := b2Login(ctx, accountKeyID, accountKey)
	if err != nil {
		return Vault{}, err
	}
	var list struct {
		Buckets []struct {
			ID string `json:"bucketId"`
		} `json:"buckets"`
	}
	if err := b.call("b2_list_buckets", map[string]any{"bucketName": bucket}, &list); err != nil {
		return Vault{}, err
	}
	if len(list.Buckets) != 1 {
		return Vault{}, fmt.Errorf("the account key's account has no bucket %s: rotate its key where the bucket is", bucket)
	}
	return b.vaultKey(bucket, list.Buckets[0].ID)
}

// DeleteVaultKey deletes a key at B2: whoever still holds a copy can no longer reach the vault.
func DeleteVaultKey(ctx context.Context, accountKeyID, accountKey, keyID string) error {
	b, err := b2Login(ctx, accountKeyID, accountKey)
	if err != nil {
		return err
	}
	return b.call("b2_delete_key", map[string]any{"applicationKeyId": keyID}, &struct{}{})
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
