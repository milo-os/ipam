package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	ipamv1alpha1 "go.miloapis.com/ipam/pkg/apis/ipam/v1alpha1"
)

func TestResolveColorPrecedence(t *testing.T) {
	var buf bytes.Buffer // not a *os.File, so "auto" resolves to no color
	cases := []struct {
		name    string
		mode    string
		output  string
		want    bool
		noColor bool
	}{
		{"always table", "always", outputTable, true, false},
		{"never table", "never", outputTable, false, false},
		{"auto pipe", "auto", outputTable, false, false},
		{"json never colored", "always", outputJSON, false, false},
		{"yaml never colored", "always", outputYAML, false, false},
		{"name never colored", "always", outputName, false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.noColor {
				t.Setenv("NO_COLOR", "1")
			}
			got := resolveColor(tc.mode, &buf, tc.output)
			if got.enabled != tc.want {
				t.Fatalf("resolveColor(%s,%s).enabled = %v, want %v", tc.mode, tc.output, got.enabled, tc.want)
			}
		})
	}
}

func TestNoColorEnvDisablesAuto(t *testing.T) {
	t.Setenv("NO_COLOR", "1")
	var buf bytes.Buffer
	if resolveColor("auto", &buf, outputTable).enabled {
		t.Fatal("NO_COLOR set but color enabled")
	}
}

func TestUtilizationCellHasTextSignal(t *testing.T) {
	// Even without color, the cell must carry the numeric percentage and, when
	// high, a textual severity label.
	cell := utilizationCell(98, 10, false)
	if !strings.Contains(cell, "98%") {
		t.Errorf("cell missing percentage: %q", cell)
	}
	if !strings.Contains(cell, "(HIGH)") {
		t.Errorf("cell missing severity label: %q", cell)
	}
	if strings.Contains(cell, "\x1b[") {
		t.Errorf("cell unexpectedly colored: %q", cell)
	}
}

func TestUtilizationBarFilled(t *testing.T) {
	bar := utilizationBar(50, 10, false)
	full := strings.Count(bar, "█")
	empty := strings.Count(bar, "░")
	if full != 5 || empty != 5 {
		t.Fatalf("bar = %q, want 5 full / 5 empty", bar)
	}
}

func TestEncodeJSONIsValidAndClean(t *testing.T) {
	pool := newPool("p", "10.0.0.0/8", ipamv1alpha1.IPv4, 100, 50)
	setPoolGVK(pool) // production sets GVK before encoding; the printer requires it
	var buf bytes.Buffer
	if err := encodeJSON(&buf, pool); err != nil {
		t.Fatal(err)
	}
	var back map[string]any
	if err := json.Unmarshal(buf.Bytes(), &back); err != nil {
		t.Fatalf("output is not valid JSON: %v\n%s", err, buf.String())
	}
	if back["apiVersion"] != apiVersion || back["kind"] != "IPPool" {
		t.Errorf("JSON missing GVK: apiVersion=%v kind=%v", back["apiVersion"], back["kind"])
	}
}

func TestManifestWriter(t *testing.T) {
	var buf bytes.Buffer
	if err := writeManifest(&buf, defaultManifest()); err != nil {
		t.Fatal(err)
	}
	var m pluginManifest
	if err := json.Unmarshal(buf.Bytes(), &m); err != nil {
		t.Fatalf("manifest is not valid JSON: %v", err)
	}
	if m.Name != "ipam" || m.APIVersion != 1 || m.MinAPIVersion == "" {
		t.Fatalf("unexpected manifest: %+v", m)
	}
}
