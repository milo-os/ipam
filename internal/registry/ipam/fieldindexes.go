package ipamregistry

import (
	"go.miloapis.com/ipam/internal/fieldindex"
	"go.miloapis.com/ipam/internal/registry/ipam/ipallocation"
	"go.miloapis.com/ipam/internal/registry/ipam/ipclaim"
	"go.miloapis.com/ipam/internal/registry/ipam/ipclass"
	"go.miloapis.com/ipam/internal/registry/ipam/ippool"
)

// AllFieldIndexes returns the combined set of SQL expression indexes for every
// IPAM resource. Pass the result to fieldindex.SyncIndexes at startup.
func AllFieldIndexes() []fieldindex.FieldIndex {
	var all []fieldindex.FieldIndex
	all = append(all, ipclaim.FieldIndexes...)
	all = append(all, ipallocation.FieldIndexes...)
	all = append(all, ippool.FieldIndexes...)
	all = append(all, ipclass.FieldIndexes...)
	return all
}
