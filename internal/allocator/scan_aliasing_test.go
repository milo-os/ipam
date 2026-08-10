package allocator

import (
	"bytes"
	"context"
	"fmt"
	"runtime"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Every decode path in this package scans an object's JSON into a []byte and
// unmarshals it — lockAndDecodeIPPool, LoadClass, the sweeper's object read, the
// claim registry's allocation read. All of them assume the scanned slice is
// theirs.
//
// If pgx instead handed back a slice pointing into the connection's read buffer,
// every one of those would be reading memory that a later read on the same
// connection can overwrite: a use-after-free whose signature is torn slice
// headers, "found pointer to free object", and a crash threshold that moves with
// load. That is close enough to a real crash under investigation (#35) that the
// assumption is worth pinning rather than trusting.
//
// pgx v5 copies today. This test exists so a version bump that changes it fails
// here, loudly and locally, instead of in a heap corruption three layers away.
func TestScannedBytesDoNotAliasTheConnectionBuffer(t *testing.T) {
	db := newMigratedPool(t)
	ctx := platformCtx()

	// Two rows with distinguishable payloads, big enough to matter.
	for i, fill := range []byte{'A', 'B'} {
		payload := append(append([]byte(`{"pad":"`), bytes.Repeat([]byte{fill}, 4096)...), []byte(`"}`)...)
		seedRawObject(t, db, ctx, i, payload)
	}

	tx, err := db.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var first []byte
	if err := tx.QueryRow(ctx, `SELECT data FROM ipam_objects WHERE key = $1`, "/alias/0").Scan(&first); err != nil {
		t.Fatalf("scan first: %v", err)
	}
	firstCopy := append([]byte(nil), first...)

	// Read the other row on the same connection, then churn the heap. If `first`
	// aliased the read buffer, its contents change underneath us.
	var second []byte
	if err := tx.QueryRow(ctx, `SELECT data FROM ipam_objects WHERE key = $1`, "/alias/1").Scan(&second); err != nil {
		t.Fatalf("scan second: %v", err)
	}
	for range 50 {
		_ = make([]byte, 1<<16)
	}
	runtime.GC()

	if !bytes.Equal(first, firstCopy) {
		t.Fatalf("the first scan aliased pgx's connection buffer: its contents changed after a later read on the same connection.\n" +
			"Every decode path in this package unmarshals such a slice, so this is a use-after-free of a reused buffer.")
	}
	if !bytes.Contains(first, bytes.Repeat([]byte{'A'}, 64)) || !bytes.Contains(second, bytes.Repeat([]byte{'B'}, 64)) {
		t.Fatal("scans returned the wrong payloads")
	}
}

func seedRawObject(t *testing.T, db *pgxpool.Pool, ctx context.Context, i int, payload []byte) {
	t.Helper()
	if _, err := db.Exec(ctx,
		`INSERT INTO ipam_objects (key, kind, namespace, name, data) VALUES ($1, 'IPPool', '', $2, $3)`,
		fmt.Sprintf("/alias/%d", i), fmt.Sprintf("alias-%d", i), payload); err != nil {
		t.Fatalf("seed: %v", err)
	}
}
