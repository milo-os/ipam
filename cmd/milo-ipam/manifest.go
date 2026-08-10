package main

import (
	"go.datum.net/datumctl/plugin"
)

// Plugin contract constants. The manifest document itself is defined by
// datumctl's plugin SDK (plugin.Manifest); this binary builds one and lets
// plugin.ServeManifest handle the --plugin-manifest protocol. Using the SDK
// type means the datumctl <-> plugin contract is enforced by the compiler
// rather than duplicated by hand.

const (
	pluginName        = "ipam"
	pluginDescription = "Manage IP address space (classes, claims, allocations, pools) across the platform"
	// pluginAPIVersion is the version of the datumctl <-> plugin contract this
	// binary speaks, not the IPAM API version.
	pluginAPIVersion = 1
	// minDatumctlVersion is the lowest datumctl that knows how to dispatch to
	// this plugin.
	minDatumctlVersion = "0.5.0"
	// minAPIVersion is the lowest datumctl <-> plugin contract version this
	// binary can run against. datumctl hard-blocks a plugin whose min_api_version
	// exceeds the host's contract version. This is an integer contract version,
	// NOT a Kubernetes API group/version.
	minAPIVersion = pluginAPIVersion
	// ipamAPIGroupVersion is the IPAM apiserver group/version this plugin talks
	// to. It is human-facing (shown by `version`) and is deliberately kept out of
	// the datumctl plugin manifest, whose api_version fields are integer contract
	// versions.
	ipamAPIGroupVersion = "ipam.miloapis.com/v1alpha1"
)

// pluginVersion is the plugin's release version. It defaults to "0.0.0" to mark
// an unreleased local build and is overridden at release time by goreleaser via
// -ldflags "-X main.pluginVersion=<version>" (the git tag without its leading
// "v"), so a published binary reports the version it was released under. It is
// a var (not a const) precisely so the linker can set it.
var pluginVersion = "0.0.0"

// pluginManifest builds the manifest datumctl reads via --plugin-manifest. The
// return type is the SDK's plugin.Manifest, so field names and types stay in
// lockstep with the host contract.
func pluginManifest() plugin.Manifest {
	return plugin.Manifest{
		Name:               pluginName,
		Version:            pluginVersion,
		Description:        pluginDescription,
		APIVersion:         pluginAPIVersion,
		MinDatumctlVersion: minDatumctlVersion,
		MinAPIVersion:      minAPIVersion,
	}
}
