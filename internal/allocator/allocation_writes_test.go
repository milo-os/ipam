package allocator

import (
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// fakeBatch replays one result per queued statement, as pgx reads them in
// order. err[i] is the i-th result's error; noRow[i] makes the i-th QueryRow
// return no row, which is not an error to pgx.
type fakeBatch struct {
	errs  []error
	noRow map[int]bool
	next  int
}

func (b *fakeBatch) result() (int, error) {
	i := b.next
	b.next++
	if i < len(b.errs) {
		return i, b.errs[i]
	}
	return i, nil
}

func (b *fakeBatch) Exec() (pgconn.CommandTag, error) {
	_, err := b.result()
	return pgconn.CommandTag{}, err
}

func (b *fakeBatch) Query() (pgx.Rows, error) {
	_, err := b.result()
	return nil, err
}

func (b *fakeBatch) QueryRow() pgx.Row {
	i, err := b.result()
	if err == nil && b.noRow[i] {
		err = pgx.ErrNoRows
	}
	return fakeRow{err: err}
}

func (b *fakeBatch) Close() error { return nil }
func (b *fakeBatch) Tx() pgx.Tx   { return nil }

type fakeRow struct{ err error }

func (r fakeRow) Scan(dest ...any) error {
	if r.err != nil {
		return r.err
	}
	if p, ok := dest[0].(*int64); ok {
		*p = 1
	}
	return nil
}

// repeat is a failed batch: its first error, repeated on every later result.
func repeat(err error, n int) []error {
	out := make([]error, n)
	for i := range out {
		out[i] = err
	}
	return out
}

func TestAllocationWritesAreClassifiedByTheFirstFailure(t *testing.T) {
	overlap := &pgconn.PgError{Code: "23P01", ConstraintName: overlapConstraint}
	holder := &pgconn.PgError{Code: "23505", ConstraintName: "ipam_cidr_allocations_claim_key_key"}
	named := PrefixRequest{PoolKey: "pools/p", Address: "10.0.0.16/28"}
	searched := PrefixRequest{PoolKey: "pools/p"}
	raise := &floorRaise{poolKey: "pools/p", scopeDigest: "d"}

	t.Run("a named address losing an overlap race is AddressInUse", func(t *testing.T) {
		err := readAllocationWrites(&fakeBatch{errs: repeat(overlap, 3)}, named, "10.0.0.16/28", raise, true)
		var inUse *AddressInUseError
		if !errors.As(err, &inUse) {
			t.Fatalf("error = %v, want AddressInUseError", err)
		}
	})

	t.Run("a searched block's overlap is not an address refusal", func(t *testing.T) {
		err := readAllocationWrites(&fakeBatch{errs: repeat(overlap, 3)}, searched, "10.0.0.16/28", raise, true)
		var inUse *AddressInUseError
		if errors.As(err, &inUse) || !strings.HasPrefix(err.Error(), "insert allocation:") {
			t.Fatalf("error = %v, want the insert's own error", err)
		}
	})

	t.Run("a holder collision is the insert's, not a later statement's", func(t *testing.T) {
		err := readAllocationWrites(&fakeBatch{errs: repeat(holder, 3)}, searched, "10.0.0.16/28", raise, true)
		var pgErr *pgconn.PgError
		if !strings.HasPrefix(err.Error(), "insert allocation:") || !errors.As(err, &pgErr) || pgErr.ConstraintName != holder.ConstraintName {
			t.Fatalf("error = %v, want the insert's unique violation, unwrappable", err)
		}
	})

	t.Run("every write succeeding is success", func(t *testing.T) {
		if err := readAllocationWrites(&fakeBatch{}, searched, "10.0.0.16/28", raise, true); err != nil {
			t.Fatalf("error = %v, want nil", err)
		}
	})
}
