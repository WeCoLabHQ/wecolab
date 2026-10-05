package main

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
)

// The catalog is the HomelabOS service catalog, imported by hack/catalog-import.py
// into web/catalog.json: one entry per role, translated mechanically into a workload,
// an optional CloudNativePG database, sidecars in the same pod, and a volume per data
// path. Entries that need the host are listed with the reason and refused.

type catalogEnv struct {
	Name  string `json:"name"`
	Value string `json:"value,omitempty"`
	DB    string `json:"db,omitempty"` // uri | host | port | user | password | dbname: from the database's own secret
}

type catalogVolume struct {
	Name      string `json:"name"`
	MountPath string `json:"mountPath"`
	ReadOnly  bool   `json:"readOnly"`
}

type catalogSidecar struct {
	Name    string          `json:"name"`
	Image   string          `json:"image"`
	Env     []catalogEnv    `json:"env"`
	Volumes []catalogVolume `json:"volumes"`
	Command any             `json:"command,omitempty"`
}

type catalogEntry struct {
	Slug, Title, Description, Category, URL, Version, Image string
	Port                                                    int
	Env                                                     []catalogEnv
	Volumes                                                 []catalogVolume
	Sidecars                                                []catalogSidecar
	Database                                                *struct{ User, Name, Password string }
	Unsupported, Attention, Secrets, Limitations            []string
	State, Validated                                        string
	Command, Entrypoint                                     any
	User                                                    string
	// WeCoLab's own entries (hack/catalog-own.json) may also say:
	Runtime   string         // "kata": the pod runs in its own VM (decision 26)
	MeshOnly  bool           // reached only from the people mesh: no public hostname
	Shm       string         // a memory-backed /dev/shm of this size
	Resources map[string]any // the main container's, instead of the defaults
}

// kataRuntime is Kata's RuntimeClass, as system/wecolab/kata.yaml names it and the userns policy admits it.
const kataRuntime = "kata-clh"

func loadCatalog() map[string]catalogEntry {
	b, err := web.ReadFile("web/catalog.json")
	if err != nil {
		return nil
	}
	var doc struct{ Entries []catalogEntry }
	if err := json.Unmarshal(b, &doc); err != nil {
		return nil
	}
	out := map[string]catalogEntry{}
	for _, e := range doc.Entries {
		out[e.Slug] = e
	}
	return out
}

var placeholder = regexp.MustCompile(`\$\{([a-zA-Z0-9_.:]+)\}`)

func randSecret() string {
	b := make([]byte, 24)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

// fillEnv turns catalog env items into container env: database fields come from the
// database's own secret, generated secrets from the app's secret (whole values and
// values that embed one), and the rest are literals with the Console's placeholders filled.
// secrets is filled with what the app's Secret must hold.
func fillEnv(items []catalogEnv, app, db string, vars map[string]string, generated map[string]string, secrets map[string]string) []any {
	out := []any{}
	for _, it := range items {
		if it.DB != "" && db != "" {
			key := it.DB
			if key == "user" {
				key = "username"
			}
			out = append(out, map[string]any{"name": it.Name, "valueFrom": map[string]any{"secretKeyRef": map[string]any{"name": db + "-app", "key": key, "optional": true}}})
			continue
		}
		v := placeholder.ReplaceAllStringFunc(it.Value, func(m string) string {
			k := placeholder.FindStringSubmatch(m)[1]
			if s, ok := strings.CutPrefix(k, "secret:"); ok {
				if generated[s] == "" {
					generated[s] = randSecret()
				}
				return generated[s]
			}
			if val, ok := vars[k]; ok {
				return val
			}
			return ""
		})
		if strings.Contains(it.Value, "${secret:") {
			secrets["env-"+it.Name] = v
			out = append(out, map[string]any{"name": it.Name, "valueFrom": map[string]any{"secretKeyRef": map[string]any{"name": app, "key": "env-" + it.Name}}})
			continue
		}
		out = append(out, map[string]any{"name": it.Name, "value": v})
	}
	return out
}

// catalogPod renders the containers, volumes and claims for an entry.
func catalogPod(e catalogEntry, app, db string, vars map[string]string, generated, secrets map[string]string) (containers, volumes []any, claims []map[string]any, err error) {
	if len(e.Unsupported) > 0 {
		return nil, nil, nil, fmt.Errorf("%s is host incompatible, so the fabric refuses it: %s", e.Title, strings.Join(e.Unsupported, "; "))
	}
	mount := func(prefix string, vols []catalogVolume) []any {
		mounts := []any{}
		for _, v := range vols {
			name := prefix + v.Name
			volumes = append(volumes, map[string]any{"name": name, "persistentVolumeClaim": map[string]any{"claimName": app + "-" + name}})
			claims = append(claims, map[string]any{"name": app + "-" + name})
			mounts = append(mounts, map[string]any{"name": name, "mountPath": v.MountPath, "readOnly": v.ReadOnly})
		}
		return mounts
	}
	sc := map[string]any{"allowPrivilegeEscalation": false}
	main := map[string]any{"name": app, "image": e.Image, "ports": []any{map[string]any{"containerPort": int64(e.Port)}},
		"env": fillEnv(e.Env, app, db, vars, generated, secrets), "volumeMounts": mount("", e.Volumes), "securityContext": sc,
		"resources": map[string]any{"requests": map[string]any{"cpu": "100m", "memory": "128Mi"}, "limits": map[string]any{"memory": "1Gi"}}}
	if e.Command != nil {
		main["args"] = toList(e.Command)
	}
	if e.Entrypoint != nil {
		main["command"] = toList(e.Entrypoint)
	}
	if e.Resources != nil {
		main["resources"] = e.Resources
	}
	if e.Shm != "" {
		volumes = append(volumes, map[string]any{"name": "shm", "emptyDir": map[string]any{"medium": "Memory", "sizeLimit": e.Shm}})
		main["volumeMounts"] = append(main["volumeMounts"].([]any), map[string]any{"name": "shm", "mountPath": "/dev/shm"})
	}
	containers = append(containers, main)
	for _, s := range e.Sidecars {
		if strings.Contains(s.Image, "${") {
			return nil, nil, nil, fmt.Errorf("sidecar %s has an unresolved image %s", s.Name, s.Image)
		}
		c := map[string]any{"name": s.Name, "image": s.Image, "env": fillEnv(s.Env, app, "", vars, generated, secrets), "volumeMounts": mount(s.Name+"-", s.Volumes), "securityContext": sc,
			"resources": map[string]any{"requests": map[string]any{"cpu": "50m", "memory": "64Mi"}, "limits": map[string]any{"memory": "1Gi"}}}
		if s.Command != nil {
			c["args"] = toList(s.Command)
		}
		containers = append(containers, c)
	}
	return containers, volumes, claims, nil
}

func toList(v any) []any {
	switch x := v.(type) {
	case []any:
		return x
	case string:
		out := []any{}
		for _, f := range strings.Fields(x) {
			out = append(out, f)
		}
		return out
	}
	return nil
}
