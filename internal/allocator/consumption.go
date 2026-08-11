package allocator

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"net"
	"time"

	"github.com/jackc/pgx/v5"

	"go.miloapis.com/ipam/internal/allocation"
	"go.miloapis.com/ipam/internal/metrics"
)

// readConsumed returns a pool's running consumption total, and whether one has
// been established yet.
func readConsumed(ctx context.Context, tx pgx.Tx, poolKey string) (*big.Int, bool, error) {
	defer metrics.ObserveQuery("read_consumption", time.Now())
	var s string
	err := tx.QueryRow(ctx,
		`SELECT consumed::text FROM ipam_pool_consumption WHERE pool_key = $1`,
		poolKey,
	).Scan(&s)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("read consumption for %q: %w", poolKey, err)
	}
	n, ok := new(big.Int).SetString(s, 10)
	if !ok {
		return nil, false, fmt.Errorf("consumption for %q is not an integer: %q", poolKey, s)
	}
	return n, true, nil
}

func writeConsumed(ctx context.Context, tx pgx.Tx, poolKey string, consumed *big.Int) error {
	defer metrics.ObserveQuery("write_consumption", time.Now())
	_, err := tx.Exec(ctx,
		`INSERT INTO ipam_pool_consumption (pool_key, consumed, updated_at)
		 VALUES ($1, $2::numeric, NOW())
		 ON CONFLICT (pool_key) DO UPDATE
		    SET consumed = EXCLUDED.consumed, updated_at = NOW()`,
		poolKey, consumed.String(),
	)
	if err != nil {
		return fmt.Errorf("write consumption for %q: %w", poolKey, err)
	}
	return nil
}

// loadOverlapping returns every allocation in the pool that intersects block.
//
// This is the bounded read a delta needs. It is served by the GiST index behind
// the EXCLUDE constraint, so its cost follows the number of allocations that
// actually overlap rather than the size of the pool.
//
// It spans every address space in the pool. Two allocations in different spaces
// may hold the same address legitimately, and the total counts that address
// once, so the query must not filter by scope digest.
func loadOverlapping(ctx context.Context, tx pgx.Tx, poolKey string, block net.IPNet, excludeKey string) ([]net.IPNet, error) {
	defer metrics.ObserveQuery("load_overlapping", time.Now())
	return queryAllocationCIDRs(ctx, tx,
		`SELECT host(allocated_cidr) || '/' || masklen(allocated_cidr)
		   FROM ipam_cidr_allocations
		  WHERE pool_key = $1
		    AND allocated_cidr && $2::cidr
		    AND ($3 = '' OR claim_key <> $3)`,
		poolKey, block.String(), excludeKey)
}

func queryAllocationCIDRs(ctx context.Context, tx pgx.Tx, sql string, args ...any) ([]net.IPNet, error) {
	rows, err := tx.Query(ctx, sql, args...)
	if err != nil {
		return nil, fmt.Errorf("load allocations: %w", err)
	}
	defer rows.Close()

	var out []net.IPNet
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			return nil, fmt.Errorf("scan allocation row: %w", err)
		}
		_, ipnet, err := net.ParseCIDR(s)
		if err != nil {
			return nil, fmt.Errorf("parse stored cidr %q: %w", s, err)
		}
		out = append(out, *ipnet)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate allocation rows: %w", err)
	}
	return out, nil
}

// consumptionAfterAllocate returns the pool's total once block is added.
//
// A pool with no total yet gets one from the full set, once. Every later write
// applies a delta, so the walk happens on a pool's first write rather than on
// each of them.
func consumptionAfterAllocate(ctx context.Context, tx pgx.Tx, poolKey string, parents []net.IPNet, block net.IPNet) (*big.Int, error) {
	overlapping, err := loadOverlapping(ctx, tx, poolKey, block, "")
	if err != nil {
		return nil, err
	}

	base, ok, err := readConsumed(ctx, tx, poolKey)
	if err != nil {
		return nil, err
	}
	if !ok {
		if base, err = seedConsumption(ctx, tx, poolKey, parents); err != nil {
			return nil, err
		}
	}

	delta, err := allocation.AddedConsumption(parents, overlapping, block)
	if err != nil {
		return nil, fmt.Errorf("consumption delta for %v: %w", block, err)
	}
	return new(big.Int).Add(base, delta), nil
}

// consumptionAfterRelease returns the pool's total once block is released.
// overlapping must exclude the released allocation itself.
func consumptionAfterRelease(ctx context.Context, tx pgx.Tx, poolKey string, parents []net.IPNet, block net.IPNet) (*big.Int, error) {
	overlapping, err := loadOverlapping(ctx, tx, poolKey, block, "")
	if err != nil {
		return nil, err
	}

	base, ok, err := readConsumed(ctx, tx, poolKey)
	if err != nil {
		return nil, err
	}
	if !ok {
		// No total to adjust: the pool's first write is a release. Measure what
		// remains rather than subtracting from nothing.
		return seedConsumption(ctx, tx, poolKey, parents)
	}

	delta, err := allocation.RemovedConsumption(parents, overlapping, block)
	if err != nil {
		return nil, fmt.Errorf("consumption delta for %v: %w", block, err)
	}
	freed := new(big.Int).Sub(base, delta)
	if freed.Sign() < 0 {
		// Unreachable while every write goes through these two functions in one
		// transaction. Clamping keeps a corrupted total from failing the CHECK
		// and taking the release with it: a wrong number is recoverable by a
		// reseed, a claim that cannot be deleted is not.
		return new(big.Int), nil
	}
	return freed, nil
}

// seedConsumption measures the pool's current occupancy from the full set. It
// is the one unbounded read, and it happens once per pool.
func seedConsumption(ctx context.Context, tx pgx.Tx, poolKey string, parents []net.IPNet) (*big.Int, error) {
	existing, err := loadExistingAllocations(ctx, tx, poolKey)
	if err != nil {
		return nil, err
	}
	m, err := allocation.Measure(parents, existing, allocation.Reservation{})
	if err != nil {
		return nil, fmt.Errorf("measure pool %q: %w", poolKey, err)
	}
	return m.Consumed, nil
}
