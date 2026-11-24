package ipclaim

// The round-trip budget of an IPClaim create (milo-os/ipam#195).
//
// Every statement a create sends to Postgres costs a network round trip, and the
// ones sent while the pool row is locked serialise every other claim on that
// pool. This test drives AllocatingREST.Create against real Postgres with a
// tracer on the pool and holds each scenario to a ceiling, so a change that
// adds a round trip has to say so here.
//
// What is counted, per create:
//
//	statements     every statement, whether sent alone or queued in a batch
//	round trips    one per statement sent alone, one per batch
//	transactions   BEGINs, including one queued by Batch.BeginTx; savepoints are not
//	acquires       pool connection checkouts
//	lock RTs       round trips from the one that takes a pool row's FOR UPDATE to
//	               the COMMIT or ROLLBACK that releases it, inclusive; the longest
//	               such hold in the create
//
// The budget is measured on warm connections: each scenario runs once to warm
// the statement cache, then again to measure. The pool has one connection, so
// the warm-up warms the connection that is measured, and a create that needed
// two connections at once would hang here (and fail on the deadline) rather
// than pass. Cold-connection prepares are bounded separately.

import (
	"context"
	"encoding/json"
	"fmt"
	"math/big"
	"net/netip"
	"runtime"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/serializer"

	"go.miloapis.com/ipam/internal/allocator"
	"go.miloapis.com/ipam/internal/scope"
	pgstore "go.miloapis.com/ipam/internal/storage/postgres"
	"go.miloapis.com/ipam/internal/tenant"
	"go.miloapis.com/ipam/internal/testdb"
	"go.miloapis.com/ipam/pkg/apis/ipam"
	ipaminstall "go.miloapis.com/ipam/pkg/apis/ipam/install"
	ipamv1alpha1 "go.miloapis.com/ipam/pkg/apis/ipam/v1alpha1"
	"go.miloapis.com/ipam/pkg/ipamerrors"
)

// rtBudget is the ceiling for one scenario. A measured value above any field
// fails the test.
type rtBudget struct {
	Statements   int
	RoundTrips   int
	Transactions int
	Acquires     int
	LockRTs      int
}

// The budgets. Lower a figure when a change removes round trips; raising one
// needs a reason in the change that raises it.
var claimRoundTripBudgets = map[string]rtBudget{
	"A": {Statements: 62, RoundTrips: 62, Transactions: 6, Acquires: 7, LockRTs: 18},
	"B": {Statements: 36, RoundTrips: 36, Transactions: 6, Acquires: 7, LockRTs: 18},
	"C": {Statements: 50, RoundTrips: 50, Transactions: 6, Acquires: 7, LockRTs: 18},
	"D": {Statements: 32, RoundTrips: 32, Transactions: 2, Acquires: 3, LockRTs: 14},
	"E": {Statements: 28, RoundTrips: 28, Transactions: 4, Acquires: 5, LockRTs: 18},
	"F": {Statements: 29, RoundTrips: 29, Transactions: 4, Acquires: 5, LockRTs: 13},
	"G": {Statements: 36, RoundTrips: 36, Transactions: 6, Acquires: 7, LockRTs: 18},
	"H": {Statements: 18, RoundTrips: 18, Transactions: 2, Acquires: 3, LockRTs: 12},
	"I": {Statements: 27, RoundTrips: 27, Transactions: 7, Acquires: 8, LockRTs: 2},
	"J": {Statements: 37, RoundTrips: 37, Transactions: 6, Acquires: 7, LockRTs: 18},
	"K": {Statements: 29, RoundTrips: 29, Transactions: 4, Acquires: 5, LockRTs: 19},
	"L": {Statements: 43, RoundTrips: 43, Transactions: 4, Acquires: 5, LockRTs: 33},
	"M": {Statements: 11, RoundTrips: 11, Transactions: 3, Acquires: 4, LockRTs: 0},
}

// coldPrepareBudget bounds the extra round trips a steady-state create (B)
// spends preparing statements on a connection that has prepared none: one per
// new statement sent alone, one per batch holding any new statement.
const coldPrepareBudget = 17

// rtStats is what one create cost.
type rtStats struct {
	Statements, RoundTrips, Transactions, Acquires, Prepares int
	LockRTs, LockRTsTotal                                    int
	trace                                                    []string
}

// rtEvent is one round trip: a statement sent alone, or a batch.
type rtEvent struct {
	conn  *pgx.Conn
	sqls  []string
	batch bool
	where string
}

// rtRecorder is a pgx tracer that records round trips while on. It implements
// QueryTracer, BatchTracer and PrepareTracer, and pgxpool's AcquireTracer.
// pgx reports a batched query only through BatchTracer, never through
// QueryTracer, so without it a batch would be invisible.
type rtRecorder struct {
	mu       sync.Mutex
	on       bool
	events   []rtEvent
	acquires int
	prepares int

	// cached mirrors each connection's statement cache, so a batch that has to
	// prepare a statement first can be charged the round trip that costs.
	cached map[*pgx.Conn]map[string]bool
}

// remember notes that conn has sql prepared, reporting whether it already did.
func (r *rtRecorder) remember(conn *pgx.Conn, sql string) bool {
	if r.cached == nil {
		r.cached = map[*pgx.Conn]map[string]bool{}
	}
	if r.cached[conn] == nil {
		r.cached[conn] = map[string]bool{}
	}
	had := r.cached[conn][sql]
	r.cached[conn][sql] = true
	return had
}

// forget is DeallocateAll for the mirror.
func (r *rtRecorder) forget() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.cached = nil
}

func (r *rtRecorder) start() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.on, r.events, r.acquires, r.prepares = true, nil, 0, 0
}

func (r *rtRecorder) stop() rtStats {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.on = false
	return summarise(r.events, r.acquires, r.prepares)
}

func (r *rtRecorder) TraceQueryStart(ctx context.Context, conn *pgx.Conn, d pgx.TraceQueryStartData) context.Context {
	r.mu.Lock()
	defer r.mu.Unlock()
	// A statement with no arguments goes over the simple protocol and is never
	// prepared; one with arguments is prepared once and cached.
	if len(d.Args) > 0 {
		r.remember(conn, d.SQL)
	}
	if r.on {
		r.events = append(r.events, rtEvent{conn: conn, sqls: []string{d.SQL}, where: rtCaller()})
	}
	return ctx
}

func (r *rtRecorder) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

func (r *rtRecorder) TraceBatchStart(ctx context.Context, conn *pgx.Conn, d pgx.TraceBatchStartData) context.Context {
	r.mu.Lock()
	defer r.mu.Unlock()
	// pgx prepares every statement of a batch it has not cached in one
	// pipelined round trip ahead of the batch itself.
	fresh := false
	for _, q := range d.Batch.QueuedQueries {
		if !r.remember(conn, q.SQL) {
			fresh = true
		}
	}
	if r.on {
		ev := rtEvent{conn: conn, batch: true, where: rtCaller()}
		for _, q := range d.Batch.QueuedQueries {
			ev.sqls = append(ev.sqls, q.SQL)
		}
		r.events = append(r.events, ev)
		if fresh {
			r.prepares++
		}
	}
	return ctx
}

func (r *rtRecorder) TraceBatchQuery(context.Context, *pgx.Conn, pgx.TraceBatchQueryData) {}
func (r *rtRecorder) TraceBatchEnd(context.Context, *pgx.Conn, pgx.TraceBatchEndData)     {}

func (r *rtRecorder) TracePrepareStart(ctx context.Context, _ *pgx.Conn, _ pgx.TracePrepareStartData) context.Context {
	return ctx
}

// TracePrepareEnd counts a prepare that went to the server for a statement
// sent alone. A batch's prepares are counted in TraceBatchStart.
func (r *rtRecorder) TracePrepareEnd(_ context.Context, _ *pgx.Conn, d pgx.TracePrepareEndData) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.on && !d.AlreadyPrepared {
		r.prepares++
	}
}

func (r *rtRecorder) TraceAcquireStart(ctx context.Context, _ *pgxpool.Pool, _ pgxpool.TraceAcquireStartData) context.Context {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.on {
		r.acquires++
	}
	return ctx
}

func (r *rtRecorder) TraceAcquireEnd(context.Context, *pgxpool.Pool, pgxpool.TraceAcquireEndData) {}

// rtCaller names the first non-test frame in internal/ that sent the statement.
func rtCaller() string {
	pcs := make([]uintptr, 48)
	n := runtime.Callers(4, pcs)
	frames := runtime.CallersFrames(pcs[:n])
	for {
		f, more := frames.Next()
		if i := strings.Index(f.File, "/internal/"); i >= 0 && !strings.HasSuffix(f.File, "_test.go") {
			return fmt.Sprintf("%s:%d", f.File[i+1:], f.Line)
		}
		if !more {
			return ""
		}
	}
}

func sqlVerb(sql string) string {
	return strings.ToLower(strings.Join(strings.Fields(sql), " "))
}

func isBegin(sql string) bool { return strings.HasPrefix(sqlVerb(sql), "begin") }

func isTxEnd(sql string) bool {
	v := sqlVerb(sql)
	if strings.HasPrefix(v, "rollback to savepoint") {
		return false
	}
	return strings.HasPrefix(v, "commit") || strings.HasPrefix(v, "rollback")
}

// isPoolLock reports whether sql takes a row lock in ipam_objects: the pool
// lock every allocation, carve and reclaim serialises on.
func isPoolLock(sql string) bool {
	v := sqlVerb(sql)
	return strings.Contains(v, "from ipam_objects") && strings.HasSuffix(v, "for update")
}

func summarise(events []rtEvent, acquires, prepares int) rtStats {
	s := rtStats{RoundTrips: len(events), Acquires: acquires, Prepares: prepares}
	type txState struct {
		locking bool
		held    int
	}
	state := map[*pgx.Conn]*txState{}
	for i, ev := range events {
		st := state[ev.conn]
		if st == nil {
			st = &txState{}
			state[ev.conn] = st
		}
		ended := false
		for _, q := range ev.sqls {
			s.Statements++
			if isBegin(q) {
				s.Transactions++
			}
			if !st.locking && isPoolLock(q) {
				st.locking = true
			}
			if isTxEnd(q) {
				ended = true
			}
		}
		if st.locking {
			st.held++
		}
		if ended {
			if st.locking {
				s.LockRTsTotal += st.held
				s.LockRTs = max(s.LockRTs, st.held)
			}
			*st = txState{}
		}

		kind := "   "
		if ev.batch {
			kind = "[b]"
		}
		for j, q := range ev.sqls {
			sql := strings.Join(strings.Fields(q), " ")
			if len(sql) > 100 {
				sql = sql[:100]
			}
			label := fmt.Sprintf("%3d", i+1)
			if j > 0 {
				label = "   "
			}
			s.trace = append(s.trace, fmt.Sprintf("%s %s %-100s | %s", label, kind, sql, ev.where))
		}
	}
	return s
}

// budgetFixture is the tenant IPv6 plan plus the classes and pools the other
// scenarios need, behind a one-connection pool carrying the recorder.
type budgetFixture struct {
	rest *AllocatingREST
	db   *pgxpool.Pool
	rec  *rtRecorder
}

func newBudgetFixture(t *testing.T) *budgetFixture {
	t.Helper()
	rec := &rtRecorder{}
	db := testdb.Pool(t, func(c *pgxpool.Config) {
		c.ConnConfig.Tracer = rec
		c.MaxConns = 1
		// An idle connection is pinged on checkout after a second, which is a
		// round trip the tracer cannot see. Production connections rarely sit
		// idle that long under load, and a test pause must not change a count.
		c.ShouldPing = func(context.Context, pgxpool.ShouldPingParams) bool { return false }
	})
	ctx := context.Background()

	seed := func(kind, name string, obj any) {
		t.Helper()
		data, err := json.Marshal(obj)
		if err != nil {
			t.Fatalf("marshal %s %s: %v", kind, name, err)
		}
		resource := "ipclasses"
		if kind == "IPPool" {
			resource = "ippools"
		}
		if _, err := db.Exec(ctx, `INSERT INTO ipam_objects (key, kind, name, data) VALUES ($1,$2,$3,$4)`,
			tenant.Identity{Name: testProject}.ResourceKey(resource, name), kind, name, data); err != nil {
			t.Fatalf("seed %s %s: %v", kind, name, err)
		}
	}
	class := func(name string, spec ipamv1alpha1.IPClassSpec) {
		seed("IPClass", name, &ipamv1alpha1.IPClass{
			TypeMeta:   metav1.TypeMeta{APIVersion: "ipam.miloapis.com/v1alpha1", Kind: "IPClass"},
			ObjectMeta: metav1.ObjectMeta{Name: name},
			Spec:       spec,
		})
	}
	rootPool := func(name, cidr string, family ipamv1alpha1.IPFamily, reservations *ipamv1alpha1.ReservationSpec, classNames ...string) {
		seed("IPPool", name, &ipamv1alpha1.IPPool{
			TypeMeta:   metav1.TypeMeta{APIVersion: "ipam.miloapis.com/v1alpha1", Kind: "IPPool"},
			ObjectMeta: metav1.ObjectMeta{Name: name},
			Spec: ipamv1alpha1.IPPoolSpec{
				CIDR: cidr, IPFamily: family, ClassNames: classNames, Reservations: reservations,
			},
			Status: ipamv1alpha1.IPPoolStatus{Phase: ipamv1alpha1.PoolReady, AllocatedCIDR: cidr, IPFamily: family},
		})
	}
	day := &metav1.Duration{Duration: 24 * time.Hour}

	// The three-level chain, as in newTenantREST; the leaf carries a lease so
	// a Retain claim of it is accepted (scenarios G and H).
	class("tenant-vpc", ipamv1alpha1.IPClassSpec{IPFamily: ipamv1alpha1.IPv6, DefaultPrefixLength: 48,
		PoolPer: []string{"network", scope.ReservedRoleProject}})
	class("tenant-subnet", ipamv1alpha1.IPClassSpec{IPFamily: ipamv1alpha1.IPv6, ParentClassName: "tenant-vpc",
		DefaultPrefixLength: 64, PoolPer: []string{"network", "location", scope.ReservedRoleProject}})
	class("tenant-endpoint", ipamv1alpha1.IPClassSpec{IPFamily: ipamv1alpha1.IPv6, ParentClassName: "tenant-subnet",
		DefaultPrefixLength: 96, RetentionLease: day})
	// A leaf with no lease: a Retain override is judged by the pool's cap (I).
	class("ep-nolease", ipamv1alpha1.IPClassSpec{IPFamily: ipamv1alpha1.IPv6, ParentClassName: "tenant-subnet",
		DefaultPrefixLength: 96})
	// A reference at the leaf: two dependent reads before admission (J).
	class("ep-ref", ipamv1alpha1.IPClassSpec{Source: &ipamv1alpha1.ClassSourceRef{Project: testProject, Name: "tenant-endpoint"}})
	rootPool("tenant-root", "fd20:1000::/36", ipamv1alpha1.IPv6, nil, "tenant-vpc")

	// A flat class (E).
	class("flat", ipamv1alpha1.IPClassSpec{IPFamily: ipamv1alpha1.IPv6, DefaultPrefixLength: 64})
	rootPool("flat-root", "fd30::/32", ipamv1alpha1.IPv6, nil, "flat")

	// A flat class whose pool declares reservations (K).
	class("flat-resv", ipamv1alpha1.IPClassSpec{IPFamily: ipamv1alpha1.IPv4, DefaultPrefixLength: 28})
	rootPool("resv-root", "10.70.0.0/16", ipamv1alpha1.IPv4,
		&ipamv1alpha1.ReservationSpec{UnitPrefixLength: 28, Leading: 1, Trailing: 1}, "flat-resv")

	// A fragmented pool (L): one /32 held at the start of each of the first
	// 2048 /24s, so no /24 is free below them, the first free address stays
	// at the bottom, and every /24 search pages through all of them.
	class("frag", ipamv1alpha1.IPClassSpec{IPFamily: ipamv1alpha1.IPv4, DefaultPrefixLength: 24})
	rootPool("frag-root", "10.80.0.0/12", ipamv1alpha1.IPv4, nil, "frag")
	_, digest, err := scope.ProjectAddressSpaceDigest(testProject, nil, nil, "uniqueWithin")
	if err != nil {
		t.Fatalf("address space digest: %v", err)
	}
	const fragmentRows = 2048
	base := netip.MustParseAddr("10.80.0.0").As4()
	cidrs := make([]string, 0, fragmentRows)
	keys := make([]string, 0, fragmentRows)
	for i := range fragmentRows {
		a := base
		a[1] += byte(i >> 8)
		a[2] = byte(i)
		cidrs = append(cidrs, netip.AddrFrom4(a).String()+"/32")
		keys = append(keys, fmt.Sprintf("fragment-%d", i))
	}
	if _, err := db.Exec(ctx,
		`INSERT INTO ipam_cidr_allocations
		    (pool_key, allocated_cidr, claim_key, allocation_key, ip_family, purpose, class_name, scope_digest, owner_project)
		 SELECT $1, c::cidr, k, k, 'IPv4', 'Claim', 'frag', $4, $5
		   FROM unnest($2::text[], $3::text[]) AS f(c, k)`,
		tenant.Identity{Name: testProject}.ResourceKey("ippools", "frag-root"), cidrs, keys, digest, testProject); err != nil {
		t.Fatalf("seed fragments: %v", err)
	}

	scheme := k8sruntime.NewScheme()
	ipaminstall.Install(scheme)
	codec := serializer.NewCodecFactory(scheme).LegacyCodec(ipamv1alpha1.SchemeGroupVersion)
	getter, err := pgstore.NewRESTOptionsGetter(db.Config().ConnString())
	if err != nil {
		t.Fatalf("rest options getter: %v", err)
	}
	getter.SetCodec(codec)
	rest, _, err := NewAllocatingStorage(scheme, getter, allocator.NewPostgresPrefixAllocator(), db, codec, nil, nil, nil)
	if err != nil {
		t.Fatalf("claim storage: %v", err)
	}
	t.Cleanup(rest.Destroy)
	return &budgetFixture{rest: rest, db: db, rec: rec}
}

// measure creates claim with the recorder on. want is the IPAM reason the
// create must be refused with, or "" for a create that must succeed.
func (f *budgetFixture) measure(t *testing.T, claim *ipam.IPClaim, opts *metav1.CreateOptions, want ipamerrors.Reason) (rtStats, *ipam.IPClaim) {
	t.Helper()
	if opts == nil {
		opts = &metav1.CreateOptions{}
	}
	ctx, cancel := context.WithTimeout(claimCtx(testProject), 30*time.Second)
	defer cancel()
	f.rec.start()
	obj, err := f.rest.Create(ctx, claim, nil, opts)
	stats := f.rec.stop()
	switch {
	case want == "" && err != nil:
		t.Fatalf("create %s: %v\n%s", claim.Name, err, strings.Join(stats.trace, "\n"))
	case want != "" && ipamerrors.ReasonFor(err) != want:
		t.Fatalf("create %s: error = %v (reason %q), want reason %q", claim.Name, err, ipamerrors.ReasonFor(err), want)
	}
	bound, _ := obj.(*ipam.IPClaim)
	return stats, bound
}

func (f *budgetFixture) subnetPoolCIDR(t *testing.T, poolName string) netip.Prefix {
	t.Helper()
	var data []byte
	if err := f.db.QueryRow(context.Background(), `SELECT data FROM ipam_objects WHERE key = $1`,
		tenant.Identity{Name: testProject}.ResourceKey("ippools", poolName)).Scan(&data); err != nil {
		t.Fatalf("read pool %s: %v", poolName, err)
	}
	var pool ipamv1alpha1.IPPool
	if err := json.Unmarshal(data, &pool); err != nil {
		t.Fatalf("decode pool %s: %v", poolName, err)
	}
	return netip.MustParsePrefix(pool.Status.AllocatedCIDR)
}

// nthBlock is the n-th /bits block of p.
func nthBlock(p netip.Prefix, bits int, n uint64) string {
	a := p.Addr().As16()
	v := new(big.Int).SetBytes(a[:])
	v.Add(v, new(big.Int).Lsh(new(big.Int).SetUint64(n), uint(128-bits)))
	var out [16]byte
	v.FillBytes(out[:])
	return netip.PrefixFrom(netip.AddrFrom16(out), bits).String()
}

func withClass(c *ipam.IPClaim, class string) *ipam.IPClaim {
	c.Spec.ClassName = class
	return c
}

func flatClaim(name, class string) *ipam.IPClaim {
	return &ipam.IPClaim{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Spec:       ipam.IPClaimSpec{ClassName: class},
	}
}

func retain(c *ipam.IPClaim) *ipam.IPClaim {
	c.Spec.ReclaimPolicy = ipam.ReclaimRetain
	return c
}

// The scenarios, in the order they run: later ones depend on the pools earlier
// ones provisioned, and M withdraws the root's offer, so it runs last.
//
//	A  3-level chain, first claim: provisions the vpc and subnet pools
//	B  3-level chain, every pool exists (the steady state)
//	C  3-level chain, a new location: provisions the subnet pool only
//	D  B as a server-side dry-run
//	E  a flat class
//	F  a claim naming its address under the chain, every pool existing
//	G  B with reclaimPolicy Retain under a class that sets a lease
//	H  a claim recreated under the name of a G claim, reclaiming its address
//	I  a Retain override under a class with no lease, refused under the pool lock
//	J  B through a reference class at the leaf
//	K  a flat class whose pool declares reservations
//	L  a flat class whose pool is fragmented, so the search pages
//	M  the chain intact but the root no longer offering it: refused
//	   NoOfferingPool, and the offer read stays on the path
func TestClaimCreateRoundTripBudget(t *testing.T) {
	f := newBudgetFixture(t)
	results := map[string]rtStats{}

	// run warms the connection with one create and measures a second of the
	// same shape.
	run := func(id string, warm, measured *ipam.IPClaim, opts *metav1.CreateOptions, want ipamerrors.Reason) *ipam.IPClaim {
		t.Helper()
		f.measure(t, warm, opts, want)
		stats, bound := f.measure(t, measured, opts, want)
		if stats.Prepares != 0 {
			t.Errorf("%s: %d statements were prepared on a warm connection; the warm-up must take the same path", id, stats.Prepares)
		}
		results[id] = stats
		return bound
	}

	run("A", endpointClaim("a-0", "net-a0", "ams"), endpointClaim("a-1", "net-a1", "ams"), nil, "")
	b := run("B", endpointClaim("b-0", "net-a1", "ams"), endpointClaim("b-1", "net-a1", "ams"), nil, "")
	run("C", endpointClaim("c-0", "net-a1", "lhr"), endpointClaim("c-1", "net-a1", "fra"), nil, "")
	run("D", endpointClaim("d-0", "net-a1", "ams"), endpointClaim("d-1", "net-a1", "ams"),
		&metav1.CreateOptions{DryRun: []string{metav1.DryRunAll}}, "")
	run("E", flatClaim("e-0", "flat"), flatClaim("e-1", "flat"), nil, "")

	subnet := f.subnetPoolCIDR(t, b.Status.PoolRef.Name)
	named := func(name string, n uint64) *ipam.IPClaim {
		c := endpointClaim(name, "net-a1", "ams")
		c.Spec.Address = nthBlock(subnet, 96, n)
		return c
	}
	run("F", named("f-0", 1000), named("f-1", 1001), nil, "")

	run("G", retain(endpointClaim("g-0", "net-a1", "ams")), retain(endpointClaim("g-1", "net-a1", "ams")), nil, "")
	for _, name := range []string{"g-0", "g-1"} {
		if _, _, err := f.rest.Delete(claimCtx(testProject), name, nil, &metav1.DeleteOptions{}); err != nil {
			t.Fatalf("delete %s: %v", name, err)
		}
	}
	run("H", retain(endpointClaim("g-0", "net-a1", "ams")), retain(endpointClaim("g-1", "net-a1", "ams")), nil, "")

	run("I", retain(withClass(endpointClaim("i-0", "net-a1", "ams"), "ep-nolease")),
		retain(withClass(endpointClaim("i-1", "net-a1", "ams"), "ep-nolease")), nil, ipamerrors.ReasonRetentionLeaseRequired)
	run("J", withClass(endpointClaim("j-0", "net-a1", "ams"), "ep-ref"),
		withClass(endpointClaim("j-1", "net-a1", "ams"), "ep-ref"), nil, "")
	run("K", flatClaim("k-0", "flat-resv"), flatClaim("k-1", "flat-resv"), nil, "")
	run("L", flatClaim("l-0", "frag"), flatClaim("l-1", "frag"), nil, "")

	// M: the chain is intact, but the root pool no longer offers its class.
	withdrawOffer(t, f.db, "tenant-root")
	run("M", endpointClaim("m-0", "net-a1", "ams"), endpointClaim("m-1", "net-a1", "ams"), nil, ipamerrors.ReasonNoOfferingPool)
	if !traceContains(results["M"], "ipam_pool_class_offer") {
		t.Errorf("M: the claim was refused without reading the root's offers; discovery must stay on the path\n%s",
			strings.Join(results["M"].trace, "\n"))
	}

	// Cold: B again on a connection that has prepared nothing.
	deallocateAll(t, f.db)
	f.rec.forget()
	restoreOffer(t, f.db, "tenant-root")
	f.rec.start()
	if _, err := f.rest.Create(claimCtx(testProject), endpointClaim("cold-0", "net-a1", "ams"), nil, &metav1.CreateOptions{}); err != nil {
		t.Fatalf("cold create: %v", err)
	}
	cold := f.rec.stop()

	report(t, results, cold)

	ids := make([]string, 0, len(results))
	for id := range results {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		got, budget := results[id], claimRoundTripBudgets[id]
		over := func(name string, got, ceiling int) {
			if got > ceiling {
				t.Errorf("%s: %d %s, over the budget of %d\n%s", id, got, name, ceiling, strings.Join(results[id].trace, "\n"))
			}
		}
		over("statements", got.Statements, budget.Statements)
		over("round trips", got.RoundTrips, budget.RoundTrips)
		over("transactions", got.Transactions, budget.Transactions)
		over("acquires", got.Acquires, budget.Acquires)
		over("round trips under a pool lock", got.LockRTs, budget.LockRTs)
	}
	if cold.Prepares > coldPrepareBudget {
		t.Errorf("cold B: %d prepare round trips, over the budget of %d", cold.Prepares, coldPrepareBudget)
	}
}

func report(t *testing.T, results map[string]rtStats, cold rtStats) {
	t.Helper()
	ids := make([]string, 0, len(results))
	for id := range results {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	var b strings.Builder
	fmt.Fprintf(&b, "\n| scenario | statements | round trips | transactions | acquires | lock RTs (longest) | lock RTs (all) |\n")
	fmt.Fprintf(&b, "|---|---|---|---|---|---|---|\n")
	for _, id := range ids {
		s := results[id]
		fmt.Fprintf(&b, "| %s | %d | %d | %d | %d | %d | %d |\n", id, s.Statements, s.RoundTrips, s.Transactions, s.Acquires, s.LockRTs, s.LockRTsTotal)
	}
	fmt.Fprintf(&b, "\ncold B: %d prepare round trips on top of %d round trips\n", cold.Prepares, cold.RoundTrips)
	for _, id := range ids {
		fmt.Fprintf(&b, "\n=== %s\n%s\n", id, strings.Join(results[id].trace, "\n"))
	}
	t.Log(b.String())
}

func traceContains(s rtStats, needle string) bool {
	for _, line := range s.trace {
		if strings.Contains(line, needle) {
			return true
		}
	}
	return false
}

// withdrawOffer removes every class from a root pool's spec.classNames, the
// way an operator withdrawing a pool from service would. The offer trigger
// drops the pool's offer rows with it.
func withdrawOffer(t *testing.T, db *pgxpool.Pool, pool string) {
	t.Helper()
	setClassNames(t, db, pool, []string{"withdrawn"})
}

func restoreOffer(t *testing.T, db *pgxpool.Pool, pool string) {
	t.Helper()
	setClassNames(t, db, pool, []string{"tenant-vpc"})
}

func setClassNames(t *testing.T, db *pgxpool.Pool, pool string, names []string) {
	t.Helper()
	ctx := context.Background()
	key := tenant.Identity{Name: testProject}.ResourceKey("ippools", pool)
	var data []byte
	if err := db.QueryRow(ctx, `SELECT data FROM ipam_objects WHERE key = $1`, key).Scan(&data); err != nil {
		t.Fatalf("read pool %s: %v", pool, err)
	}
	var p ipamv1alpha1.IPPool
	if err := json.Unmarshal(data, &p); err != nil {
		t.Fatalf("decode pool %s: %v", pool, err)
	}
	p.Spec.ClassNames = names
	if data, err := json.Marshal(&p); err != nil {
		t.Fatalf("encode pool %s: %v", pool, err)
	} else if _, err := db.Exec(ctx, `UPDATE ipam_objects SET data = $1 WHERE key = $2`, data, key); err != nil {
		t.Fatalf("update pool %s: %v", pool, err)
	}
}

// deallocateAll forgets every statement the pool's one connection prepared.
func deallocateAll(t *testing.T, db *pgxpool.Pool) {
	t.Helper()
	c, err := db.Acquire(context.Background())
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	defer c.Release()
	if err := c.Conn().DeallocateAll(context.Background()); err != nil {
		t.Fatalf("deallocate all: %v", err)
	}
}
