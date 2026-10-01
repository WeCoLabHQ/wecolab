package warden

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"path"
	"slices"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"wecolab.io/wecolab/api/v1alpha1"
	"wecolab.io/wecolab/internal/fabric"
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

// B2API is Backblaze B2's native API; tests point it elsewhere.
var B2API = "https://api.backblazeb2.com"

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
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, B2API+"/b2api/v3/b2_authorize_account", nil)
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

// DeleteVaultKey deletes a key of a vault's bucket at B2: whoever still holds a copy can no longer reach
// the vault. A key B2 no longer has counts as deleted, if the account holds the bucket: an account key of
// another account (Settings changed) cannot see the key, which may still work there.
func DeleteVaultKey(ctx context.Context, accountKeyID, accountKey, bucket, keyID string) error {
	b, err := b2Login(ctx, accountKeyID, accountKey)
	if err != nil {
		return err
	}
	if err := b.call("b2_delete_key", map[string]any{"applicationKeyId": keyID}, &struct{}{}); err != nil && (b.hasKey(keyID) || !b.hasBucket(bucket)) {
		return err
	}
	return nil
}

// hasBucket is whether the account holds a bucket.
func (b *b2) hasBucket(bucket string) bool {
	var list struct {
		Buckets []struct{} `json:"buckets"`
	}
	return b.call("b2_list_buckets", map[string]any{"bucketName": bucket}, &list) == nil && len(list.Buckets) == 1
}

// hasKey is whether the account still has a key; true when B2 cannot say.
func (b *b2) hasKey(keyID string) bool {
	start := ""
	for {
		var page struct {
			Keys []struct {
				ID string `json:"applicationKeyId"`
			} `json:"keys"`
			Next string `json:"nextApplicationKeyId"`
		}
		body := map[string]any{"maxKeyCount": 10000}
		if start != "" {
			body["startApplicationKeyId"] = start
		}
		if err := b.call("b2_list_keys", body, &page); err != nil {
			return true
		}
		for _, k := range page.Keys {
			if k.ID == keyID {
				return true
			}
		}
		if page.Next == "" {
			return false
		}
		start = page.Next
	}
}

// StorageSecret is the Secret in wecolab-system holding the object storage account key (Settings):
// key-id and key.
const StorageSecret = "storage"

// Retirable is who a vault's retiring keys still wait for: every site of each live database app of the
// project, until it answers and says its copy of the app's Secret holds the vault's current key (report
// is nil for a site that did not answer). Nothing awaited: no site archives with a retiring key any more,
// so they may be deleted. A deleted app, or one without a database, holds no key.
func Retirable(current string, apps []v1alpha1.App, report func(site string) *SiteStatus) []string {
	awaited := []string{}
	for _, a := range apps {
		if a.Spec.Database == "" || a.Spec.Deleted {
			continue
		}
		for _, site := range a.Spec.Sites {
			if st := report(site); st == nil || st.Site != site || current == "" || st.Apps[a.Namespace+"/"+a.Name].VaultKeyID != current {
				awaited = append(awaited, site)
			}
		}
	}
	slices.Sort(awaited)
	return slices.Compact(awaited)
}

// retire is the writer's part in rotating a vault's key (decision 25). The Console makes the new key and
// marks the old one retiring in the vault's Secret; once every site of the project's database apps reports
// the new one (Retirable), the writer deletes the retiring keys at B2 with the account key and takes them
// out of the vault in Git. Until then they keep working, so a site that has not applied the new key yet
// keeps archiving. The cluster's copy says where to look; what is deleted is also retiring in Git.
func (w *Writer) retire(ctx context.Context) error {
	secs := &corev1.SecretList{}
	if err := w.Client.List(ctx, secs, client.InNamespace(SystemNS)); err != nil {
		return err
	}
	report := func(site string) *SiteStatus {
		st, _ := w.Peers.Get(ctx, site)
		return st
	}
	errs := []error{}
	for _, sec := range secs.Items {
		project, ok := strings.CutPrefix(sec.Name, "vault-")
		checked := strings.Fields(string(sec.Data["retiring"]))
		if !ok || len(checked) == 0 {
			continue
		}
		apps := &v1alpha1.AppList{}
		if err := w.Client.List(ctx, apps, client.InNamespace(project)); err != nil {
			errs = append(errs, err)
			continue
		}
		if awaited := Retirable(string(sec.Data["b2-key-id"]), apps.Items, report); len(awaited) > 0 {
			w.note(ctx, project, "a vault's old keys wait for sites to report its new one", "project", project, "sites", awaited)
			continue
		}
		acct := &corev1.Secret{}
		if err := w.Client.Get(ctx, types.NamespacedName{Namespace: SystemNS, Name: StorageSecret}, acct); err != nil {
			w.note(ctx, project, "no B2 account key in Settings: a vault's old keys stay valid until one is there", "project", project)
			continue
		}
		ageKey, err := siteAgeKey(ctx, w.Client, w.Site)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		p, gone := "secrets/"+sec.Name+".sops.yaml", []string{}
		sha, err := w.Git.Edit(ctx, fabric.Author{Name: "Warden", Email: "warden@" + w.Site}, "project "+project+": its vault's old keys deleted at B2",
			[]string{p}, func(snap *fabric.Snapshot) ([]fabric.FileChange, error) {
				enc, ok := snap.Get(p)
				if !ok {
					return nil, nil
				}
				gone = []string{}
				out, err := reseal(enc, path.Base(p), ageKey, func(v map[string]string) error {
					keep := []string{}
					for _, id := range strings.Fields(v["retiring"]) {
						// Only keys the sites were checked against: one retired in Git since may be the one they hold.
						if !slices.Contains(checked, id) || id == v["b2-key-id"] {
							keep = append(keep, id)
							continue
						}
						if err := DeleteVaultKey(ctx, string(acct.Data["key-id"]), string(acct.Data["key"]), v["bucket"], id); err != nil {
							return fmt.Errorf("vault-%s: delete key %s at B2: %w", project, id, err)
						}
						gone = append(gone, id)
					}
					v["retiring"] = strings.Join(keep, " ")
					if len(keep) == 0 {
						delete(v, "retiring")
					}
					return nil
				})
				if err != nil || len(gone) == 0 {
					return nil, err
				}
				return []fabric.FileChange{{Path: p, Content: out}}, nil
			})
		if err != nil {
			errs = append(errs, err)
			continue
		}
		w.mu.Lock()
		delete(w.notes, project) // the next rotation is told again
		w.mu.Unlock()
		if sha != "" {
			log.FromContext(ctx).Info("a vault's old keys deleted at B2: every site has its new one", "project", project, "keys", gone)
		}
	}
	return errors.Join(errs...)
}

// note logs what a vault waits for once, until it changes.
func (w *Writer) note(ctx context.Context, project, msg string, kv ...any) {
	s := fmt.Sprint(msg, kv)
	w.mu.Lock()
	if w.notes == nil {
		w.notes = map[string]string{}
	}
	same := w.notes[project] == s
	w.notes[project] = s
	w.mu.Unlock()
	if !same {
		log.FromContext(ctx).Info(msg, kv...)
	}
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
