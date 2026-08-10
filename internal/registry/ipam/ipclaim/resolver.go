package ipclaim

import (
	"context"
	"fmt"

	"go.miloapis.com/ipam/internal/allocator"
	"go.miloapis.com/ipam/pkg/apis/ipam"
	"go.miloapis.com/ipam/pkg/apis/ipam/v1alpha1"
)

// Resolver is the half of the Create path that reads and provisions.
//
// It is an interface for one reason: the transaction structure behind it is the
// hardest thing in the service to get right — a read-only class lookup, then a
// sequence of independently-committed pool provisions — and a test that could
// only exercise it against a live database would exercise it rarely. Behind the
// interface, the request path above can be tested for what it does with each
// answer, and the transaction structure itself is tested where it belongs,
// against a real Postgres.
//
// It is deliberately not a "storage" abstraction. It has exactly the three
// questions the Create path asks, and no general read/write surface for
// anything else to grow into.
type Resolver interface {
	// ResolveClass returns the class a claim allocates under: the one it names,
	// or the default for the family it asked for.
	ResolveClass(ctx context.Context, className string, family v1alpha1.IPFamily) (*v1alpha1.IPClass, error)

	// ResolvePool returns the pool the claim draws from, provisioning any
	// missing level of the class's chain along the way. Pools it creates are
	// committed and durable before it returns, including when the caller's
	// allocation subsequently fails.
	ResolvePool(ctx context.Context, class *v1alpha1.IPClass, claimScope map[string]ipam.ScopeRef, project string) (string, error)

	// ResolveExistingPool answers the same question without provisioning,
	// reporting the levels that would have to be built. It backs server-side
	// dry-run, which must leave nothing behind.
	ResolveExistingPool(ctx context.Context, class *v1alpha1.IPClass, claimScope map[string]ipam.ScopeRef, project string) (string, []allocator.CascadeLevel, error)
}

// postgresResolver is the production Resolver: the allocator's cascade, with a
// connection pool to run it on.
type postgresResolver struct {
	db txBeginner
}

// NewPostgresResolver returns the Resolver the apiserver wires in.
func NewPostgresResolver(db txBeginner) Resolver { return &postgresResolver{db: db} }

// ResolveClass reads the class in a read-only transaction of its own.
//
// Separate from the allocation transaction rather than its first statement,
// because everything it can reject — an unknown class, no default for the
// family — should be rejected before the cascade starts building pools.
func (r *postgresResolver) ResolveClass(ctx context.Context, className string, family v1alpha1.IPFamily) (*v1alpha1.IPClass, error) {
	tx, err := r.db.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("begin class resolution transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	return allocator.ResolveClass(ctx, tx, className, family)
}

func (r *postgresResolver) ResolvePool(ctx context.Context, class *v1alpha1.IPClass, claimScope map[string]ipam.ScopeRef, project string) (string, error) {
	return allocator.ResolvePool(ctx, r.db, class, claimScope, project)
}

func (r *postgresResolver) ResolveExistingPool(ctx context.Context, class *v1alpha1.IPClass, claimScope map[string]ipam.ScopeRef, project string) (string, []allocator.CascadeLevel, error) {
	return allocator.ResolveExistingPool(ctx, r.db, class, claimScope, project)
}

// Compile-time assertion that the production implementation stays in step with
// the interface.
var _ Resolver = (*postgresResolver)(nil)
