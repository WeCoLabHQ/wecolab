package main

import (
	"net/http"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"wecolab.io/wecolab/api/v1alpha1"
	"wecolab.io/wecolab/internal/fabric"
	"wecolab.io/wecolab/internal/warden"
)

type fencingConfirmation struct {
	ArchiveID       string
	PreviousPrimary string
	ExpectedAppSHA  string
	Method          string
	Evidence        string
}

func (s *server) forcePreview(w http.ResponseWriter, r *http.Request) {
	if !s.can(w, r, r.PathValue("ns")) {
		return
	}
	a := &v1alpha1.App{ObjectMeta: metav1.ObjectMeta{Namespace: r.PathValue("ns"), Name: r.PathValue("app")}}
	p, err := s.pathOf(a)
	if err != nil {
		answer(w, err, 400)
		return
	}
	var preview fencingConfirmation
	_, err = s.git.Edit(r.Context(), fabric.Author{}, "read fencing preview", []string{p}, func(snap *fabric.Snapshot) ([]fabric.FileChange, error) {
		b, ok := snap.Get(p)
		if !ok {
			return nil, fail(404, "no such app")
		}
		if _, err := decode(b, true, a); err != nil {
			return nil, err
		}
		if a.Spec.Database == "" || a.Spec.ArchiveID == "" {
			return nil, fail(409, "database identity missing; complete the archive migration first")
		}
		preview = fencingConfirmation{ArchiveID: a.Spec.ArchiveID, PreviousPrimary: warden.Serving(a.Spec), ExpectedAppSHA: snap.SHA(p)}
		return nil, nil
	})
	if err != nil {
		answer(w, err, 502)
		return
	}
	writeJSON(w, preview)
}
