package main

import (
	"crypto/sha256"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

const releaseCommit = "411f2c6802a73aaf5517ed3b3ff01a083312d5c9"

func candidate(t *testing.T) (string, string) {
	t.Helper()
	dir := t.TempDir()
	for _, f := range []string{"install.sh", "VERSION", "source.tar.gz", "warden-amd64", "warden-arm64", "console-amd64", "console-arm64"} {
		if err := os.WriteFile(filepath.Join(dir, f), []byte(f), 0600); err != nil {
			t.Fatal(err)
		}
	}
	script := `#!/bin/sh
case "$*" in
 *"--repo wecolabhq/wecolab"*"--signer-workflow wecolabhq/wecolab/.github/workflows/release.yml"*"--source-ref refs/heads/main"*"--source-digest ` + releaseCommit + `"*"--deny-self-hosted-runners"*) exit 0 ;;
 *) exit 1 ;;
esac
`
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "gh"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("sh", "-c", `for f in install.sh VERSION source.tar.gz warden-amd64 warden-arm64 console-amd64 console-arm64; do printf '%s %s\n' "$(sha256sum "$1/$f" | cut -d' ' -f1)" "$f"; done`, "sh", dir)
	b, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	digests := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		fields := strings.Fields(line)
		digests[fields[1]] = fields[0]
	}
	manifest := `{"schema":1,"repository":"wecolabhq/wecolab","sourceCommit":"` + releaseCommit + `","apiVersion":"wecolab.io/v1alpha1","migrations":["archive-id","vault-key-version","name-claims","placement-revision"],"artifacts":[{"name":"install.sh","platform":"linux/amd64,linux/arm64","sha256":"` + digests["install.sh"] + `"},{"name":"VERSION","platform":"linux/amd64,linux/arm64","sha256":"` + digests["VERSION"] + `"},{"name":"source.tar.gz","platform":"source","sha256":"` + digests["source.tar.gz"] + `"},{"name":"warden-amd64","platform":"linux/amd64","sha256":"` + digests["warden-amd64"] + `"},{"name":"warden-arm64","platform":"linux/arm64","sha256":"` + digests["warden-arm64"] + `"},{"name":"console-amd64","platform":"linux/amd64","sha256":"` + digests["console-amd64"] + `"},{"name":"console-arm64","platform":"linux/arm64","sha256":"` + digests["console-arm64"] + `"}]}`
	writeFile(t, filepath.Join(dir, "release.json"), manifest, 0600)
	return dir, bin
}

func TestReleaseVerificationRejectsUntrustedInputs(t *testing.T) {
	if _, err := exec.LookPath("jq"); err != nil {
		t.Skip("jq unavailable")
	}
	if _, err := exec.LookPath("sha256sum"); err != nil {
		t.Skip("sha256sum unavailable")
	}
	cases := []struct {
		name   string
		mutate func(string, string)
	}{
		{"valid", func(string, string) {}},
		{"wrong digest", func(d, b string) { writeFile(t, d+"/install.sh", "altered", 0600) }},
		{"missing manifest", func(d, b string) {
			if err := os.Remove(d + "/release.json"); err != nil {
				t.Fatal(err)
			}
		}},
		{"missing platform", func(d, b string) {
			payload := readFile(t, d+"/release.json")
			writeFile(t, d+"/release.json", strings.Replace(payload, `"platform":"linux/arm64"`, `"platform":"unsupported"`, 1), 0600)
		}},
		{"wrong signer", func(d, b string) { writeFile(t, b+"/gh", "#!/bin/sh\nexit 1\n", 0700) }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir, bin := candidate(t)
			tc.mutate(dir, bin)
			cmd := exec.Command("bash", "../../hack/release.sh", "verify", dir, releaseCommit)
			cmd.Env = append(os.Environ(), "PATH="+bin+":"+os.Getenv("PATH"))
			out, err := cmd.CombinedOutput()
			if (err == nil) != (tc.name == "valid") {
				t.Fatalf("verify err=%v output=%s", err, out)
			}
		})
	}
}

func TestReleaseVerifyReadsManifestUnderNounset(t *testing.T) {
	if _, err := exec.LookPath("jq"); err != nil {
		t.Skip("jq unavailable")
	}
	if _, err := exec.LookPath("sha256sum"); err != nil {
		t.Skip("sha256sum unavailable")
	}
	dir, bin := candidate(t)
	cmd := exec.Command("bash", "../../hack/release.sh", "verify", dir, releaseCommit)
	cmd.Env = append(os.Environ(), "PATH="+bin+":"+os.Getenv("PATH"))
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("verified release rejected: %v\n%s", err, out)
	}
}

func TestInstallerOnlyRemovesOwnedNetBirdBinary(t *testing.T) {
	for _, owned := range []bool{false, true} {
		dir := scratchBox(t)
		binary := dir + "/netbird"
		writeFile(t, binary, "another client's binary", 0755)
		if owned {
			writeFile(t, dir+"/state/installed", "netbird-direct\n", 0600)
		}
		fn := strings.ReplaceAll(installFns(t, "has", "remove_netbird_binary"), "/usr/local/bin/netbird", binary)
		out, ok := runOn(t, dir, nil, "bash", "-c", fmt.Sprintf("set -euo pipefail\nSTATE=%q\n%s\nremove_netbird_binary", dir+"/state", fn))
		if !ok {
			t.Fatalf("remove owned=%t: %s", owned, out)
		}
		_, err := os.Stat(binary)
		if owned && !os.IsNotExist(err) || !owned && err != nil {
			t.Fatalf("owned=%t binary stat: %v", owned, err)
		}
	}
}

func TestInstallerInterruptedDownloadNeverExecutes(t *testing.T) {
	dir := scratchBox(t)
	writeFile(t, dir+"/bin/curl", "#!/bin/bash\nwhile [ \"$#\" -gt 0 ]; do if [ \"$1\" = -o ]; then shift; printf partial > \"$1\"; exit 7; fi; shift; done\nexit 7\n", 0700)
	script := `CACHE=; DEV=; fetch https://example.invalid/payload "` + dir + `/payload" && touch "` + dir + `/executed"`
	_, ok := bashFns(t, dir, script, "fetch")
	if ok {
		t.Fatal("failed download accepted")
	}
	for _, name := range []string{"payload", "executed"} {
		if _, err := os.Stat(dir + "/" + name); !os.IsNotExist(err) {
			t.Fatalf("%s survived interrupted download: %v", name, err)
		}
	}
}

func TestInstallerRejectsCorruptOfflineCache(t *testing.T) {
	if _, err := exec.LookPath("sha256sum"); err != nil {
		t.Skip("sha256sum unavailable")
	}
	dir := scratchBox(t)
	if err := os.Mkdir(dir+"/cache", 0700); err != nil {
		t.Fatal(err)
	}
	writeFile(t, dir+"/cache/payload", "corrupt", 0600)
	writeFile(t, dir+"/bin/curl", "#!/bin/sh\nexit 99\n", 0700)
	correct := fmt.Sprintf("%x", sha256.Sum256([]byte("correct")))
	script := fmt.Sprintf(`CACHE=%q; DEV=; fetch https://example.invalid/payload %q && verify_payload %q %q && touch %q`,
		dir+"/cache", dir+"/payload", dir+"/payload", correct, dir+"/executed")
	_, ok := bashFns(t, dir, script, "fetch", "verify_payload", "die")
	if ok {
		t.Fatal("corrupt cache accepted")
	}
	if _, err := os.Stat(dir + "/executed"); !os.IsNotExist(err) {
		t.Fatalf("executed corrupt cache: %v", err)
	}
	writeFile(t, dir+"/cache/payload", "correct", 0600)
	_, ok = bashFns(t, dir, script, "fetch", "verify_payload", "die")
	if !ok {
		t.Fatal("valid pinned offline artifact rejected")
	}
}
