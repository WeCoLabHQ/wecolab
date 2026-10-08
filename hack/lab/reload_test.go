package lab

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The fake SSH runs the writer's stdin script, rather than accepting an image-build command as text.
func TestReloadArchitectures(t *testing.T) {
	for _, tc := range []struct {
		name, writer, siteA, siteB, failure string
		success                             bool
	}{
		{"mixed-arm-writer", "aarch64", "x86_64", "aarch64", "", true},
		{"mixed-amd-writer", "x86_64", "aarch64", "x86_64", "", true},
		{"unknown-manager", "x86_64", "aarch64", "mips64", "", false},
		{"unknown-writer", "mips64", "x86_64", "aarch64", "", false},
		{"copy-failure", "aarch64", "x86_64", "aarch64", "copy", false},
		{"import-failure", "aarch64", "x86_64", "aarch64", "import", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			stub := `#!/bin/bash
set -e
printf '%s %s\n' "$(basename "$0")" "$*" >> "$EVENTS"
if [ "$(basename "$0")" = go ]; then
 out=""; while [ $# -gt 0 ]; do [ "$1" != -o ] || { out=$2; shift; }; shift; done
 mkdir -p "$(dirname "$out")"; printf 'binary' > "$out"; exit
fi
if [ "$(basename "$0")" = scp ]; then
 [[ "$*" != *"/var/lib/wecolab/dist/"* ]] || [ "$FAILURE" != copy ] || exit 1
 exit
fi
host=""; for arg in "$@"; do case "$arg" in writer|siteA|siteB) host=$arg; break;; esac; done
case "$*" in
 *"uname -m"*) case "$host" in writer) echo "$WRITER_ARCH";; siteA) echo "$SITE_A_ARCH";; siteB) echo "$SITE_B_ARCH";; esac; exit;;
 *"jsonpath="*) echo 'ghcr.io/test/warden:v1'; exit;;
 *"bash -s"*)
   export V=v1 R=ghcr.io/test COMPONENTS=warden
   uname() { echo "$WRITER_ARCH"; }
   sudo() { "$@"; }
   export -f uname sudo
   mkdir -p "$IMAGES"
   for arch in amd64 arm64; do
     printf '#!/bin/sh\nout=\nwhile [ "$#" -gt 0 ]; do [ "$1" != --out ] || { out=$2; shift; }; shift; done\nprintf tar > "$out"\n' > "$IMAGES/warden-$arch"
     chmod +x "$IMAGES/warden-$arch"
   done
   script=$(cat)
   script=${script//D=\/var\/lib\/wecolab\/dist B=\/tmp\/wcl-reload/D=$IMAGES B=$IMAGES}
   bash -c "$script"; exit;;
 *"ctr images import"*) [ "$FAILURE" != import ] || exit 1;;
esac
`
			for _, name := range []string{"go", "ssh", "scp"} {
				if err := os.WriteFile(filepath.Join(dir, name), []byte(stub), 0o755); err != nil {
					t.Fatal(err)
				}
			}
			events := filepath.Join(dir, "events")
			cmd := exec.Command("bash", "reload.sh", "warden")
			cmd.Env = append(os.Environ(), "PATH="+dir+":"+os.Getenv("PATH"), "LAB_WRITER=writer", "LAB_SITES=siteA siteB", "EVENTS="+events, "IMAGES="+filepath.Join(dir, "images"), "WRITER_ARCH="+tc.writer, "SITE_A_ARCH="+tc.siteA, "SITE_B_ARCH="+tc.siteB, "FAILURE="+tc.failure)
			out, err := cmd.CombinedOutput()
			if (err == nil) != tc.success {
				t.Fatalf("exit %v: %s", err, out)
			}
			b, _ := os.ReadFile(events)
			log := string(b)
			if !tc.success && (strings.Contains(tc.name, "unknown")) {
				if strings.Contains(log, "ctr images import") || strings.Contains(log, "delete pod") {
					t.Fatalf("unknown architecture mutated manager: %s", log)
				}
				return
			}
			for _, c := range []struct{ host, machine string }{{"writer", tc.writer}, {"siteA", tc.siteA}, {"siteB", tc.siteB}} {
				arch := "amd64"
				if c.machine == "aarch64" || c.machine == "arm64" {
					arch = "arm64"
				}
				if !tc.success && tc.failure != "" && c.host != "writer" {
					break
				}
				tar := "warden-v1-" + arch + ".tar"
				if !strings.Contains(log, "ssh -n -o BatchMode=yes "+c.host+" sudo sh -c 'cp ") || !strings.Contains(log, "/var/lib/rancher/k3s/agent/images/"+tar) || !strings.Contains(log, "k3s ctr images label ghcr.io/test/warden:v1") {
					t.Errorf("%s does not import/pin %s: %s", c.host, tar, log)
				}
				if c.host != "writer" && !strings.Contains(log, "writer:/var/lib/wecolab/dist/"+tar) {
					t.Errorf("wrong transfer for %s: %s", c.host, log)
				}
			}
			if !tc.success && tc.failure != "" && strings.Contains(log, "ssh -n -o BatchMode=yes siteA sudo k3s kubectl -n wecolab-system delete pod") {
				t.Errorf("failed manager restarted: %s", log)
			}
		})
	}
}
