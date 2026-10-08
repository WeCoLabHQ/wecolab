package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"strings"
	"sync"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"wecolab.io/wecolab/api/v1alpha1"
	"wecolab.io/wecolab/internal/fabric"
	"wecolab.io/wecolab/internal/warden"
)

func TestVaultRekeyCannotRegressAfterLaterRotation(t *testing.T) {
	age := fakeSops(t)
	app := &v1alpha1.App{ObjectMeta: metav1.ObjectMeta{Namespace: "p", Name: "wiki"}, Spec: v1alpha1.AppSpec{Database: "wiki-db", ArchiveID: "wiki-db", Sites: []string{"a"}, Primary: "a"}}
	git := inGit(t, app)
	vault := func(id, key, version, revision string) fabric.FileChange {
		f, err := secretFile(vaultSecret("p"), map[string]string{"b2-key-id": id, "b2-key": key, "bucket": "wcl-x", "key-version": version, "mutation-revision": revision}, []string{testAge})
		if err != nil {
			t.Fatal(err)
		}
		return f
	}
	k1, k2 := vault("k1", "one", "1", "1"), vault("k2", "two", "2", "3")
	git[k1.Path] = string(k1.Content)
	appPath := fabric.AppFolder("p", "wiki") + "/secret-wiki.sops.yaml"
	git[appPath] = "apiVersion: v1\nkind: Secret\nmetadata: {name: wiki, namespace: p}\nstringData: {b2-key-id: k0, b2-key: zero, key-version: '0', db-password: keep}\nsops:\n  age:\n    - recipient: " + testAge + "\n"
	s, copy := testServer(t, git, age)
	key, err := s.siteKey(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	copy.pausePost = func(raw []byte) {
		var body struct {
			Files []struct{ Path, Content string }
		}
		if json.Unmarshal(raw, &body) != nil {
			return
		}
		for _, f := range body.Files {
			if f.Path == appPath {
				b, _ := base64.StdEncoding.DecodeString(f.Content)
				if strings.Contains(string(b), "b2-key-id: k1") {
					once.Do(func() { close(entered); <-release })
				}
			}
		}
	}
	done := make(chan error, 1)
	go func() { done <- warden.ConvergeVaultApps(context.Background(), s.git, key, "p") }()
	<-entered
	_, err = s.git.Edit(context.Background(), fabric.Author{}, "second rotation", []string{k2.Path}, func(*fabric.Snapshot) ([]fabric.FileChange, error) { return []fabric.FileChange{k2}, nil })
	if err != nil {
		close(release)
		t.Fatal(err)
	}
	if err := warden.ConvergeVaultApps(context.Background(), s.git, key, "p"); err != nil {
		close(release)
		t.Fatal(err)
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	sec, err := s.openSecret(context.Background(), []byte(copy.get(appPath)), appPath)
	if err != nil {
		t.Fatal(err)
	}
	if sec["b2-key-id"] != "k2" || sec["b2-key"] != "two" || sec["key-version"] != "2" || sec["db-password"] != "keep" {
		t.Fatalf("paused K1 repair regressed K2: %v", sec)
	}
}
