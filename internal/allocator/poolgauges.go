package allocator

import (
	"context"
	"fmt"
	"math/big"
	"time"

	"github.com/jackc/pgx/v5"

	"go.miloapis.com/ipam/internal/metrics"
	"go.miloapis.com/ipam/internal/tenant"
)

// Querier is the read half of a pgx pool or transaction. A read that needs no
// lock and no other statement's view takes one, so it can run on the pool
// without a transaction: under READ COMMITTED a statement on its own sees what
// the same statement sees inside a short transaction, committed data as of the
// statement, and BEGIN and ROLLBACK around it are two round trips that buy
// nothing.
type Querier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// LoadPoolGauges reads every IPPool's capacity as committed, for the per-pool
// gauges.
//
// The figures are the pool's own status.capacity, which every write that
// changes a pool's occupancy persists in the same transaction. Reading them
// back, rather than publishing them from that transaction, is what keeps a
// rolled-back write (a dry-run, a refused claim) and a deleted pool out of the
// gauges. A pool whose capacity has not been computed yet is left out rather
// than reported as empty.
//
// The cost is parsing each pool document once in postgres and sending back
// four short strings per pool. Each document is parsed exactly once: the
// MATERIALIZED CTE stops the planner inlining the parse into every reference,
// which parsed it four times. Measured at 10,000 pools of about 2KB each, the
// read takes 0.35-0.55s on a laptop (1-1.5s before), 2-4% of the 15s
// interval, per replica. The set is read whole, without a LIMIT or pages: a partial set
// would stop exporting the pools it left out, and pages would add round trips
// without reducing the parsing.
func LoadPoolGauges(ctx context.Context, db Querier) ([]metrics.PoolGauge, error) {
	defer metrics.ObserveQuery("load_pool_gauges", time.Now())
	rows, err := db.Query(ctx,
		`WITH pools AS MATERIALIZED (
		     SELECT key, ipam_data_to_jsonb(data) AS d
		       FROM ipam_objects
		      WHERE kind = 'IPPool')
		 SELECT p.key,
		        COALESCE(NULLIF(d -> 'status' ->> 'ipFamily', ''), d -> 'spec' ->> 'ipFamily', ''),
		        COALESCE(d -> 'status' -> 'capacity' ->> 'total', ''),
		        COALESCE(d -> 'status' -> 'capacity' ->> 'allocated', ''),
		        COALESCE(NULLIF(d -> 'status' ->> 'allocatedCIDR', ''), d -> 'spec' ->> 'cidr', ''),
		        c.consumed::text
		   FROM pools p
		   LEFT JOIN ipam_pool_consumption c ON c.pool_key = p.key
		  ORDER BY p.key`)
	if err != nil {
		return nil, fmt.Errorf("load pool gauges: %w", err)
	}
	defer rows.Close()

	var out []metrics.PoolGauge
	for rows.Next() {
		var key, family, total, allocated, cidr string
		var consumed *string
		if err := rows.Scan(&key, &family, &total, &allocated, &cidr, &consumed); err != nil {
			return nil, fmt.Errorf("scan pool gauge: %w", err)
		}
		// A pool that has been written to has a running total; its stored
		// status does not track it (see PoolCapacityFromConsumed).
		if consumed != nil {
			if n, ok := new(big.Int).SetString(*consumed, 10); ok {
				if t, a, _, _, ok := PoolCapacityFromConsumed(cidr, n); ok {
					total, allocated = t, a
				}
			}
		}
		totalF, ok := decimalFloat(total)
		if !ok {
			continue
		}
		allocatedF, ok := decimalFloat(allocated)
		if !ok {
			allocatedF = 0
		}
		out = append(out, metrics.PoolGauge{
			PoolKey:  key,
			IPFamily: family,
			Resource: "ippools",
			// The key encodes only the immediate parent (the project, for a
			// project-scoped pool), so org is "" today; see
			// tenant.Identity.Org.
			Project:   tenant.ProjectFromKey(key),
			Total:     totalF,
			Allocated: allocatedF,
		})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate pool gauges: %w", err)
	}
	return out, nil
}

// decimalFloat converts one of status.capacity's decimal strings to a float64.
// The counts are strings because an IPv6 pool overflows int64.
func decimalFloat(s string) (float64, bool) {
	if s == "" {
		return 0, false
	}
	n, ok := new(big.Int).SetString(s, 10)
	if !ok {
		return 0, false
	}
	f, _ := new(big.Float).SetInt(n).Float64()
	return f, true
}
