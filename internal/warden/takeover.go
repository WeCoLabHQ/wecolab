package warden

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/yaml"

	"wecolab.io/wecolab/api/v1alpha1"
	"wecolab.io/wecolab/internal/fabric"
)

// ErrWriterAlready and ErrNotSteward are the two ways a takeover is refused before anything is written.
var (
	ErrWriterAlready = errors.New("this site is the writer already")
	ErrNotSteward    = errors.New("only a steward can take over")
)

// TakeOver makes site the writer, from its own copy of the Fabric: it must be a steward there, and the new
// epoch beats every one this copy and the stewards that answer know, so an old writer that comes back
// follows. The copy is opened for the Console's commits first (Forgejo's branch protection) and closed
// again if the commit fails. The Console's Settings page calls it, and so does `warden takeover` on a
// steward's manager when no Console can be reached (docs/operations.md, "The writer").
func TakeOver(ctx context.Context, g *fabric.Git, site string, peers *Peers, who fabric.Author) (int, error) {
	b, ok, err := g.Read(ctx, SettingsPath)
	if err != nil {
		return 0, err
	}
	if !ok {
		return 0, fmt.Errorf("this site's copy has no %s: it holds no Fabric yet", SettingsPath)
	}
	set, err := settingsIn(b)
	if err != nil {
		return 0, err
	}
	if set.Writer == site {
		return 0, ErrWriterAlready
	}
	sites, err := sitesIn(ctx, g)
	if err != nil {
		return 0, err
	}
	epoch, steward := set.Epoch, false
	for _, x := range sites {
		if !x.Spec.Steward {
			continue
		}
		steward = steward || x.Name == site
		if peers == nil {
			continue
		}
		if st, ok := peers.Get(ctx, x.Name); ok {
			epoch = max(epoch, st.Epoch)
		}
	}
	if !steward {
		return 0, ErrNotSteward
	}
	epoch++
	for try := 0; try < 2; try++ {
		if err = g.Protect(ctx, []string{ForgejoMirror, ForgejoOwner}); err != nil {
			return 0, fmt.Errorf("open this copy for commits: %w", err)
		}
		_, err = g.Edit(ctx, who, fmt.Sprintf("the writer is now %s (epoch %d)", site, epoch), []string{SettingsPath}, func(s *fabric.Snapshot) ([]fabric.FileChange, error) {
			b, _ := s.Get(SettingsPath)
			cm := &corev1.ConfigMap{}
			if err := yaml.Unmarshal(b, cm); err != nil {
				return nil, err
			}
			cur, err := ParseSettings(cm.Data)
			if err != nil {
				return nil, err
			}
			if cur.Writer == site || cur.Epoch >= epoch {
				return nil, fabric.ErrConflict // the writer changed meanwhile
			}
			cm.Data["writer"], cm.Data["epoch"] = site, strconv.Itoa(epoch)
			out, err := yaml.Marshal(cm)
			return []fabric.FileChange{{Path: SettingsPath, Content: append([]byte("# The fabric's settings (docs/architecture.md). Changing the writer raises the epoch.\n"), out...)}}, err
		})
		if err == nil || !strings.Contains(err.Error(), " 403 ") { // protection can take a moment to apply
			break
		}
	}
	if err != nil {
		if perr := g.Protect(ctx, []string{ForgejoMirror}); perr != nil {
			return 0, fmt.Errorf("%w (and this copy could not be closed again: %v)", err, perr)
		}
		return 0, err
	}
	return epoch, nil
}

func settingsIn(b []byte) (*Settings, error) {
	cm := &corev1.ConfigMap{}
	if err := yaml.Unmarshal(b, cm); err != nil {
		return nil, err
	}
	return ParseSettings(cm.Data)
}

// sitesIn are the Sites in a copy of the Fabric.
func sitesIn(ctx context.Context, g *fabric.Git) ([]v1alpha1.Site, error) {
	paths, err := g.List(ctx, "fabric/sites")
	if err != nil {
		return nil, err
	}
	out := []v1alpha1.Site{}
	for _, p := range paths {
		if !strings.HasSuffix(p, ".yaml") {
			continue
		}
		b, ok, err := g.Read(ctx, p)
		if err != nil {
			return nil, err
		}
		var s v1alpha1.Site
		if ok && yaml.Unmarshal(b, &s) == nil {
			out = append(out, s)
		}
	}
	return out, nil
}
