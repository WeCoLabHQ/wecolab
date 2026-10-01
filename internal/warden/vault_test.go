package warden

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"regexp"
	"slices"
	"strings"
	"testing"
)

func TestVaultName(t *testing.T) {
	re := regexp.MustCompile(`^[a-z0-9-]{6,63}$`)
	for _, c := range [][2]string{{"fab.example.org", "team"}, {"a.b", "x"}, {"very-long-fabric-domain-name.example.org", "a-project-with-a-long-name-too"}} {
		n := VaultName(c[0], c[1])
		if !re.MatchString(n) || len(n) < 6 || len(n) > 63 || n[:3] == "b2-" {
			t.Errorf("bad bucket name %q", n)
		}
	}
}

// Rotating a vault's key: sign in with the account key, find the bucket, make a key for it alone; then
// delete the old one. A bucket the account does not hold is refused.
func TestVaultKeyRotation(t *testing.T) {
	calls := []string{}
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		name := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
		calls = append(calls, name)
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		switch name {
		case "b2_authorize_account":
			if r.Header.Get("Authorization") != "Basic "+base64.StdEncoding.EncodeToString([]byte("acct:secret")) {
				t.Errorf("authorize with %q", r.Header.Get("Authorization"))
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"accountId": "A", "authorizationToken": "T",
				"apiInfo": map[string]any{"storageApi": map[string]any{"apiUrl": srv.URL, "s3ApiUrl": "https://s3.example"}}})
		case "b2_list_buckets":
			if body["bucketName"] == "wcl-x" {
				_ = json.NewEncoder(w).Encode(map[string]any{"buckets": []any{map[string]any{"bucketId": "B"}}})
			} else {
				_ = json.NewEncoder(w).Encode(map[string]any{"buckets": []any{}})
			}
		case "b2_create_key":
			if body["bucketId"] != "B" || body["accountId"] != "A" || r.Header.Get("Authorization") != "T" {
				t.Errorf("a key for the bucket alone: %v", body)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"applicationKeyId": "new", "applicationKey": "k"})
		case "b2_delete_key":
			if body["applicationKeyId"] != "old" {
				t.Errorf("deleted %v", body)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{})
		}
	}))
	defer srv.Close()
	b2API = srv.URL
	defer func() { b2API = "https://api.backblazeb2.com" }()
	ctx := context.Background()
	v, err := NewVaultKey(ctx, "acct", "secret", "wcl-x")
	if err != nil || v.KeyID != "new" || v.Key != "k" || v.Bucket != "wcl-x" || v.Endpoint != "https://s3.example" {
		t.Fatalf("%+v %v", v, err)
	}
	if err := DeleteVaultKey(ctx, "acct", "secret", "old"); err != nil {
		t.Fatal(err)
	}
	if _, err := NewVaultKey(ctx, "acct", "secret", "someone-elses"); err == nil {
		t.Fatal("a bucket the account does not hold")
	}
	if !slices.Contains(calls, "b2_delete_key") {
		t.Fatalf("calls: %v", calls)
	}
}
