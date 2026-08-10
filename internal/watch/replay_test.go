package watch_test

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/serializer"
	"k8s.io/apimachinery/pkg/runtime/serializer/json"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/apiserver/pkg/authentication/user"
	"k8s.io/apiserver/pkg/endpoints/request"
	"k8s.io/apiserver/pkg/storage"

	pgstore "go.miloapis.com/ipam/internal/storage/postgres"
	"go.miloapis.com/ipam/internal/tenant"
	pgwatch "go.miloapis.com/ipam/internal/watch"
	"go.miloapis.com/ipam/pkg/apis/ipam/install"
	"go.miloapis.com/ipam/pkg/apis/ipam/v1alpha1"

	"go.miloapis.com/ipam/internal/testdb"
)

// This file is about one promise: a watch opened at resourceVersion=R must
// deliver the events that happened after R but before the watch connected.
// That is the promise a reconnecting informer is built on — after a
// disconnect it re-watches from its last seen RV and expects the gap to be
// filled. Delivering nothing, with no error, is the one failure mode an
// informer cannot detect.
//
// The tests need a live PostgreSQL. internal/testdb finds one — the DSN in
// IPAM_TEST_POSTGRES_DSN, or a throwaway container for this package — and pins
// the image for the whole repo, which moves these from 16-alpine to 17-alpine.

// schemaDDL mirrors migrations/001_initial_schema.sql: the RV sequence, the
// durable object table, the prunable changelog with its commit_xid horizon
// column, and the NOTIFY trigger the watcher LISTENs for. Kept inline so the
// test exercises the real SQL without dragging in the goose runner.
const schemaDDL = `
CREATE SEQUENCE IF NOT EXISTS ipam_resource_version_seq;

CREATE TABLE IF NOT EXISTS ipam_objects (
    key              TEXT PRIMARY KEY,
    resource_version BIGINT NOT NULL DEFAULT nextval('ipam_resource_version_seq'),
    kind             TEXT NOT NULL,
    namespace        TEXT NOT NULL DEFAULT '',
    name             TEXT NOT NULL,
    data             BYTEA NOT NULL,
    labels           jsonb NOT NULL DEFAULT '{}',
    created_at       TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at       TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE TABLE IF NOT EXISTS ipam_changelog (
    id               BIGSERIAL PRIMARY KEY,
    key              TEXT NOT NULL,
    resource_version BIGINT NOT NULL,
    event_type       TEXT NOT NULL CHECK (event_type IN ('ADDED', 'MODIFIED', 'DELETED')),
    data             BYTEA,
    commit_xid       BIGINT NOT NULL DEFAULT (pg_current_xact_id()::text::bigint),
    created_at       TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE OR REPLACE FUNCTION ipam_notify_changelog() RETURNS trigger AS $$
BEGIN
    PERFORM pg_notify('ipam_changes', NEW.resource_version::text);
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS ipam_changelog_notify ON ipam_changelog;
CREATE TRIGGER ipam_changelog_notify
    AFTER INSERT ON ipam_changelog
    FOR EACH ROW EXECUTE FUNCTION ipam_notify_changelog();
`

const watchKeyPrefix = "/ipam.miloapis.com/ippools"

// eventTimeout bounds how long a test waits for events. The watcher's safety
// poll ticker is 5s and the replay path gets no NOTIFY (the writes already
// happened), so this must comfortably exceed one tick.
const eventTimeout = 20 * time.Second

func newTestCodec(t *testing.T) runtime.Codec {
	t.Helper()
	scheme := runtime.NewScheme()
	install.Install(scheme)
	cf := serializer.NewCodecFactory(scheme)
	ser := json.NewSerializerWithOptions(json.DefaultMetaFactory, scheme, scheme, json.SerializerOptions{})
	return cf.CodecForVersions(ser, ser, v1alpha1.SchemeGroupVersion, v1alpha1.SchemeGroupVersion)
}

// watchTestProject is the project every object in this file is written under.
//
// Writes must be project-scoped — every object this service stores belongs to a
// project. These tests previously wrote with context.Background(), which landed
// them in the unprefixed keyspace no read path consults; the watch assertions
// still passed because the watcher was reading the same wrong place.
const watchTestProject = "datum-cloud"

// writeCtx returns a context carrying watchTestProject, mimicking what Milo's
// front gate forwards after authentication.
func writeCtx() context.Context {
	return request.WithUser(context.Background(), &user.DefaultInfo{
		Name: "watch-test",
		Extra: map[string][]string{
			tenant.ExtraParentAPIGroup: {tenant.ParentAPIGroupProject},
			tenant.ExtraParentType:     {tenant.ParentTypeProject},
			tenant.ExtraParentName:     {watchTestProject},
		},
	})
}

// createPool writes one IPPool through the real Store and returns the
// resource version it was assigned.
func createPool(t *testing.T, store *pgstore.Store, name string) string {
	t.Helper()
	in := &v1alpha1.IPPool{ObjectMeta: metav1.ObjectMeta{Name: name}}
	in.SetGroupVersionKind(v1alpha1.SchemeGroupVersion.WithKind("IPPool"))
	out := &v1alpha1.IPPool{}
	if err := store.Create(writeCtx(), watchKeyPrefix+"/"+name, in, out, 0); err != nil {
		t.Fatalf("create %s: %v", name, err)
	}
	return out.ResourceVersion
}

// collect drains w until it has seen want non-bookmark events or the timeout
// expires. It returns the names carried by the events it saw, in order.
func collect(t *testing.T, w watch.Interface, want int) []string {
	t.Helper()
	var got []string
	deadline := time.After(eventTimeout)
	for len(got) < want {
		select {
		case ev, ok := <-w.ResultChan():
			if !ok {
				return got
			}
			if ev.Type == watch.Bookmark {
				continue
			}
			if ev.Type == watch.Error {
				t.Fatalf("watch returned an error event: %#v", ev.Object)
			}
			pool, ok := ev.Object.(*v1alpha1.IPPool)
			if !ok {
				t.Fatalf("unexpected object type %T", ev.Object)
			}
			got = append(got, string(ev.Type)+":"+pool.Name)
		case <-deadline:
			return got
		}
	}
	return got
}

// TestWatchDeliversEventsCreatedAfterItConnects is the positive control for
// TestWatchReplaysFromResourceVersion below. Without it, a replay assertion
// that fails proves nothing: a harness that never delivers any event at all
// fails the replay test for a reason that has nothing to do with replay.
func TestWatchDeliversEventsCreatedAfterItConnects(t *testing.T) {
	db, dsn := startEphemeralPostgres(t)
	store := newStore(t, db, dsn)

	w, err := store.Watch(writeCtx(), watchKeyPrefix, storage.ListOptions{Predicate: storage.Everything})
	if err != nil {
		t.Fatalf("watch: %v", err)
	}
	defer w.Stop()

	createPool(t, store, "after-connect")

	got := collect(t, w, 1)
	if len(got) == 0 {
		t.Fatal("watch delivered 0 events for a create that happened while it was connected; " +
			"the harness itself is broken, so no conclusion can be drawn from the replay test")
	}
	if got[0] != "ADDED:after-connect" {
		t.Errorf("got %v, want [ADDED:after-connect]", got)
	}
}

// TestWatchReplaysFromResourceVersion is the defect. A client holds RV R (from
// a list, or from the last event it saw before a disconnect), misses three
// creates, and re-watches from R. All three must arrive.
func TestWatchReplaysFromResourceVersion(t *testing.T) {
	db, dsn := startEphemeralPostgres(t)
	store := newStore(t, db, dsn)

	// The anchor stands in for whatever the client last saw. Its RV is the
	// resume point.
	resumeRV := createPool(t, store, "anchor")

	// Three creates the client misses while disconnected.
	want := []string{"ADDED:missed-1", "ADDED:missed-2", "ADDED:missed-3"}
	for _, name := range []string{"missed-1", "missed-2", "missed-3"} {
		createPool(t, store, name)
	}

	w, err := store.Watch(writeCtx(), watchKeyPrefix, storage.ListOptions{
		ResourceVersion: resumeRV, Predicate: storage.Everything,
	})
	if err != nil {
		t.Fatalf("watch from rv %s: %v", resumeRV, err)
	}
	defer w.Stop()

	got := collect(t, w, len(want))

	// Sample-count control first. Every assertion below is vacuous over an
	// empty slice, and delivering nothing is exactly the current behaviour.
	if len(got) == 0 {
		t.Fatalf("watch from resourceVersion=%s delivered 0 events in %s; "+
			"3 creates happened after that version and all 3 must be replayed",
			resumeRV, eventTimeout)
	}
	if len(got) != len(want) {
		t.Fatalf("watch from resourceVersion=%s delivered %d events (%v), want %d (%v)",
			resumeRV, len(got), got, len(want), want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("event %d = %s, want %s (full stream: %v)", i, got[i], want[i], got)
		}
	}
	// The anchor is at or before the resume point and must NOT be replayed:
	// "after R" is exclusive of R.
	for _, ev := range got {
		if strings.HasSuffix(ev, ":anchor") {
			t.Errorf("replay re-delivered the anchor event at the resume point itself: %v", got)
		}
	}
}

func newStore(t *testing.T, db *sql.DB, dsn string) *pgstore.Store {
	t.Helper()
	codec := newTestCodec(t)
	store := pgstore.NewWithWatchExclusions(db, codec, dsn, nil)
	store.SetNewFunc(func() runtime.Object { return &v1alpha1.IPPool{} })
	t.Cleanup(store.Stop)
	return store
}

// startEphemeralPostgres returns an open *sql.DB on a private database with
// schemaDDL applied, plus its DSN (the watcher needs the DSN for its dedicated
// LISTEN connection).
//
// It used to start a Postgres container per test. internal/testdb now owns
// backend selection for the whole repo, so at most one container is started per
// package and the skip condition is stated in one place.
//
// A private DATABASE rather than a private schema, which is the exception
// testdb.PrivateDatabase exists for: LISTEN/NOTIFY channel names are scoped to
// the database. Sharing one would have these tests hear each other's changelog
// notifications, and — with IPAM_TEST_POSTGRES_DSN pointed at a dev cluster —
// hear the live service's.
func startEphemeralPostgres(t *testing.T) (*sql.DB, string) {
	t.Helper()

	dsn := testdb.PrivateDatabase(t, "ipam_watch_"+testDBName(t))
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	if _, err := db.ExecContext(context.Background(), schemaDDL); err != nil {
		t.Fatalf("apply schema: %v", err)
	}
	return db, dsn
}

// testDBName derives a unique, legal identifier from the test name.
func testDBName(t *testing.T) string {
	t.Helper()
	var b strings.Builder
	for _, r := range strings.ToLower(t.Name()) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		} else {
			b.WriteByte('_')
		}
	}
	return b.String()
}

// TestMain removes the throwaway Postgres container testdb starts when
// IPAM_TEST_POSTGRES_DSN is unset. It is shared by every test in the package,
// so no single test can own its teardown.
func TestMain(m *testing.M) { testdb.TestMain(m) }

// TestWatchFromCompactedResourceVersionIsRejected covers the boundary the
// replay promise cannot be kept past. The changelog is pruned on a retention
// window, so a client disconnected longer than that window is asking for
// events that no longer exist. Kubernetes' answer is 410 Gone, which an
// informer handles by relisting. Delivering the surviving suffix and nothing
// else is silent, unrecoverable data loss.
func TestWatchFromCompactedResourceVersionIsRejected(t *testing.T) {
	db, dsn := startEphemeralPostgres(t)
	store := newStore(t, db, dsn)

	resumeRV := createPool(t, store, "anchor")
	for _, name := range []string{"missed-1", "missed-2", "missed-3"} {
		createPool(t, store, name)
	}

	// Prune exactly what the cleanup loop prunes: everything older than the
	// retention window. Here that is the anchor and the first two creates.
	if _, err := db.Exec(
		`DELETE FROM ipam_changelog WHERE key <> $1`, watchKeyPrefix+"/missed-3",
	); err != nil {
		t.Fatalf("prune changelog: %v", err)
	}

	w, err := store.Watch(writeCtx(), watchKeyPrefix, storage.ListOptions{
		ResourceVersion: resumeRV, Predicate: storage.Everything,
	})
	if err == nil {
		defer w.Stop()
		got := collect(t, w, 3)
		t.Fatalf("watch from compacted resourceVersion=%s returned no error and delivered %v; "+
			"missed-1 and missed-2 no longer exist and can never be delivered, so the client "+
			"must be told to relist (410 Gone / ResourceExpired)", resumeRV, got)
	}
	if !apierrors.IsResourceExpired(err) && !apierrors.IsGone(err) {
		t.Errorf("watch from compacted resourceVersion=%s returned %v (%T); want a 410 Gone / ResourceExpired error",
			resumeRV, err, err)
	}
}

// TestWatchWithEmptyResourceVersionStartsFromNow pins the third of the three
// resourceVersion modes. "" means "start from now": the client did not ask for
// history and must not be handed any. "0" and a specific RV are covered above
// and below.
func TestWatchWithEmptyResourceVersionStartsFromNow(t *testing.T) {
	db, dsn := startEphemeralPostgres(t)
	store := newStore(t, db, dsn)

	for _, name := range []string{"before-1", "before-2"} {
		createPool(t, store, name)
	}

	w, err := store.Watch(writeCtx(), watchKeyPrefix, storage.ListOptions{
		Predicate: storage.Everything,
	})
	if err != nil {
		t.Fatalf("watch: %v", err)
	}
	defer w.Stop()

	createPool(t, store, "after")

	got := collect(t, w, 1)
	// Sample-count control: an empty stream would satisfy "no history was
	// delivered" for the wrong reason.
	if len(got) == 0 {
		t.Fatal("watch delivered 0 events; the create after the watch connected must arrive")
	}
	if got[0] != "ADDED:after" {
		t.Errorf("first event = %s, want ADDED:after — resourceVersion=\"\" means start from now, "+
			"but history was replayed (full stream: %v)", got[0], got)
	}
}

// TestWatchFromResourceVersionZeroDeliversNewEvents pins "0" = "any version".
// Replaying retained history is permitted here; dropping new events is not.
func TestWatchFromResourceVersionZeroDeliversNewEvents(t *testing.T) {
	db, dsn := startEphemeralPostgres(t)
	store := newStore(t, db, dsn)

	createPool(t, store, "before")

	w, err := store.Watch(writeCtx(), watchKeyPrefix, storage.ListOptions{
		ResourceVersion: "0", Predicate: storage.Everything,
	})
	if err != nil {
		t.Fatalf("watch from rv 0: %v", err)
	}
	defer w.Stop()

	createPool(t, store, "after")

	got := collect(t, w, 2)
	if len(got) == 0 {
		t.Fatal("watch from resourceVersion=0 delivered 0 events")
	}
	if got[len(got)-1] != "ADDED:after" {
		t.Errorf("last event = %s, want ADDED:after (full stream: %v)", got[len(got)-1], got)
	}
}

// TestPruneLeavesRetainedSetAsSuffix asserts the property the 410 boundary
// rests on: after pruning, the surviving changelog rows are exactly those
// above some resource_version, with no holes. checkResumePointRetained reads
// MIN(resource_version) as the floor and tells any client at or above it that
// it can be served completely — which is a lie if a row above the floor was
// pruned.
//
// The setup crafts the inversion that makes this non-trivial: row rv=3 is
// inserted with an OLD created_at while rv=2 is inserted with a NEW one,
// which is what two overlapping transactions produce (resource_version is
// drawn before commit, created_at at insert). A created_at-only delete keeps
// rv=2 and drops rv=3.
func TestPruneLeavesRetainedSetAsSuffix(t *testing.T) {
	db, dsn := startEphemeralPostgres(t)
	pw := pgwatch.New(db, newTestCodec(t), dsn)
	t.Cleanup(pw.Stop)

	old := time.Now().Add(-10 * time.Minute)
	recent := time.Now()
	rows := []struct {
		rv        int64
		createdAt time.Time
	}{
		{1, old},
		{2, recent}, // inverted: newer timestamp, lower version
		{3, old},    // inverted: older timestamp, higher version
		{4, recent},
		{5, recent},
	}
	for _, r := range rows {
		if _, err := db.Exec(
			`INSERT INTO ipam_changelog (key, resource_version, event_type, data, created_at)
			 VALUES ($1, $2, 'ADDED', '{}'::bytea, $3)`,
			fmt.Sprintf("%s/obj-%d", watchKeyPrefix, r.rv), r.rv, r.createdAt,
		); err != nil {
			t.Fatalf("seed changelog rv=%d: %v", r.rv, err)
		}
	}

	deleted, err := pw.PruneChangelogForTest(time.Now().Add(-5 * time.Minute))
	if err != nil {
		t.Fatalf("prune: %v", err)
	}
	// Sample-count control: a prune that deleted nothing leaves a trivially
	// hole-free set and would pass the suffix assertion for the wrong reason.
	if deleted == 0 {
		t.Fatal("prune deleted 0 rows; the suffix assertion below would be vacuous")
	}

	var retained []int64
	res, err := db.Query(`SELECT resource_version FROM ipam_changelog ORDER BY resource_version`)
	if err != nil {
		t.Fatalf("read retained: %v", err)
	}
	defer func() { _ = res.Close() }()
	for res.Next() {
		var rv int64
		if err := res.Scan(&rv); err != nil {
			t.Fatalf("scan: %v", err)
		}
		retained = append(retained, rv)
	}
	if err := res.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	if len(retained) == 0 {
		t.Fatal("prune removed every row; nothing left to check the floor against")
	}

	// The retained set must be a contiguous suffix: everything from its
	// minimum upward is present.
	floor := retained[0]
	for i, rv := range retained {
		if rv != floor+int64(i) {
			t.Fatalf("retained set %v has a hole: MIN is %d but %d is missing, so a client "+
				"resuming at %d would be told it is safe and then silently miss an event",
				retained, floor, floor+int64(i), floor)
		}
	}
	t.Logf("pruned %d rows; retained %v", deleted, retained)
}

// TestOnlyOneReplicaCompactsAtATime is the #96 guard.
//
// Both apiserver replicas run cleanupLoop on their own ticker, and compaction
// is global rather than per-replica — the cutoff comes from the table, so both
// compute the same range and issue the same DELETE. Measured on the dev
// cluster: four processes in one deadlock report, every one of them on that
// statement, with no pool row or claim involved.
//
// Two IDENTICAL statements deadlocking reads as impossible, which is why the
// mechanism is worth stating: a bulk DELETE takes row locks in whatever order
// its plan yields, and `synchronize_seqscans` deliberately starts a concurrent
// sequential scan of the same table at a different position so the two can
// share work. The replicas therefore walk the same rows in different orders.
//
// The fix is that only one compacts per pass. This asserts the property that
// makes that true — a second caller finding the lock held returns `ran=false`
// rather than issuing the DELETE — and asserts it while the first is still
// INSIDE its compaction, which is the only window where it matters.
func TestOnlyOneReplicaCompactsAtATime(t *testing.T) {
	db, dsn := startEphemeralPostgres(t)

	// Two watchers over one database is exactly two replicas over one database.
	replicaA := pgwatch.New(db, newTestCodec(t), dsn)
	t.Cleanup(replicaA.Stop)
	replicaB := pgwatch.New(db, newTestCodec(t), dsn)
	t.Cleanup(replicaB.Stop)

	old := time.Now().Add(-10 * time.Minute)
	for i := 1; i <= 20; i++ {
		if _, err := db.Exec(
			`INSERT INTO ipam_changelog (key, resource_version, event_type, data, created_at)
			 VALUES ($1, $2, 'ADDED', '{}'::bytea, $3)`,
			fmt.Sprintf("%s/compact-%d", watchKeyPrefix, i), i, old,
		); err != nil {
			t.Fatalf("seed changelog %d: %v", i, err)
		}
	}

	// Hold the lock on a session of our own, standing in for the replica that
	// won this pass. Taken through a pinned connection for the same reason the
	// production path does: an advisory lock is session-scoped and database/sql
	// hands out whatever connection it likes.
	holder, err := db.Conn(context.Background())
	if err != nil {
		t.Fatalf("open holder connection: %v", err)
	}
	defer func() { _ = holder.Close() }()

	var held bool
	if err := holder.QueryRowContext(context.Background(),
		`SELECT pg_try_advisory_lock($1)`, pgwatch.ChangelogCompactionLockIDForTest).Scan(&held); err != nil {
		t.Fatalf("take the lock: %v", err)
	}
	if !held {
		t.Fatal("could not take the compaction lock on a fresh database; the test cannot " +
			"contend for something it does not hold")
	}

	// The other replica must decline rather than run the DELETE.
	rows, ran, err := replicaB.TryPruneChangelogForTest(time.Now().Add(-5 * time.Minute))
	if err != nil {
		t.Fatalf("second replica errored instead of skipping: %v", err)
	}
	if ran {
		t.Error("both replicas compacted in the same pass; that is the state that deadlocked " +
			"on the dev cluster, four processes on one DELETE")
	}
	if rows != 0 {
		t.Errorf("skipped pass reported %d rows deleted, want 0", rows)
	}

	// Positive control: nothing was actually removed, so "ran=false" is the
	// replica declining rather than a DELETE that matched nothing.
	var left int
	if err := db.QueryRow(`SELECT count(*) FROM ipam_changelog`).Scan(&left); err != nil {
		t.Fatalf("count changelog: %v", err)
	}
	if left != 20 {
		t.Errorf("changelog rows = %d, want 20 untouched while the lock was held", left)
	}

	// And once the holder lets go, compaction proceeds — otherwise this would
	// pass against a lock that is never released, which is the failure mode
	// that stops compaction cluster-wide.
	if _, err := holder.ExecContext(context.Background(),
		`SELECT pg_advisory_unlock($1)`, pgwatch.ChangelogCompactionLockIDForTest); err != nil {
		t.Fatalf("release the lock: %v", err)
	}
	deleted, ran, err := replicaA.TryPruneChangelogForTest(time.Now().Add(-5 * time.Minute))
	if err != nil {
		t.Fatalf("prune after release: %v", err)
	}
	if !ran {
		t.Fatal("compaction still declined after the lock was released; the lock leaked, " +
			"which stops compaction on every replica until a connection is recycled")
	}
	if deleted != 20 {
		t.Errorf("deleted = %d, want 20 once the lock was free", deleted)
	}
}
