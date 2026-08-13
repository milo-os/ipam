package fixtures

import (
	"bufio"
	"bytes"
	"io"
	"os"
	"path/filepath"
	"testing"

	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
	"sigs.k8s.io/yaml"
)

// TestChainsawSuitesParse checks every chainsaw suite is valid YAML.
//
// A suite that does not parse is skipped by chainsaw with a message that reads
// like a configuration problem, so a broken suite looks like a suite that was
// never meant to run. Parsing here fails the build instead.
func TestChainsawSuitesParse(t *testing.T) {
	suites, err := filepath.Glob(filepath.Join(repoRoot, "test", "e2e", "*", "chainsaw-test.yaml"))
	if err != nil {
		t.Fatalf("glob chainsaw suites: %v", err)
	}
	if len(suites) == 0 {
		t.Fatal("no chainsaw suites found: the glob is looking in the wrong place")
	}

	for _, suite := range suites {
		t.Run(filepath.Base(filepath.Dir(suite)), func(t *testing.T) {
			data, err := os.ReadFile(suite)
			if err != nil {
				t.Fatalf("read: %v", err)
			}
			reader := utilyaml.NewYAMLReader(bufio.NewReader(bytes.NewReader(data)))
			for {
				doc, err := reader.Read()
				if err == io.EOF {
					return
				}
				if err != nil {
					t.Fatalf("split documents: %v", err)
				}
				var parsed map[string]any
				if err := yaml.Unmarshal(doc, &parsed); err != nil {
					t.Fatalf("parse: %v", err)
				}
			}
		})
	}
}
