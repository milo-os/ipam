package main

import (
	"encoding/json"
	"io"
)

// Plugin contract constants. datumctl's plugin SDK is internal to the datumctl
// repository and is not importable, so the thin manifest/env contract is
// reimplemented here. datumctl discovers a plugin by invoking it with
// --plugin-manifest and expects a single JSON document on stdout and exit 0.

const (
	pluginName        = "ipam"
	pluginDescription = "Manage IP address space (pools and prefixes) across the platform"
	// pluginAPIVersion is the version of the datumctl <-> plugin contract this
	// binary speaks, not the IPAM API version.
	pluginAPIVersion = 1
	// minDatumctlVersion is the lowest datumctl that knows how to dispatch to
	// this plugin.
	minDatumctlVersion = "0.5.0"
	// minAPIVersion is the IPAM apiserver API group/version this plugin targets.
	minAPIVersion = "ipam.miloapis.com/v1alpha1"
)

// pluginVersion is the plugin's release version. It defaults to "0.0.0" to mark
// an unreleased local build and is overridden at release time by goreleaser via
// -ldflags "-X main.pluginVersion=<version>" (the git tag without its leading
// "v"), so a published binary reports the version it was released under. It is
// a var (not a const) precisely so the linker can set it.
var pluginVersion = "0.0.0"

// pluginManifest is the document emitted in response to --plugin-manifest.
type pluginManifest struct {
	Name               string `json:"name"`
	Version            string `json:"version"`
	Description        string `json:"description"`
	APIVersion         int    `json:"api_version"`
	MinDatumctlVersion string `json:"min_datumctl_version"`
	MinAPIVersion      string `json:"min_api_version"`
}

func defaultManifest() pluginManifest {
	return pluginManifest{
		Name:               pluginName,
		Version:            pluginVersion,
		Description:        pluginDescription,
		APIVersion:         pluginAPIVersion,
		MinDatumctlVersion: minDatumctlVersion,
		MinAPIVersion:      minAPIVersion,
	}
}

// writeManifest renders the manifest as indented JSON. Returned separately from
// printing so it can be unit tested.
func writeManifest(w io.Writer, m pluginManifest) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(m)
}
