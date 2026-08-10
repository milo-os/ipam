package allocator_test

// InsertObject must translate a primary-key collision on ipam_objects into
// ErrObjectExists, with nothing of the driver's left in the message.
//
// This needs the real database. The translation reads a *pgconn.PgError with
// SQLSTATE 23505 raised by a specific constraint, and a fake cannot produce
// one — the last version of this check that used a hand-built error proved only
// that errors.As works.
//
//	docker run -d -e POSTGRES_PASSWORD=postgres -p 55432:5432 postgres:17-alpine
//	IPAM_TEST_POSTGRES_DSN="postgres://postgres:postgres@127.0.0.1:55432/postgres?sslmode=disable" \
//	    go test ./internal/allocator/ -run InsertObject -count=1

import (
	"context"
	"errors"
	"strings"
	"testing"

	"go.miloapis.com/ipam/internal/allocator"
	"go.miloapis.com/ipam/internal/testdb"
)

func TestInsertObjectTranslatesDuplicateKey(t *testing.T) {
	pool := testdb.Pool(t, "ipam_insert_conflict_test")
	ctx := context.Background()
	a := allocator.NewPostgresPrefixAllocator()

	const key = "project/p1/ipam.miloapis.com/ippools/dup"
	data := []byte(`{"apiVersion":"ipam.miloapis.com/v1alpha1","kind":"IPPool","metadata":{"name":"dup"}}`)

	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := a.InsertObject(ctx, tx, key, "IPPool", "", "dup", data); err != nil {
		t.Fatalf("first insert should succeed: %v", err)
	}

	// The second insert runs in its own transaction: the first one is aborted by
	// the failed statement, and reusing it would make every later statement fail
	// with "current transaction is aborted" — a different error than the one
	// under test.
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit first insert: %v", err)
	}

	tx2, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin second: %v", err)
	}
	defer func() { _ = tx2.Rollback(ctx) }()

	_, err = a.InsertObject(ctx, tx2, key, "IPPool", "", "dup", data)
	if err == nil {
		t.Fatal("second insert of the same key succeeded; the primary key is not doing its job")
	}
	if !errors.Is(err, allocator.ErrObjectExists) {
		t.Fatalf("error is not ErrObjectExists: %v", err)
	}

	// The reason the sentinel exists: none of this may reach an API client.
	for _, leak := range []string{"SQLSTATE", "23505", "ipam_objects_pkey", "duplicate key"} {
		if strings.Contains(err.Error(), leak) {
			t.Errorf("error leaks driver internals (%q): %v", leak, err)
		}
	}
	// It must still say what collided, or the operator gets a sentinel and no
	// subject.
	if !strings.Contains(err.Error(), "dup") {
		t.Errorf("error does not name the object: %v", err)
	}
}
