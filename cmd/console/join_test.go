package main

import (
	"bytes"
	"crypto/sha256"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"
)

var funcEnd = regexp.MustCompile("\n}\n[\n#]")

// installFns is install.sh's functions named, as bash source.
func installFns(t *testing.T, names ...string) string {
	b, err := os.ReadFile("join.sh")
	if err != nil {
		t.Fatal(err)
	}
	src, out := string(b), ""
	for _, n := range names {
		i := strings.Index(src, "\n"+n+"() {")
		if i < 0 {
			t.Fatalf("join.sh has no function %s", n)
		}
		rest := src[i+1:]
		fn, _, _ := strings.Cut(rest, "\n")
		if !strings.HasSuffix(fn, "}") {
			fn = ""
			for _, end := range funcEnd.FindAllStringIndex(rest, -1) {
				candidate := rest[:end[0]+2]
				parse := exec.Command("bash", "-n")
				parse.Stdin = strings.NewReader(candidate)
				if parse.Run() == nil {
					fn = candidate
					break
				}
			}
			if fn == "" {
				t.Fatalf("no complete bash function %s", n)
			}
		}
		out += fn + "\n"
	}
	return out
}

// runOn runs a command with dir/bin first on its PATH; it returns the output and whether it succeeded.
func runOn(t *testing.T, dir string, env []string, args ...string) (string, bool) {
	for _, tool := range []string{"bash", "jq", "awk"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("no %s", tool)
		}
	}
	cmd := exec.Command(args[0], args[1:]...)
	cmd.Env = append(append(os.Environ(), "PATH="+dir+"/bin:"+os.Getenv("PATH")), env...)
	out, err := cmd.CombinedOutput()
	return string(out), err == nil
}

// scratchBox is a scratch box: install.sh's state in dir/state, root's SSH keys in dir/ssh, stubs in dir/bin.
func scratchBox(t *testing.T) string {
	dir := t.TempDir()
	for _, d := range []string{"state", "ssh", "bin"} {
		if err := os.MkdirAll(filepath.Join(dir, d), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// bashFns runs script after install.sh's functions named, on the box at dir.
func bashFns(t *testing.T, dir, script string, fns ...string) (string, bool) {
	head := fmt.Sprintf("set -euo pipefail\nSTATE=%q BUNDLE=%q NETWORK=10.77.0.0/16 CERTS_PORT=8094 DEV=\n", dir+"/state", dir+"/bundle.json")
	return runOn(t, dir, nil, "bash", "-c", head+strings.ReplaceAll(installFns(t, fns...), "/root/.ssh", dir+"/ssh")+script)
}

func writeFile(t *testing.T, path, s string, mode os.FileMode) {
	if err := os.WriteFile(path, []byte(s), mode); err != nil {
		t.Fatal(err)
	}
}

func readFile(t *testing.T, path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// A re-run on a box that belongs to a fabric converges, and never makes the fabric again.
func TestInstallFabricMade(t *testing.T) {
	for _, c := range []struct {
		file, content string
		made          bool
	}{
		{"", "", false},
		{"done", "", true},
		{"install.json", `{"siteKey": "AGE-SECRET-KEY-1X"}`, false}, // a first run that stopped partway goes on
		{"install.json", `{"box": {}}`, true},                       // its secrets forgotten: done
	} {
		dir := scratchBox(t)
		if c.file != "" {
			writeFile(t, dir+"/state/"+c.file, c.content, 0o600)
		}
		if _, made := bashFns(t, dir, "fabric_made", "fabric_made"); made != c.made {
			t.Errorf("%s %s: made %v", c.file, c.content, made)
		}
	}
}

// The fabric's keys live in a block rewritten each time; the box's own lines stay; nothing else gets in.
func TestInstallSSHKeys(t *testing.T) {
	dir := scratchBox(t)
	keys, own := dir+"/ssh/authorized_keys", "ssh-ed25519 OWN1 me@box\n"
	writeFile(t, keys, own+"# BEGIN WeCoLab\nssh-ed25519 GONE x@y\n# END WeCoLab\nssh-rsa OWN2 me@box\n", 0o644)
	sync := func(json string) (string, bool) {
		writeFile(t, dir+"/keys.json", json, 0o600)
		return bashFns(t, dir, "ssh_keys "+dir+"/keys.json", "ssh_keys")
	}
	if out, ok := sync(`{"sshKeys": ["ssh-ed25519 NEW a@b", "ssh-ed25519 X\ncommand=\"sh\" ssh-rsa Y", "command=\"sh\" ssh-rsa Z", 7]}`); !ok {
		t.Fatal(out)
	}
	got := readFile(t, keys)
	if want := own + "ssh-rsa OWN2 me@box\n# BEGIN WeCoLab"; !strings.HasPrefix(got, want) || !strings.HasSuffix(got, "\nssh-ed25519 NEW a@b\n# END WeCoLab\n") ||
		strings.Count(got, "BEGIN") != 1 || strings.Contains(got, "GONE") || strings.Contains(got, "command=") {
		t.Fatalf("authorized_keys:\n%s", got)
	}
	if fi, _ := os.Stat(keys); fi.Mode().Perm() != 0o600 {
		t.Errorf("mode %v", fi.Mode())
	}
	if _, ok := sync(`{}`); !ok || readFile(t, keys) != got {
		t.Fatal("an answer without sshKeys changes nothing")
	}
	if _, ok := sync(`{"sshKeys": null}`); !ok || readFile(t, keys) != own+"ssh-rsa OWN2 me@box\n" {
		t.Fatalf("no keys, no block:\n%s", readFile(t, keys))
	}
	_ = os.RemoveAll(dir + "/ssh")
	writeFile(t, dir+"/ssh", "", 0o600)
	if _, ok := sync(`{"sshKeys": ["ssh-ed25519 NEW a@b"]}`); ok {
		t.Fatal("ssh_keys says when it failed, set -e or not")
	}
}

// Keys an older install.sh appended one at a time join the block; the box's own stay outside it.
func TestInstallSSHUnappend(t *testing.T) {
	for _, c := range []struct{ keys, want string }{
		{"ssh-ed25519 OWN me@box\nssh-ed25519 APP a@b\n", "ssh-ed25519 OWN me@box\n# BEGIN WeCoLab: the fabric rewrites this block every hour; edits here are lost\nssh-ed25519 APP a@b\n# END WeCoLab\n"},
		{"ssh-ed25519 OWN me@box\nssh-ed25519 APP a@b\n# BEGIN WeCoLab\nssh-ed25519 APP a@b\n# END WeCoLab\n", "ssh-ed25519 OWN me@box\n# BEGIN WeCoLab\nssh-ed25519 APP a@b\n# END WeCoLab\n"},
	} {
		dir := scratchBox(t)
		writeFile(t, dir+"/state/installed", "nebula\nssh-dir\nssh ssh-ed25519 APP a@b\nssh-file\nk3s\n", 0o600)
		writeFile(t, dir+"/ssh/authorized_keys", c.keys, 0o600)
		if out, ok := bashFns(t, dir, "ssh_unappend", "ssh_keys", "ssh_unappend"); !ok {
			t.Fatal(out)
		}
		if got := readFile(t, dir+"/ssh/authorized_keys"); got != c.want {
			t.Errorf("authorized_keys:\n%s", got)
		}
		if got := readFile(t, dir+"/state/installed"); got != "nebula\nk3s\n" {
			t.Errorf("installed:\n%s", got)
		}
	}
}

// The firewall as the box runs it: only Nebula reaches k3s and NetBird's metrics, on both address
// families; pods reach DNS before anything local is dropped.
func TestInstallFirewall(t *testing.T) {
	for _, c := range []struct{ public, v6 bool }{{true, true}, {false, false}} {
		dir := scratchBox(t)
		// iptables and ip6tables log their rules; -C finds none; ip6tables works only with V6.
		writeFile(t, dir+"/bin/iptables", "#!/bin/sh\necho \"$(basename \"$0\") $*\" >> \"$LOG\"\n"+
			"case \" $* \" in *\" -C \"*) exit 1 ;; *\" -nL \"*) [ \"$(basename \"$0\")\" = iptables ] || [ -n \"$V6\" ] ;; esac\n", 0o755)
		if err := os.Link(dir+"/bin/iptables", dir+"/bin/ip6tables"); err != nil {
			t.Fatal(err)
		}
		script, ok := bashFns(t, dir, fmt.Sprintf("firewall_script %v", c.public), "firewall_script")
		if !ok {
			t.Fatal(script)
		}
		writeFile(t, dir+"/fw.sh", script, 0o755)
		v6 := ""
		if c.v6 {
			v6 = "1"
		}
		if out, ok := runOn(t, dir, []string{"LOG=" + dir + "/log", "V6=" + v6}, "sh", dir+"/fw.sh"); !ok {
			t.Fatal(out)
		}
		log := strings.Split(readFile(t, dir+"/log"), "\n")
		at := func(rule string) int { return slices.Index(log, rule) }
		for _, ipt := range []string{"iptables", "ip6tables"} {
			nebula, drop := at(ipt+" -A WECOLAB-HOST -i nebula1 -j RETURN"), at(ipt+" -A WECOLAB-HOST -p tcp -m multiport --dports 6443,10250,9091 -j DROP")
			if ipt == "ip6tables" && !c.v6 {
				if drop >= 0 {
					t.Error("a box without IPv6 gets no IPv6 chain")
				}
				continue
			}
			if nebula < 0 || drop < nebula || at(ipt+" -A WECOLAB-HOST -p udp --dport 8472 -j DROP") < drop || at(ipt+" -I INPUT 1 -j WECOLAB-HOST") < 0 {
				t.Errorf("%s: WECOLAB-HOST is not as it must be:\n%s", ipt, strings.Join(log, "\n"))
			}
		}
		local := at("iptables -t mangle -A WECOLAB-POD -m addrtype --dst-type LOCAL -j DROP")
		dns := at("iptables -t mangle -A WECOLAB-POD -p udp --dport 53 -j RETURN")
		door := at("iptables -t mangle -A WECOLAB-POD -m addrtype --dst-type LOCAL -p tcp -m multiport --dports 80,443 -j RETURN")
		if dns < 0 || local < dns || (door >= 0) != c.public || door > local {
			t.Errorf("public %v: WECOLAB-POD is not as it must be:\n%s", c.public, strings.Join(log, "\n"))
		}
	}
}

// Managers set the agent token nodes join with; nodes hold only that; k3s listens on Nebula.
func TestInstallK3sConfig(t *testing.T) {
	config := func(bundle string) (map[string]any, bool) {
		dir := scratchBox(t)
		writeFile(t, dir+"/bundle.json", bundle, 0o600)
		out, ok := bashFns(t, dir, "k3s_config", "die", "valid", "bundle", "k3s_config")
		m := map[string]any{}
		if err := yaml.Unmarshal([]byte(out), &m); ok && err != nil {
			t.Fatalf("%v:\n%s", err, out)
		}
		return m, ok
	}
	m, ok := config(`{"role": "manager", "ip": "10.77.0.1", "box": "home-box1", "k3sToken": "server-tok", "k3sAgentToken": "agent-tok"}`)
	if !ok || m["bind-address"] != "10.77.0.1" || m["token"] != "server-tok" || m["agent-token"] != "agent-tok" {
		t.Errorf("manager: %v", m)
	}
	m, ok = config(`{"role": "node", "ip": "10.77.0.9", "box": "home-mac", "k3sToken": "agent-tok", "k3sServer": "https://10.77.0.1:6443", "laptop": true}`)
	if taints, _ := m["node-taint"].([]any); !ok || m["bind-address"] != "10.77.0.9" || m["token"] != "agent-tok" || m["agent-token"] != nil ||
		m["server"] != "https://10.77.0.1:6443" || !slices.Contains(taints, any("wecolab.io/idle=true:NoExecute")) {
		t.Errorf("laptop: %v", m)
	}
	if _, ok := config(`{"role": "node", "ip": "10.77.0.9", "box": "home-mac", "k3sToken": "tok\nserver: https://203.0.113.1:6443", "k3sServer": "https://10.77.0.1:6443"}`); ok {
		t.Error("a token carrying YAML is refused")
	}
}

// The hourly sync swaps Nebula's files only for an answer Nebula accepts, and reloads Nebula whatever
// becomes of the SSH keys.
func TestInstallSync(t *testing.T) {
	good := `{"ca": "new ca", "config": "pki: {ca: N/ca.crt}", "cert": "new cert", "stewards": ["10.77.0.1"], "sshKeys": ["ssh-ed25519 K a@b"]}`
	for _, c := range []struct {
		name, answer                 string
		nebulaOK, sshBroken, swapped bool
	}{
		{"good", good, true, false, true},
		{"no config", `{"ca": "new ca", "cert": "new cert", "stewards": ["10.77.0.1"]}`, true, false, false},
		{"refused by nebula", good, false, false, false},
		{"keys not writable", good, true, true, false},
	} {
		dir := scratchBox(t)
		n := dir + "/nebula"
		if err := os.MkdirAll(n+"/config.d", 0o755); err != nil {
			t.Fatal(err)
		}
		for f, s := range map[string]string{"name": "home-box1\n", "stewards": "10.77.0.1\n", "ca.crt": "old ca\n", "host.crt": "old cert\n",
			"config.d/20-fabric.yml": "old config\n", "config.d/10-local.yml": "listen: {}\n"} {
			writeFile(t, n+"/"+f, s, 0o644)
		}
		if c.sshBroken {
			_ = os.RemoveAll(dir + "/ssh")
			writeFile(t, dir+"/ssh", "", 0o600)
		}
		writeFile(t, dir+"/answer.json", c.answer, 0o600)
		writeFile(t, dir+"/bin/curl", "#!/bin/sh\nwhile [ $# -gt 0 ]; do [ \"$1\" = -o ] && out=$2; shift; done\ncp \"$ANSWER\" \"$out\"\n", 0o755)
		writeFile(t, dir+"/bin/nebula", "#!/bin/sh\n[ -n \"$NEBULA_OK\" ]\n", 0o755)
		writeFile(t, dir+"/bin/systemctl", "#!/bin/sh\necho \"systemctl $*\" >> \"$LOG\"\n", 0o755)
		script, ok := bashFns(t, dir, "sync_script", "ssh_keys", "sync_script")
		if !ok {
			t.Fatal(script)
		}
		writeFile(t, dir+"/sync", strings.ReplaceAll(script, "/etc/nebula", n), 0o755)
		nebulaOK := ""
		if c.nebulaOK {
			nebulaOK = "1"
		}
		out, ok := runOn(t, dir, []string{"ANSWER=" + dir + "/answer.json", "NEBULA_OK=" + nebulaOK, "LOG=" + dir + "/log"}, "bash", dir+"/sync")
		expectApplied := c.name == "good" || c.name == "keys not writable"
		if ok != c.swapped {
			t.Errorf("%s: succeeded %v:\n%s", c.name, ok, out)
		}
		log, _ := os.ReadFile(dir + "/log")
		if got := readFile(t, n+"/host.crt"); expectApplied != (got == "new cert\n") || expectApplied != strings.Contains(string(log), "reload nebula") {
			t.Errorf("%s: host.crt %q, systemctl %q", c.name, got, log)
		}
		if expectApplied && (readFile(t, n+"/host.crt.bak") != "old cert\n" || readFile(t, n+"/config.d/20-fabric.yml") != "pki: {ca: N/ca.crt}\n") {
			t.Errorf("%s: the files it replaced are kept as .bak", c.name)
		}
		if c.name == "good" && !strings.Contains(readFile(t, dir+"/ssh/authorized_keys"), "\nssh-ed25519 K a@b\n") {
			t.Errorf("%s: the answer's SSH keys", c.name)
		}
	}
}
func TestInstallSyncRecovery(t *testing.T) {
	for _, tc := range []struct {
		name, failure string
		failRollback  bool
	}{
		{"reload", "reload", false}, {"rollback-reload", "reload", true}, {"partial-install", "install", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := scratchBox(t)
			n := dir + "/nebula"
			if err := os.MkdirAll(n+"/config.d", 0o755); err != nil {
				t.Fatal(err)
			}
			for f, s := range map[string]string{"name": "box\n", "stewards": "10.77.0.1\n", "ca.crt": "old ca\n", "host.crt": "old cert\n", "config.d/20-fabric.yml": "old config\n", "config.d/10-local.yml": "local\n"} {
				writeFile(t, n+"/"+f, s, 0o600)
			}
			writeFile(t, dir+"/answer", `{"ca":"new ca","cert":"new cert","config":"new config","stewards":["10.77.0.1"],"sshKeys":[]}`, 0o600)
			writeFile(t, dir+"/bin/curl", "#!/bin/sh\nwhile [ $# -gt 0 ]; do [ \"$1\" = -o ] && out=$2; shift; done\ncp \"$ANSWER\" \"$out\"\n", 0o755)
			writeFile(t, dir+"/bin/nebula", "#!/bin/sh\nexit 0\n", 0o755)
			writeFile(t, dir+"/bin/systemctl", "#!/bin/sh\necho \"$*\" >> \"$LOG\"\nif [ -f \"$FAIL\" ]; then n=$(cat \"$FAIL\"); if [ \"$n\" -gt 0 ]; then echo $((n-1)) > \"$FAIL\"; exit 1; fi; fi\n", 0o755)
			writeFile(t, dir+"/bin/install", "#!/bin/sh\ncase \"$*\" in *host.crt*) if [ -f \"$FAIL_INSTALL\" ]; then rm \"$FAIL_INSTALL\"; exit 1; fi;; esac\nexec /usr/bin/install \"$@\"\n", 0o755)
			script, ok := bashFns(t, dir, "sync_script", "ssh_keys", "sync_script")
			if !ok {
				t.Fatal(script)
			}
			writeFile(t, dir+"/sync", strings.ReplaceAll(script, "/etc/nebula", n), 0o755)
			fail, installFail := dir+"/fail", dir+"/fail-install"
			if tc.failure == "reload" {
				count := "1"
				if tc.failRollback {
					count = "2"
				}
				writeFile(t, fail, count, 0o600)
			} else {
				writeFile(t, installFail, "1", 0o600)
			}
			env := []string{"ANSWER=" + dir + "/answer", "LOG=" + dir + "/log", "FAIL=" + fail, "FAIL_INSTALL=" + installFail}
			if out, ok := runOn(t, dir, env, "bash", dir+"/sync"); ok {
				t.Fatalf("first failed operation unexpectedly succeeded: %s", out)
			}
			if tc.failRollback {
				if _, err := os.Stat(n + "/.wecolab-reload-pending"); err != nil {
					t.Error("uncertain reload lost pending marker")
				}
			} else if readFile(t, n+"/host.crt") != "old cert\n" {
				t.Error("failed transaction did not restore original bytes")
			}
			if mode, err := os.Stat(n + "/host.crt"); err != nil || mode.Mode().Perm() != 0o600 {
				t.Errorf("rollback mode = %v (%v)", mode, err)
			}
			if out, ok := runOn(t, dir, env, "bash", dir+"/sync"); !ok {
				t.Fatalf("retry failed: %s", out)
			}
			if readFile(t, n+"/host.crt") != "new cert\n" {
				t.Error("retry did not apply answer")
			}
			before := readFile(t, dir+"/log")
			if out, ok := runOn(t, dir, env, "bash", dir+"/sync"); !ok {
				t.Fatalf("idempotent sync: %s", out)
			}
			if got := readFile(t, dir+"/log"); got != before {
				t.Errorf("unchanged successful sync reloaded: %s", got)
			}
			if _, err := os.Stat(n + "/.wecolab-reload-pending"); !os.IsNotExist(err) {
				t.Error("successful reload retained pending marker")
			}
			writeFile(t, n+"/.wecolab-reload-pending", "", 0o600)
			if out, ok := runOn(t, dir, env, "bash", dir+"/sync"); !ok {
				t.Fatalf("pending identical retry: %s", out)
			}
			if got := readFile(t, dir+"/log"); strings.Count(got, "reload nebula") != strings.Count(before, "reload nebula")+1 {
				t.Errorf("identical files with pending marker skipped reload: %s", got)
			}
		})
	}
}

// Run the installer against a scratch service directory, including the real role and identity checks.
func TestInstallInterruptedK3s(t *testing.T) {
	for _, role := range []string{"manager", "node"} {
		for _, scenario := range []string{"cached", "owned-service", "foreign-binary", "identity-conflict", "installer-fails"} {
			t.Run(role+"/"+scenario, func(t *testing.T) {
				dir := scratchBox(t)
				for _, p := range []string{"etc/rancher/k3s", "etc/systemd/system", "usr/local/bin", "cache"} {
					if err := os.MkdirAll(filepath.Join(dir, p), 0o755); err != nil {
						t.Fatal(err)
					}
				}
				unit := "k3s-agent.service"
				if role == "manager" {
					unit = "k3s.service"
				}
				writeFile(t, dir+"/bundle.json", fmt.Sprintf(`{"role":%q,"box":"home-box","ip":"10.77.0.2","k3sToken":"token","k3sAgentToken":"agent","k3sServer":"https://10.77.0.1:6443","public":false}`, role), 0o600)
				if scenario != "foreign-binary" {
					writeFile(t, dir+"/state/installed", "k3s\n", 0o600)
				}
				binary := "#!/bin/sh\nprintf 'k3s version v1\\n'\n"
				writeFile(t, dir+"/usr/local/bin/k3s", binary, 0o755)
				if scenario == "owned-service" {
					writeFile(t, dir+"/etc/systemd/system/"+unit, "[Service]\n", 0o644)
				}
				if scenario == "identity-conflict" {
					writeFile(t, dir+"/etc/rancher/k3s/config.yaml", "node-name: another-box\n", 0o600)
				}
				writeFile(t, dir+"/bin/systemctl", `#!/bin/bash
echo "$*" >> "$EVENTS"
case "$1" in cat) [ -f "$UNITS/$2" ];; start) [ -f "$UNITS/$2" ] && [ -f "$FIREWALL_OK" ];; esac
`, 0o755)
				source := installFns(t, "die", "mark", "has", "valid", "bundle", "verify_payload", "cache_verified", "k3s_config", "k3s_dependencies", "k3s_identity", "install_k3s")
				source = strings.ReplaceAll(source, "/etc/rancher/k3s", dir+"/etc/rancher/k3s")
				source = strings.ReplaceAll(source, "/etc/systemd/system", dir+"/etc/systemd/system")
				source = strings.ReplaceAll(source, "/usr/local/bin/k3s", dir+"/usr/local/bin/k3s")
				run := `set -euo pipefail
STATE=` + dir + `/state BUNDLE=` + dir + `/bundle.json NETWORK=10.77.0.0/16 CACHE= ARCH=amd64 K3S_VERSION=v1 DEV=
K3S_SHA256_amd64=` + fmt.Sprintf("%x", sha256.Sum256([]byte(binary))) + `
K3S_INSTALL_COMMIT=test K3S_INSTALL_SHA256=` + fmt.Sprintf("%x", sha256.Sum256([]byte("#!/bin/sh\nexit 0\n"))) + `
` + source + `
firewall() { touch "$FIREWALL_OK"; }
apparmor() { :; }
kvm_label() { :; }
kubectl() { :; }
sleep() { :; }
fetch() { printf '#!/bin/sh\nexit 0\n' > "$2"; }
sh() {
 echo "$INSTALL_K3S_EXEC skip=$INSTALL_K3S_SKIP_DOWNLOAD" >> "$INSTALL_LOG"
 [ ! -f "$INSTALL_FAIL" ] || return 1
 case "$INSTALL_K3S_EXEC" in server) touch "$UNITS/k3s.service";; agent) touch "$UNITS/k3s-agent.service";; esac
}
install_k3s
`
				writeFile(t, dir+"/run", run, 0o755)
				env := []string{"PATH=" + dir + "/bin:" + dir + "/usr/local/bin:" + os.Getenv("PATH"), "EVENTS=" + dir + "/events", "UNITS=" + dir + "/etc/systemd/system", "FIREWALL_OK=" + dir + "/fw-ok", "INSTALL_LOG=" + dir + "/installer", "INSTALL_FAIL=" + dir + "/fail"}
				if scenario == "installer-fails" {
					writeFile(t, dir+"/fail", "1", 0o600)
				}
				out, ok := runOn(t, dir, env, "bash", dir+"/run")
				expected := scenario == "cached" || scenario == "owned-service"
				if ok != expected {
					t.Fatalf("first run success=%v: %s", ok, out)
				}
				log, _ := os.ReadFile(dir + "/installer")
				if scenario == "cached" && !strings.Contains(string(log), "skip=true") {
					t.Errorf("cached binary did not resume pinned installer: %s", log)
				}
				if scenario == "owned-service" && len(log) > 0 {
					t.Errorf("owned unit reinstalled: %s", log)
				}
				if scenario == "foreign-binary" || scenario == "identity-conflict" {
					if len(log) > 0 {
						t.Errorf("unsafe installer invocation: %s", log)
					}
				}
				if scenario == "installer-fails" {
					if err := os.Remove(dir + "/fail"); err != nil {
						t.Fatal(err)
					}
					if out, ok := runOn(t, dir, env, "bash", dir+"/run"); !ok {
						t.Fatalf("retry: %s", out)
					}
				}
				if expected || scenario == "installer-fails" {
					if _, err := os.Stat(dir + "/etc/systemd/system/" + unit); err != nil {
						t.Errorf("missing role unit: %v", err)
					}
					if !strings.Contains(readFile(t, dir+"/events"), "start "+unit) {
						t.Error("role service not started after firewall")
					}
				}
			})
		}
	}
}

func TestInstallFirewallGate(t *testing.T) {
	dir := scratchBox(t)
	for _, p := range []string{"etc/systemd/system", "usr/local/sbin"} {
		if err := os.MkdirAll(dir+"/"+p, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	writeFile(t, dir+"/bin/systemctl", `#!/bin/sh
echo "$*" >> "$EVENTS"
[ "$1" != restart ] || [ ! -f "$FAIL_RESTART" ]
`, 0o755)
	src := installFns(t, "firewall_script", "firewall", "k3s_dependencies")
	src = strings.ReplaceAll(src, "/etc/systemd/system", dir+"/etc/systemd/system")
	src = strings.ReplaceAll(src, "/usr/local/sbin", dir+"/usr/local/sbin")
	writeFile(t, dir+"/run", "set -euo pipefail\nNETWORK=10.77.0.0/16\n"+src+"\nk3s_dependencies\nfirewall false\n", 0o755)
	env := []string{"EVENTS=" + dir + "/events", "FAIL_RESTART=" + dir + "/fail"}
	writeFile(t, dir+"/fail", "1", 0o600)
	if _, ok := runOn(t, dir, env, "bash", dir+"/run"); ok {
		t.Error("firewall restart failure did not propagate")
	}
	if err := os.Remove(dir + "/fail"); err != nil {
		t.Fatal(err)
	}
	if out, ok := runOn(t, dir, env, "bash", dir+"/run"); !ok {
		t.Fatal(out)
	}
	for _, role := range []string{"k3s", "k3s-agent"} {
		drop := readFile(t, dir+"/etc/systemd/system/"+role+".service.d/10-nebula.conf")
		if !strings.Contains(drop, "Wants=nebula.service\n") || !strings.Contains(drop, "Requires=wecolab-pod-isolation.service\n") ||
			!strings.Contains(drop, "After=nebula.service wecolab-pod-isolation.service\n") {
			t.Errorf("%s dependency omitted: %s", role, drop)
		}
	}
	if got := readFile(t, dir+"/events"); !strings.Contains(got, "daemon-reload\n") || !strings.Contains(got, "restart wecolab-pod-isolation\n") {
		t.Errorf("firewall unit was not enabled and applied: %s", got)
	}
}

func TestInstallAppArmorOwnership(t *testing.T) {
	for _, tc := range []struct {
		name                               string
		original, loaded, external, reject bool
	}{
		{"absent", false, false, false, false}, {"existing-loaded", true, true, false, false},
		{"existing-unloaded", true, false, false, false}, {"parser-rejects", true, true, false, true},
		{"external-conflict", true, true, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := scratchBox(t)
			profiles := dir + "/profiles"
			if err := os.MkdirAll(profiles, 0o755); err != nil {
				t.Fatal(err)
			}
			p := profiles + "/cri-containerd.apparmor.d"
			if tc.original {
				writeFile(t, p, "original policy\n", 0o640)
			}
			if tc.loaded {
				writeFile(t, dir+"/loaded", "cri-containerd.apparmor.d (enforce)\n", 0o600)
			} else {
				writeFile(t, dir+"/loaded", "", 0o600)
			}
			writeFile(t, dir+"/bin/apparmor_parser", `#!/bin/sh
echo "$*" >> "$PARSER_LOG"
[ ! -f "$REJECT" ] || [ "$1" != -r ] || exit 1
case "$1" in -r) echo 'cri-containerd.apparmor.d (enforce)' > "$LOADED";; -R) : > "$LOADED";; esac
`, 0o755)
			src := installFns(t, "mark", "has", "die", "apparmor", "apparmor_restore")
			src = strings.ReplaceAll(src, "/sys/kernel/security/apparmor/profiles", dir+"/loaded")
			src = strings.ReplaceAll(src, "/etc/apparmor.d", profiles)
			writeFile(t, dir+"/run", "set -euo pipefail\nSTATE="+dir+"/state\n"+src+"\n"+`case "$1" in install) apparmor ;; restore) apparmor_restore ;; esac`+"\n", 0o755)
			env := []string{"PARSER_LOG=" + dir + "/parser-log", "LOADED=" + dir + "/loaded", "REJECT=" + dir + "/reject"}
			if tc.reject {
				writeFile(t, dir+"/reject", "1", 0o600)
			}
			if out, ok := runOn(t, dir, env, "bash", dir+"/run", "install"); ok == tc.reject {
				t.Fatalf("install status: %s", out)
			}
			if tc.reject {
				if got := readFile(t, p); got != "original policy\n" {
					t.Errorf("parser rejection changed profile: %q", got)
				}
				if _, err := os.Stat(dir + "/state/installed"); !os.IsNotExist(err) {
					t.Error("parser rejection marked ownership")
				}
				return
			}
			if out, ok := runOn(t, dir, env, "bash", dir+"/run", "install"); !ok {
				t.Fatalf("reinstall: %s", out)
			}
			if tc.external {
				writeFile(t, p, "third-party edit\n", 0o644)
				if out, ok := runOn(t, dir, env, "bash", dir+"/run", "install"); ok {
					t.Errorf("external edits were overwritten on reinstall: %s", out)
				}
			}
			out, ok := runOn(t, dir, env, "bash", dir+"/run", "restore")
			if ok == tc.external {
				t.Errorf("restore success=%v: %s", ok, out)
			}
			if tc.external {
				if readFile(t, p) != "third-party edit\n" {
					t.Error("external policy overwritten")
				}
				if readFile(t, dir+"/state/apparmor/original") != "original policy\n" {
					t.Error("backup lost on conflict")
				}
			} else if tc.original {
				if readFile(t, p) != "original policy\n" {
					t.Error("original bytes not restored")
				}
				if st, _ := os.Stat(p); st.Mode().Perm() != 0o640 {
					t.Errorf("mode %v", st.Mode())
				}
				wantLoaded := tc.loaded
				if loaded := strings.Contains(readFile(t, dir+"/loaded"), "cri-containerd.apparmor.d"); loaded != wantLoaded {
					t.Errorf("loaded=%v want %v", loaded, wantLoaded)
				}
			} else if _, err := os.Stat(p); !os.IsNotExist(err) {
				t.Error("initially absent profile not removed")
			}
		})
	}
	dir := scratchBox(t)
	writeFile(t, dir+"/state/installed", "apparmor\n", 0o600)
	src := installFns(t, "apparmor_restore")
	src = strings.ReplaceAll(src, "/etc/apparmor.d", dir+"/profiles")
	if out, ok := runOn(t, dir, nil, "bash", "-c", "set -euo pipefail\nSTATE="+dir+"/state\n"+src+"\napparmor_restore"); ok || !strings.Contains(out, "missing AppArmor recovery state") {
		t.Errorf("legacy ownership without backup was not refused: %s", out)
	}
}

// The Console serves install.sh as join.sh from an embedded copy, which make refreshes: a build that
// skips make would serve a stale script to every box that joins.
func TestJoinIsInstall(t *testing.T) {
	a, errA := os.ReadFile("join.sh")
	b, errB := os.ReadFile("../../install.sh")
	if errA != nil || errB != nil || !bytes.Equal(a, b) {
		t.Fatalf("cmd/console/join.sh is not install.sh (run make, which copies it): %v %v", errA, errB)
	}
}
