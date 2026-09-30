package main

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"

	"wecolab.io/wecolab/internal/warden"
)

// install.sh (served as join.sh) reads the join response only through its bundle helper and ssh_keys:
// every field it names there must be one the Console sends (docs/plans/2026-09-29-hardening.md, R9).
func TestInstallReadsOnlyJoinFields(t *testing.T) {
	b, err := os.ReadFile("join.sh")
	if err != nil {
		t.Fatal(err)
	}
	sent := map[string]bool{}
	rt := reflect.TypeFor[joinResponse]()
	for i := range rt.NumField() {
		name, _, _ := strings.Cut(rt.Field(i).Tag.Get("json"), ",")
		sent[name] = true
	}
	reads := regexp.MustCompile(`(?m)(?:\$\(|^\s*)bundle ([A-Za-z0-9]+)|has\("([A-Za-z0-9]+)"\)`).FindAllStringSubmatch(string(b), -1)
	if len(reads) < 16 {
		t.Fatalf("found %d reads of the join response in join.sh: is its helper still called bundle?", len(reads))
	}
	for _, r := range reads {
		if f := r[1] + r[2]; !sent[f] {
			t.Errorf("join.sh reads %q, which the join response does not have", f)
		}
	}
}

// Warden names the service account on exactly the Kustomizations install.sh makes.
func TestInstallRootsAreWardens(t *testing.T) {
	b, err := os.ReadFile("join.sh")
	if err != nil {
		t.Fatal(err)
	}
	made := []string{}
	for _, m := range regexp.MustCompile(`kind: Kustomization\nmetadata: \{ name: ([a-z-]+), namespace: flux-system \}`).FindAllStringSubmatch(string(b), -1) {
		made = append(made, m[1])
	}
	if !slices.Equal(made, warden.Roots) {
		t.Fatalf("install.sh makes %v, Warden names %v", made, warden.Roots)
	}
}

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
		if !strings.HasSuffix(fn, "}") { // not a one-liner: up to a closing brace that ends a paragraph (a heredoc's does not)
			fn = rest[:funcEnd.FindStringIndex(rest)[0]+2]
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
		{"keys not writable", good, true, true, true},
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
		if ok != c.swapped {
			t.Errorf("%s: succeeded %v:\n%s", c.name, ok, out)
		}
		log, _ := os.ReadFile(dir + "/log")
		if got := readFile(t, n+"/host.crt"); c.swapped != (got == "new cert\n") || c.swapped != strings.Contains(string(log), "reload nebula") {
			t.Errorf("%s: host.crt %q, systemctl %q", c.name, got, log)
		}
		if c.swapped && (readFile(t, n+"/host.crt.bak") != "old cert\n" || readFile(t, n+"/config.d/20-fabric.yml") != "pki: {ca: N/ca.crt}\n") {
			t.Errorf("%s: the files it replaced are kept as .bak", c.name)
		}
		if c.swapped && !c.sshBroken && !strings.Contains(readFile(t, dir+"/ssh/authorized_keys"), "\nssh-ed25519 K a@b\n") {
			t.Errorf("%s: the answer's SSH keys", c.name)
		}
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
