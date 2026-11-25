package allocator

import (
	"context"
	"fmt"
	"net"
	"time"

	"github.com/jackc/pgx/v5"

	"go.miloapis.com/ipam/internal/metrics"
	"go.miloapis.com/ipam/pkg/apis/ipam"
)

// ChainLookup is the read of a claim's class chain: the ancestry, the pools
// offering its root, and the pool each level already has. A create makes it
// in two pipelined round trips after admission, instead of one round trip per
// read:
//
//  1. BeginChainLookup sends the check for an allocation retained under the
//     claim's identity together with the first read the chain needs: the
//     leaf's parent class, or for a leaf with no parent the pools offering it.
//  2. The ancestry is walked on from that parent, one dependent read per
//     level above it. Then one batch reads the pools offering the root and
//     every level's identity row in one statement.
//
// Nothing here takes a lock. Each statement sees committed data as of itself,
// as it would inside a short READ COMMITTED transaction. A level read as
// missing decides nothing: provisioning it claims the identity row first, and
// that arbitrates a race. A level read as present cannot change while it
// exists.
//
// Errors keep the order they had when every read was its own round trip. The
// retained-allocation check is reported by BeginChainLookup and acted on
// first; every other read's error is held and returned by Resolve or Pending,
// in the order those reads used to run: ancestry and plan, then discovery,
// then the identity rows.
type ChainLookup struct {
	leaf  *ResolvedClass
	scope map[string]ipam.ScopeRef

	// start is when resolution began: the first ancestry read.
	start time.Time

	// What the first round trip read for the chain. parent is set for a leaf
	// with a parent, offers for one without.
	parent *loadedClass
	offers *offerRead

	read bool
	err  error // ancestry or plan

	levels []CascadeLevel

	// identities holds each level's pool key, "" for a level with none yet;
	// identityErr is the error reading them returned.
	identities  []string
	identityErr error
}

// offerRead is the result of reading the pools offering a class.
type offerRead struct {
	pools []offeringPool
	err   error
}

func newChainLookup(leaf *ResolvedClass, claimScope map[string]ipam.ScopeRef) *ChainLookup {
	return &ChainLookup{leaf: leaf, scope: claimScope, start: time.Now()}
}

// BeginChainLookup starts the chain read for a claim of leaf with claimScope,
// in one round trip that also reports whether a claim allocation row exists
// under allocationKey (see HasClaimAllocation). The EXISTS is queued first:
// a batch that fails repeats the first error on every later result, so an
// error in the chain read can never hide the EXISTS result, and the EXISTS is
// what the caller acts on before anything the chain read says.
//
// err is the EXISTS's own failure. A failure of the chain read is held for
// Resolve or Pending.
func BeginChainLookup(ctx context.Context, db BatchQuerier, leaf *ResolvedClass, claimScope map[string]ipam.ScopeRef, allocationKey string) (*ChainLookup, bool, error) {
	c := newChainLookup(leaf, claimScope)
	defer metrics.ObserveQuery("claim_lookup_batch", time.Now())

	b := &pgx.Batch{}
	b.Queue(hasClaimAllocationSQL, allocationKey)
	parentName := leaf.Spec.ParentClassName
	if parentName != "" {
		b.Queue(loadClassSQL, classStorageKey(leaf.Project, parentName))
	} else {
		b.Queue(offeringPoolsSQL, offeringPoolsArgs(leaf)...)
	}
	br := db.SendBatch(ctx, b)

	exists, err := scanHasClaimAllocation(br.QueryRow(), allocationKey)
	if err != nil {
		_ = br.Close()
		return nil, false, err
	}
	if parentName != "" {
		class, err := scanClass(br.QueryRow(), leaf.Project, parentName)
		c.parent = &loadedClass{class: class, err: err}
	} else {
		rows, err := br.Query()
		pools, err := scanOfferingPools(rows, err, leaf)
		c.offers = &offerRead{pools: pools, err: err}
	}
	if err := br.Close(); err != nil && !c.failedFirst() {
		return nil, false, fmt.Errorf("look up allocation %q: %w", allocationKey, err)
	}
	return c, exists, nil
}

// failedFirst reports whether the chain read in the first round trip failed.
func (c *ChainLookup) failedFirst() bool {
	return (c.parent != nil && c.parent.err != nil) || (c.offers != nil && c.offers.err != nil)
}

// identitySQL reads every level's identity row in one statement. A plain
// SELECT, never the INSERT-then-fallback form migration 002 forbids: a miss
// here only sends the level to claimPoolIdentity, which arbitrates.
const identitySQL = `SELECT i.class_name, i.scope_digest, i.pool_key
	   FROM ipam_pool_identity i
	   JOIN unnest($1::text[], $2::text[]) AS w(class_name, scope_digest)
	     ON i.class_name = w.class_name AND i.scope_digest = w.scope_digest`

// readChain finishes the chain read, once: the rest of the ancestry, the plan,
// and one batch with the root's offers and every level's identity row.
func (c *ChainLookup) readChain(ctx context.Context, db BatchQuerier) {
	if c.read {
		return
	}
	c.read = true

	if c.leaf.Spec.ParentClassName == "" {
		// No ancestry: the leaf draws straight from the pools offering it.
		if c.offers == nil {
			pools, err := offeringPools(ctx, db, c.leaf)
			c.offers = &offerRead{pools: pools, err: err}
		}
		return
	}

	ancestry, err := walkAncestry(ctx, db, c.leaf, c.parent)
	if err != nil {
		c.err = err
		return
	}
	levels, err := planLevels(ctx, ancestry, c.scope)
	if err != nil {
		c.err = err
		return
	}
	c.levels = levels

	defer metrics.ObserveQuery("resolve_chain_batch", time.Now())
	classes := make([]string, len(levels))
	digests := make([]string, len(levels))
	for i, level := range levels {
		classes[i], digests[i] = level.Class.Name, level.ScopeDigest
	}
	b := &pgx.Batch{}
	// Discovery first, as it ran before the lookups when each was a round
	// trip of its own.
	b.Queue(offeringPoolsSQL, offeringPoolsArgs(levels[0].Class)...)
	b.Queue(identitySQL, classes, digests)
	br := db.SendBatch(ctx, b)

	rows, err := br.Query()
	pools, err := scanOfferingPools(rows, err, levels[0].Class)
	c.offers = &offerRead{pools: pools, err: err}
	if err != nil {
		// Every later result repeats this error; none of them is the lookups'.
		_ = br.Close()
		return
	}
	c.identities, c.identityErr = scanIdentities(br, levels)
	if err := br.Close(); err != nil && c.identityErr == nil {
		c.identityErr = fmt.Errorf("look up pool identity for %q: %w", levels[0].Class.Name, err)
	}
}

// scanIdentities reads identitySQL's rows into one pool key per level.
func scanIdentities(br pgx.BatchResults, levels []CascadeLevel) ([]string, error) {
	rows, err := br.Query()
	if err != nil {
		return nil, fmt.Errorf("look up pool identity for %q: %w", levels[0].Class.Name, err)
	}
	defer rows.Close()
	found := map[[2]string]string{}
	for rows.Next() {
		var class, digest, poolKey string
		if err := rows.Scan(&class, &digest, &poolKey); err != nil {
			return nil, fmt.Errorf("look up pool identity for %q: %w", levels[0].Class.Name, err)
		}
		found[[2]string{class, digest}] = poolKey
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("look up pool identity for %q: %w", levels[0].Class.Name, err)
	}
	keys := make([]string, len(levels))
	for i, level := range levels {
		keys[i] = found[[2]string{level.Class.Name, level.ScopeDigest}]
	}
	return keys, nil
}

// rootSource is the pool the chain's root carves from, or the error that
// refuses the claim: no pool offers the root, or none serves the scope.
func (c *ChainLookup) rootSource(want *net.IPNet) (string, error) {
	if c.offers.err != nil {
		return "", c.offers.err
	}
	class := c.leaf
	if len(c.levels) > 0 {
		class = c.levels[0].Class
	}
	return pickOfferingPool(class, c.scope, want, c.offers.pools)
}

// existing is ResolveExistingPool from the chain read: the leaf pool, or the
// levels still to be provisioned, provisioning nothing.
func (c *ChainLookup) existing(ctx context.Context, db BatchQuerier) (string, []CascadeLevel, error) {
	c.readChain(ctx, db)
	if c.err != nil {
		return "", nil, c.err
	}
	// The root must be backed whether or not anything below it exists, so a
	// class backed by nothing fails here rather than looking satisfiable.
	source, err := c.rootSource(nil)
	if err != nil {
		return "", nil, err
	}
	if len(c.levels) == 0 {
		return source, nil, nil
	}
	if c.identityErr != nil {
		return "", nil, c.identityErr
	}
	poolKey := ""
	for i, key := range c.identities {
		if key == "" {
			return "", c.levels[i:], nil
		}
		poolKey = key
	}
	return poolKey, nil, nil
}

// Pending reports the levels of the chain that have no pool yet, provisioning
// nothing. A leaf with no parent has none.
func (c *ChainLookup) Pending(ctx context.Context, db BatchQuerier) ([]CascadeLevel, error) {
	_, pending, err := c.existing(ctx, db)
	return pending, err
}

// Resolve returns the key of the pool the claim draws from, provisioning any
// missing level on the way, as ResolvePoolFor does. db must be a pool, or the
// dry-run's transaction: each level commits on its own.
func (c *ChainLookup) Resolve(ctx context.Context, db TxBeginner, want *net.IPNet) (string, error) {
	// Resolution runs outside the allocation transaction, so its cost lands in
	// end-to-end claim latency without appearing in any query timing.
	result, provisioned := "error", false
	defer func() { metrics.ObserveCascadeResolution(ctx, result, provisioned, c.start) }()

	c.readChain(ctx, db)
	if c.err != nil {
		return "", c.err
	}

	// A leaf with no ancestry draws straight from whichever operator-authored
	// pool offers it. There is nothing to cascade.
	if len(c.levels) == 0 {
		poolKey, err := c.rootSource(want)
		if err == nil {
			result = "success"
		}
		return poolKey, err
	}

	// A claim naming its block never provisions. A level placed by first-free
	// to serve it would land without regard to the address, is never moved,
	// and would then make the chain refuse the ScopeRange claim that could
	// have placed it around the address. So the chain must already exist.
	if want != nil {
		poolKey, pending, err := c.existing(ctx, db)
		if err != nil {
			return "", err
		}
		if len(pending) > 0 {
			return "", unplacedChain(c.levels, pending, *want)
		}
		result = "success"
		return poolKey, nil
	}

	// The root-most level carves from an operator-authored pool offering its
	// class; every level below carves from the level above.
	source, err := c.rootSource(nil)
	if err != nil {
		return "", err
	}
	if c.identityErr != nil {
		metrics.RecordCascadeLevel(ctx, c.levels[0].Class.Name, levelError)
		return "", fmt.Errorf("provision pool for class %q: %w", c.levels[0].Class.Name, c.identityErr)
	}
	for i, level := range c.levels {
		if key := c.identities[i]; key != "" {
			metrics.RecordCascadeLevel(ctx, level.Class.Name, levelReused)
			source = key
			continue
		}
		poolKey, created, err := provisionLevel(ctx, db, level, source)
		if err != nil {
			return "", fmt.Errorf("provision pool for class %q: %w", level.Class.Name, err)
		}
		provisioned = provisioned || created
		source = poolKey
	}
	result = "success"
	return source, nil
}
