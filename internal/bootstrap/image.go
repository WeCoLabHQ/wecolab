package bootstrap

import (
	"archive/tar"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"time"
)

// File is one file of an image: where it comes from on this machine and where it goes.
type File struct{ From, To string }

// Image writes a docker-archive tarball (what `k3s ctr images import` takes) of one layer holding the
// files, with the first as its entrypoint, running as uid 65532. No Docker needed: WeCoLab's images
// are static binaries (decision 13).
func Image(ref, arch string, files []File, out string) error {
	var layer bytes.Buffer
	tw := tar.NewWriter(&layer)
	now := time.Unix(0, 0)
	dirs := map[string]bool{}
	mkdir := func(d string, mode int64) error {
		if d == "" || dirs[d] {
			return nil
		}
		dirs[d] = true
		return tw.WriteHeader(&tar.Header{Typeflag: tar.TypeDir, Name: d + "/", Mode: mode, ModTime: now})
	}
	if err := mkdir("tmp", 0o1777); err != nil {
		return err
	}
	for _, f := range files {
		b, err := os.ReadFile(f.From)
		if err != nil {
			return err
		}
		to := trim(f.To)
		for i := range to {
			if to[i] == '/' {
				if err := mkdir(to[:i], 0o755); err != nil {
					return err
				}
			}
		}
		if err := tw.WriteHeader(&tar.Header{Typeflag: tar.TypeReg, Name: to, Mode: 0o755, Size: int64(len(b)), ModTime: now}); err != nil {
			return err
		}
		if _, err := tw.Write(b); err != nil {
			return err
		}
	}
	if err := tw.Close(); err != nil {
		return err
	}
	diff := sha256.Sum256(layer.Bytes())
	cfg, _ := json.Marshal(map[string]any{
		"architecture": arch, "os": "linux", "created": now.UTC().Format(time.RFC3339),
		"config": map[string]any{"Entrypoint": []string{"/" + trim(files[0].To)}, "User": "65532:65532",
			"Env": []string{"PATH=/usr/local/bin:/usr/bin:/bin"}},
		"rootfs": map[string]any{"type": "layers", "diff_ids": []string{"sha256:" + hex.EncodeToString(diff[:])}},
	})
	cfgSum := sha256.Sum256(cfg)
	cfgName := hex.EncodeToString(cfgSum[:]) + ".json"
	layerName := hex.EncodeToString(diff[:]) + "/layer.tar"
	manifest, _ := json.Marshal([]map[string]any{{"Config": cfgName, "RepoTags": []string{ref}, "Layers": []string{layerName}}})

	f, err := os.Create(out)
	if err != nil {
		return err
	}
	defer f.Close()
	ot := tar.NewWriter(f)
	for _, e := range []struct {
		name string
		b    []byte
	}{{cfgName, cfg}, {layerName, layer.Bytes()}, {"manifest.json", manifest}} {
		if err := ot.WriteHeader(&tar.Header{Typeflag: tar.TypeReg, Name: e.name, Mode: 0o644, Size: int64(len(e.b)), ModTime: now}); err != nil {
			return err
		}
		if _, err := ot.Write(e.b); err != nil {
			return err
		}
	}
	if err := ot.Close(); err != nil {
		return fmt.Errorf("image %s: %w", ref, err)
	}
	return nil
}

func trim(p string) string {
	for len(p) > 0 && p[0] == '/' {
		p = p[1:]
	}
	return p
}
