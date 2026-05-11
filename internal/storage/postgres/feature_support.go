package postgres

import (
	"k8s.io/apiserver/pkg/storage"
	etcdfeature "k8s.io/apiserver/pkg/storage/feature"
)

// FeatureSupportChecker advertises etcd-style storage features as supported
// for the Postgres backend.
//
// k8s.io/apiserver hardcodes its `DefaultFeatureSupportChecker` to be an
// etcd-specific implementation that probes the etcd cluster's progress notify
// support at startup. For non-etcd backends like Postgres, this checker
// always reports `false`, which prevents the cacher from enabling
// ConsistentListFromCache. The result is that every default kubectl read
// (no resource version) bypasses the in-memory cache and hits Postgres.
//
// This wrapper embeds the existing default checker (so we inherit its
// `CheckClient` method, which references an unexported `client` interface
// type and therefore can't be implemented from outside the package) and
// overrides `Supports` to declare support for RequestWatchProgress. The
// Postgres Store provides a working RequestWatchProgress implementation
// (Store.RequestWatchProgress nudges all active watchers to emit a bookmark
// at the current global resource version), so the cacher can use it to
// drive ConsistentListFromCache.
//
// Wire it from cmd/ipam/serve.go (Postgres is the only backend):
//
//	etcdfeature.DefaultFeatureSupportChecker = postgres.NewFeatureSupportChecker()
type FeatureSupportChecker struct {
	// Embed the original interface so we inherit CheckClient.
	etcdfeature.FeatureSupportChecker
}

// NewFeatureSupportChecker returns a checker that reports support for the
// features the Postgres Store implements.
func NewFeatureSupportChecker() *FeatureSupportChecker {
	return &FeatureSupportChecker{
		FeatureSupportChecker: etcdfeature.DefaultFeatureSupportChecker,
	}
}

// Supports overrides the embedded checker. It returns true for features the
// Postgres Store implements; for everything else it falls through to the
// embedded etcd checker (which is harmless because IPAM never initializes
// etcd clients — the etcd RESTOptionsGetter is disabled in serve.go).
func (f *FeatureSupportChecker) Supports(feature storage.Feature) bool {
	switch feature {
	case storage.RequestWatchProgress:
		return true
	}
	return f.FeatureSupportChecker.Supports(feature)
}
