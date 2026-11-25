package main

import (
	"os"
	"testing"

	"sigs.k8s.io/yaml"
)

// The retraction list parses as the platform installer reads it, and names only charts this
// repository publishes.
func TestTheRetractedVersionsNameThisRepositorysCharts(t *testing.T) {
	raw, err := os.ReadFile("retracted-versions.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Retracted []struct {
			Version  string   `json:"version"`
			Versions []string `json:"versions"`
			Charts   []string `json:"charts"`
			Use      string   `json:"use"`
			Reason   string   `json:"reason"`
		} `json:"retracted"`
	}
	if err := yaml.UnmarshalStrict(raw, &doc); err != nil {
		t.Fatal(err)
	}
	if len(doc.Retracted) == 0 {
		t.Fatal("no entry")
	}
	for i, e := range doc.Retracted {
		if (e.Version == "") == (len(e.Versions) == 0) {
			t.Fatalf("entry %d: exactly one of version and a non-empty versions", i)
		}
		if len(e.Charts) == 0 || e.Use == "" || e.Reason == "" {
			t.Fatalf("entry %d: charts, use and reason are required", i)
		}
		for _, c := range e.Charts {
			if st, err := os.Stat("helm/" + c + "/Chart.yaml"); err != nil || st.IsDir() {
				t.Fatalf("entry %d: %s is not a chart of this repository", i, c)
			}
		}
	}
}
