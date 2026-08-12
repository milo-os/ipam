package allocator

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"go.miloapis.com/ipam/internal/metrics"
	"go.miloapis.com/ipam/internal/scope"
	ipamv1alpha1 "go.miloapis.com/ipam/pkg/apis/ipam/v1alpha1"
)

// ClassSpace is a class together with the roles that decide what counts as one
// address space for it.
type ClassSpace struct {
	Name         string
	UniqueWithin []string
}

// SpaceKey is the value two classes must share for one pool to serve both. It
// ignores order and duplicates, so [network, location] and [location, network]
// describe the same space.
func (c ClassSpace) SpaceKey() string { return scope.RoleSetKey(c.UniqueWithin) }

// Disagreement reports two classes that would divide one pool into address
// spaces that cannot see each other.
type Disagreement struct {
	A, B ClassSpace
}

func (d *Disagreement) Error() string {
	return fmt.Sprintf("IPClass %q is unique within %s but IPClass %q is unique within %s; "+
		"a pool serving both would let one class hand out addresses the other already holds, "+
		"because non-overlap is only enforced inside a single address space",
		d.A.Name, describeRoles(d.A.UniqueWithin), d.B.Name, describeRoles(d.B.UniqueWithin))
}

func describeRoles(roles []string) string {
	if len(roles) == 0 {
		return "the whole pool"
	}
	return strings.Join(roles, "+")
}

// FirstDisagreement returns the first space that keys differently from the
// first, or nil when every space agrees. Agreement is transitive, so one pass
// against spaces[0] settles the whole set.
func FirstDisagreement(spaces []ClassSpace) *Disagreement {
	for i := 1; i < len(spaces); i++ {
		if spaces[i].SpaceKey() != spaces[0].SpaceKey() {
			return &Disagreement{A: spaces[0], B: spaces[i]}
		}
	}
	return nil
}

// OfferedSpaces returns one ClassSpace per named class that would really
// allocate from a pool in project.
//
// The set matches DiscoverPool's. A pool backs only class definitions held in
// its own project, discovery keys on the definition's own name, and only the
// root of a parent chain draws from the pools offering it. Every other name a
// pool lists is inert — no claim reaches the pool through it — so holding those
// to the agreement rule would reject offers that cannot collide. Names that do
// not resolve are inert for the same reason and are skipped rather than
// reported: whether a pool may name a class that does not exist is a separate
// question from whether its classes agree.
func OfferedSpaces(ctx context.Context, tx pgx.Tx, project string, names []string) ([]ClassSpace, error) {
	spaces := make([]ClassSpace, 0, len(names))
	for _, name := range names {
		class, err := loadClassIn(ctx, tx, project, name)
		if err != nil {
			if errors.Is(err, ErrClassNotFound) {
				continue
			}
			return nil, err
		}
		if class.Spec.Source != nil || class.Spec.ParentClassName != "" {
			continue
		}
		spaces = append(spaces, ClassSpace{Name: name, UniqueWithin: class.Spec.UniqueWithin})
	}
	return spaces, nil
}

// PoolOffer is a pool and the classes it publishes itself to.
type PoolOffer struct {
	Name       string
	ClassNames []string
}

// PoolsOffering returns the pools in project that publish themselves to
// className, in name order.
func PoolsOffering(ctx context.Context, tx pgx.Tx, project, className string) ([]PoolOffer, error) {
	defer metrics.ObserveQuery("pools_offering_class", time.Now())

	rows, err := tx.Query(ctx,
		`SELECT obj.name, obj.data
		   FROM ipam_pool_class_offer o
		   JOIN ipam_objects obj ON obj.key = o.pool_key
		  WHERE o.class_name = $1
		    AND obj.kind = 'IPPool'
		    AND obj.key LIKE $2
		  ORDER BY obj.name`,
		className, resourceKeyPrefixFor(project, "ippools")+"%",
	)
	if err != nil {
		return nil, fmt.Errorf("list pools offering class %q: %w", className, err)
	}
	defer rows.Close()

	var out []PoolOffer
	for rows.Next() {
		var name string
		var data []byte
		if err := rows.Scan(&name, &data); err != nil {
			return nil, fmt.Errorf("scan pool row: %w", err)
		}
		var pool ipamv1alpha1.IPPool
		if err := json.Unmarshal(data, &pool); err != nil {
			return nil, fmt.Errorf("decode pool %q: %w", name, err)
		}
		out = append(out, PoolOffer{Name: name, ClassNames: pool.Spec.ClassNames})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate pool rows: %w", err)
	}
	return out, nil
}

// ClassJoinsDisagreement reports a pool that already offers class.Name and
// serves another class that disagrees with it, naming the pool.
//
// This is the pool's own rule asked from the other side, for the case where the
// class is the thing arriving: a pool may list a class that does not exist yet,
// and a class deleted and recreated keeps every pool that named it.
func ClassJoinsDisagreement(ctx context.Context, tx pgx.Tx, project string, class ClassSpace) (string, *Disagreement, error) {
	offers, err := PoolsOffering(ctx, tx, project, class.Name)
	if err != nil {
		return "", nil, err
	}
	for _, offer := range offers {
		spaces, err := OfferedSpaces(ctx, tx, project, offer.ClassNames)
		if err != nil {
			return "", nil, err
		}
		if d := FirstDisagreement(append(spaces, class)); d != nil {
			return offer.Name, d, nil
		}
	}
	return "", nil, nil
}
