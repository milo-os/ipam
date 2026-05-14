package ipamregistry

import (
	"go.miloapis.com/ipam/internal/fieldindex"
	"go.miloapis.com/ipam/internal/registry/ipam/ipaddress"
	"go.miloapis.com/ipam/internal/registry/ipam/ipaddressclaim"
	"go.miloapis.com/ipam/internal/registry/ipam/ipprefix"
	"go.miloapis.com/ipam/internal/registry/ipam/ipprefixclaim"
)

// AllFieldIndexes returns the combined set of SQL expression indexes for every
// IPAM resource. Pass the result to fieldindex.SyncIndexes at startup.
func AllFieldIndexes() []fieldindex.FieldIndex {
	var all []fieldindex.FieldIndex
	all = append(all, ipprefixclaim.FieldIndexes...)
	all = append(all, ipaddressclaim.FieldIndexes...)
	all = append(all, ipaddress.FieldIndexes...)
	all = append(all, ipprefix.FieldIndexes...)
	return all
}
