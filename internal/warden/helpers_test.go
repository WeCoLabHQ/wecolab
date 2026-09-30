package warden

import (
	"encoding/json"

	"wecolab.io/wecolab/api/v1alpha1"
)

func sampleApp() *v1alpha1.App {
	a := &v1alpha1.App{}
	a.Name, a.Namespace = "docs", "vince"
	a.Spec = v1alpha1.AppSpec{Sites: []string{"vince", "friend"}, Primary: "vince", Workload: "docs", Database: "docs-db"}
	return a
}

func toJSON(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}
