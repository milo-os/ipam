package ipclaim

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apiserver/pkg/authentication/user"
	genericapirequest "k8s.io/apiserver/pkg/endpoints/request"

	"go.miloapis.com/ipam/internal/allocator"
	"go.miloapis.com/ipam/internal/registry/ipam/registryerrors"
	"go.miloapis.com/ipam/internal/scope"
	"go.miloapis.com/ipam/internal/tenant"
	"go.miloapis.com/ipam/internal/tracing"
	"go.miloapis.com/ipam/pkg/apis/ipam"
	ipaminstall "go.miloapis.com/ipam/pkg/apis/ipam/install"
	"go.miloapis.com/ipam/pkg/apis/ipam/v1alpha1"
)

// --- fakes -----------------------------------------------------------------

// fakeTx records whether the transaction was committed or rolled back. Only
// Commit/Rollback are exercised; the other pgx.Tx methods are left nil and would
// panic if the code under test unexpectedly touched the database — which is the
// assertion that the Resolver seam is really carrying every read.
type fakeTx struct {
	pgx.Tx
	commits   int
	rollbacks int
}

func (f *fakeTx) Commit(context.Context) error   { f.commits++; return nil }
func (f *fakeTx) Rollback(context.Context) error { f.rollbacks++; return nil }

// QueryRow is the one read the Create/Delete path still makes directly: the
// release handler re-reads a retained allocation's object so it can clear the
// claim reference on it. The fake reports it as absent, which the handler is
// required to tolerate — a retained row whose object has already gone is not
// this transaction's problem to fix.
func (f *fakeTx) QueryRow(context.Context, string, ...any) pgx.Row { return noRow{} }

type noRow struct{}

func (noRow) Scan(...any) error { return pgx.ErrNoRows }

type fakeBeginner struct{ tx *fakeTx }

func (b *fakeBeginner) Begin(context.Context) (pgx.Tx, error) { return b.tx, nil }

// fakeResolver answers the three questions the Create path asks, and records
// what it was asked.
type fakeResolver struct {
	class   *v1alpha1.IPClass
	poolKey string
	missing []allocator.CascadeLevel

	classErr error
	poolErr  error

	resolveN     int
	gotProject   string
	gotScope     map[string]ipam.ScopeRef
	gotClassName string
}

func (f *fakeResolver) ResolveClass(_ context.Context, className string, _ v1alpha1.IPFamily) (*v1alpha1.IPClass, error) {
	f.gotClassName = className
	if f.classErr != nil {
		return nil, f.classErr
	}
	return f.class, nil
}

func (f *fakeResolver) ResolvePool(_ context.Context, _ *v1alpha1.IPClass, claimScope map[string]ipam.ScopeRef, project string) (string, error) {
	f.resolveN++
	f.gotScope = claimScope
	f.gotProject = project
	return f.poolKey, f.poolErr
}

func (f *fakeResolver) ResolveExistingPool(_ context.Context, _ *v1alpha1.IPClass, claimScope map[string]ipam.ScopeRef, project string) (string, []allocator.CascadeLevel, error) {
	f.resolveN++
	f.gotScope = claimScope
	f.gotProject = project
	if f.poolErr != nil {
		return "", nil, f.poolErr
	}
	if len(f.missing) > 0 {
		return "", f.missing, nil
	}
	return f.poolKey, nil, nil
}

// fakeAllocator computes a CIDR without any real persistence and counts the
// mutating calls so tests can assert nothing was written under dry-run.
type fakeAllocator struct {
	cidr      string
	allocErr  error
	outcomes  []allocator.ReleaseOutcome
	allocateN int
	insertN   int
	releaseN  int
	deleteN   int
	updateN   int
	nextRV    int64

	reclaimCIDR   string
	reclaimErr    error
	reclaimReq    *allocator.ReclaimRequest
	gotRequest    allocator.AllocateRequest
	gotInsertKeys []string
	gotDeleteKeys []string
	gotUpdateKeys []string
}

func (a *fakeAllocator) AllocatePrefix(_ context.Context, _ pgx.Tx, req allocator.AllocateRequest) (string, error) {
	a.allocateN++
	a.gotRequest = req
	if a.allocErr != nil {
		return "", a.allocErr
	}
	return a.cidr, nil
}

// CarveChildPool exists only to satisfy the interface. The claim registry has
// no business carving a child pool, so this errors rather than returning a
// plausible CIDR — a fake that answers a call the code under test should never
// make turns a wrong call site into a passing test.
func (a *fakeAllocator) CarveChildPool(_ context.Context, _ pgx.Tx, parentPoolKey string, _ int, _ allocator.PoolCarveRecord) (string, error) {
	return "", fmt.Errorf("claim registry called CarveChildPool against %q; a claim is an "+
		"allocation within an address space, not a sub-pool leaving the parent", parentPoolKey)
}

func (a *fakeAllocator) InsertObject(_ context.Context, _ pgx.Tx, key, _, _, _ string, _ []byte) (int64, error) {
	a.insertN++
	a.gotInsertKeys = append(a.gotInsertKeys, key)
	a.nextRV++
	return a.nextRV, nil
}

func (a *fakeAllocator) Release(_ context.Context, _ pgx.Tx, _ string) ([]allocator.ReleaseOutcome, error) {
	a.releaseN++
	return a.outcomes, nil
}

func (a *fakeAllocator) ForceRelease(_ context.Context, _ pgx.Tx, _ string) (bool, error) {
	return true, nil
}

func (a *fakeAllocator) ReclaimRetained(_ context.Context, _ pgx.Tx, req allocator.ReclaimRequest) (string, bool, error) {
	a.reclaimReq = &req
	return a.reclaimCIDR, a.reclaimCIDR != "", a.reclaimErr
}

func (a *fakeAllocator) DeleteObject(_ context.Context, _ pgx.Tx, key string) (int64, error) {
	a.deleteN++
	a.gotDeleteKeys = append(a.gotDeleteKeys, key)
	return 0, nil
}

func (a *fakeAllocator) UpdateObject(_ context.Context, _ pgx.Tx, key string, _ []byte) (int64, error) {
	a.updateN++
	a.gotUpdateKeys = append(a.gotUpdateKeys, key)
	a.nextRV++
	return a.nextRV, nil
}

// fakeCodec satisfies runtime.Codec; only Encode is reached in these tests.
type fakeCodec struct{}

func (fakeCodec) Encode(_ runtime.Object, w io.Writer) error {
	_, err := w.Write([]byte("x"))
	return err
}
func (fakeCodec) Identifier() runtime.Identifier { return runtime.Identifier("fake") }
func (fakeCodec) Decode(_ []byte, _ *schema.GroupVersionKind, _ runtime.Object) (runtime.Object, *schema.GroupVersionKind, error) {
	return nil, nil, io.EOF
}

// --- fixtures --------------------------------------------------------------

// endpointClass is the doc's tenant-endpoint-ipv4: no parent, unique within a
// network, fixed /32.
func endpointClass() *v1alpha1.IPClass {
	return &v1alpha1.IPClass{
		ObjectMeta: metav1.ObjectMeta{Name: "tenant-endpoint-ipv4"},
		Spec: v1alpha1.IPClassSpec{
			IPFamily:             v1alpha1.IPv4,
			UniqueWithin:         []string{"network"},
			AllowedPrefixLengths: &v1alpha1.PrefixLengthRange{Min: 32, Max: 32},
			ReclaimPolicy:        v1alpha1.ReclaimDelete,
			// Consumable by tenants, which is what a class a workload names has
			// to be. The authorization tests below override this; every other
			// test wants the ordinary case.
			Visibility: v1alpha1.VisibilityShared,
		},
	}
}

func newClaim() *ipam.IPClaim {
	return &ipam.IPClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "claim-1", Namespace: "default"},
		Spec: ipam.IPClaimSpec{
			ClassName: "tenant-endpoint-ipv4",
			Scope: map[string]ipam.ScopeRef{
				"network":  {APIGroup: "networking.datumapis.com", Kind: "Network", Name: "default"},
				"location": {APIGroup: "networking.datumapis.com", Kind: "Location", Name: "us-central-1"},
			},
		},
	}
}

func newTestREST() (*AllocatingREST, *fakeAllocator, *fakeResolver, *fakeTx) {
	scheme := runtime.NewScheme()
	ipaminstall.Install(scheme)
	fa := &fakeAllocator{cidr: "10.128.0.2/32"}
	fr := &fakeResolver{class: endpointClass(), poolKey: "/ipam.miloapis.com/ippools/tenant-v4-us-central-1"}
	ftx := &fakeTx{}
	r := &AllocatingREST{
		allocator:    fa,
		db:           &fakeBeginner{tx: ftx},
		resolver:     fr,
		classChecker: &staticClassChecker{allow: true},
		strategy:     NewStrategy(scheme),
		codec:        fakeCodec{},
	}
	return r, fa, fr, ftx
}

// projectContext produces the request context a project-scoped call arrives
// with: Milo's front gate forwards the parent identity as authentication extras.
// testPlatformProject is the project this package configures as the platform's
// own — the equivalent of --platform-project on a running server.
const testPlatformProject = "milo-platform"

func projectContext(project string) context.Context {
	ctx := genericapirequest.WithNamespace(context.Background(), "default")
	// Every request context carries the server's configured platform project,
	// exactly as cmd/ipam's platformProjectFilter puts it there. Without it no
	// caller reads as the platform, which is the correct fail-closed default and
	// would make the bypass tests below vacuous.
	ctx = tenant.WithPlatformProject(ctx, testPlatformProject)
	return genericapirequest.WithUser(ctx, &user.DefaultInfo{
		Name: "system:serviceaccount:milo:network",
		Extra: map[string][]string{
			tenant.ExtraParentAPIGroup: {"resourcemanager.miloapis.com"},
			tenant.ExtraParentType:     {"Project"},
			tenant.ExtraParentName:     {project},
		},
	})
}

// platformContext is a caller acting as the configured platform project. The
// platform authenticates as a project like everything else — there is no
// unprefixed keyspace and no identity-free caller with privileges.
func platformContext() context.Context { return projectContext(testPlatformProject) }

// untenantedContext is a caller carrying no parent extras at all: an
// unimpersonated kubeconfig. It used to be indistinguishable from the platform,
// and the tests below exist to keep it distinguishable.
func untenantedContext() context.Context {
	ctx := genericapirequest.WithNamespace(context.Background(), "default")
	ctx = tenant.WithPlatformProject(ctx, testPlatformProject)
	return genericapirequest.WithUser(ctx, &user.DefaultInfo{Name: "someone"})
}

// --- tests -----------------------------------------------------------------

func TestCreateDryRunPersistsNothing(t *testing.T) {
	// Rollbacks are not asserted exactly. A normal create rolls back the reclaim
	// probe (which finds nothing) and the class-resolution read before
	// committing the allocation; a dry-run rolls back the allocation too and
	// skips the probe entirely. The property worth pinning is what was
	// *persisted*, so these count inserts and commits.
	cases := []struct {
		name        string
		dryRun      []string
		wantInserts int
		wantCommits int
	}{
		{"server dry-run persists nothing", []string{metav1.DryRunAll}, 0, 0},
		{"normal create persists and commits", nil, 2, 1},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r, fa, _, ftx := newTestREST()

			obj, err := r.Create(projectContext("acme"), newClaim(), nil, &metav1.CreateOptions{DryRun: tc.dryRun})
			if err != nil {
				t.Fatalf("Create returned error: %v", err)
			}
			claim, ok := obj.(*ipam.IPClaim)
			if !ok {
				t.Fatalf("expected *ipam.IPClaim, got %T", obj)
			}

			// Both paths must return the identical bound status: a dry-run that
			// reported a different shape from the real thing would not be
			// answering the question it was asked.
			if claim.Status.Phase != ipam.ClaimBound {
				t.Errorf("phase = %q, want Bound", claim.Status.Phase)
			}
			if claim.Status.AllocatedCIDR != "10.128.0.2/32" {
				t.Errorf("allocatedCIDR = %q", claim.Status.AllocatedCIDR)
			}
			if claim.Status.Address != "10.128.0.2" {
				t.Errorf("address = %q, want the bare form for a /32", claim.Status.Address)
			}
			if claim.Status.PoolRef == nil || claim.Status.PoolRef.Name != "tenant-v4-us-central-1" {
				t.Errorf("poolRef = %+v", claim.Status.PoolRef)
			}
			if fa.allocateN != 1 {
				t.Errorf("AllocatePrefix calls = %d, want 1 — the CIDR must be the real next block", fa.allocateN)
			}
			if fa.insertN != tc.wantInserts {
				t.Errorf("InsertObject calls = %d, want %d", fa.insertN, tc.wantInserts)
			}
			if ftx.commits != tc.wantCommits {
				t.Errorf("tx commits = %d, want %d", ftx.commits, tc.wantCommits)
			}
			if tc.wantCommits == 0 && ftx.rollbacks == 0 {
				t.Error("a dry-run must roll the allocation transaction back")
			}
		})
	}
}

// A dry-run against a scope whose pools do not exist must not build them. Pools
// are never renumbered once created, so provisioning one to answer a
// hypothetical would leave the hypothetical behind permanently.
func TestCreateDryRunDoesNotProvision(t *testing.T) {
	r, fa, fr, _ := newTestREST()
	fr.missing = []allocator.CascadeLevel{
		{PoolName: "network-default-abc12345", Class: &v1alpha1.IPClass{ObjectMeta: metav1.ObjectMeta{Name: "tenant-network-ipv6"}}},
	}

	obj, err := r.Create(projectContext("acme"), newClaim(), nil, &metav1.CreateOptions{DryRun: []string{metav1.DryRunAll}})
	if err != nil {
		t.Fatalf("Create returned error: %v", err)
	}
	claim := obj.(*ipam.IPClaim)

	if claim.Status.Phase != ipam.ClaimPending {
		t.Errorf("phase = %q, want Pending", claim.Status.Phase)
	}
	if claim.Status.AllocatedCIDR != "" {
		t.Errorf("a dry-run that could not resolve a pool must not report an address, got %q", claim.Status.AllocatedCIDR)
	}
	if fa.allocateN != 0 {
		t.Errorf("AllocatePrefix calls = %d, want 0", fa.allocateN)
	}
	if len(claim.Status.Conditions) == 0 || !strings.Contains(claim.Status.Conditions[0].Message, "network-default-abc12345") {
		t.Errorf("the condition must name the pools that would be built, got %+v", claim.Status.Conditions)
	}
}

// The tenant prefix must reach every key the allocator writes, or a project's
// claim would be stored where another project's read would find it.
func TestCreateAppliesTenantPrefix(t *testing.T) {
	r, fa, fr, _ := newTestREST()

	if _, err := r.Create(projectContext("acme"), newClaim(), nil, &metav1.CreateOptions{}); err != nil {
		t.Fatalf("Create returned error: %v", err)
	}

	if fr.gotProject != "acme" {
		t.Errorf("resolver project = %q, want acme", fr.gotProject)
	}
	if fa.gotRequest.OwnerProject != "acme" {
		t.Errorf("allocate ownerProject = %q, want acme", fa.gotRequest.OwnerProject)
	}
	for _, key := range fa.gotInsertKeys {
		if !strings.HasPrefix(key, "project/acme/") {
			t.Errorf("insert key %q is missing the tenant prefix", key)
		}
	}
	if !strings.HasPrefix(fa.gotRequest.ClaimKey, "project/acme/") {
		t.Errorf("claim key %q is missing the tenant prefix", fa.gotRequest.ClaimKey)
	}
	if !strings.HasPrefix(fa.gotRequest.AllocationKey, "project/acme/") {
		t.Errorf("allocation key %q is missing the tenant prefix", fa.gotRequest.AllocationKey)
	}
}

// The digest the allocation is written under must be the projection onto the
// class's uniqueWithin — not the claim's whole scope. The claim here carries a
// location the class does not name, and including it would put every location
// in its own address space, defeating the shared per-location range.
func TestCreateScopesUniquenessToTheClass(t *testing.T) {
	r, fa, _, _ := newTestREST()
	claim := newClaim()

	if _, err := r.Create(projectContext("acme"), claim, nil, &metav1.CreateOptions{}); err != nil {
		t.Fatalf("Create returned error: %v", err)
	}

	want := scope.AddressSpaceDigest("acme", map[string]ipam.ScopeRef{
		"network": claim.Spec.Scope["network"],
	})
	if fa.gotRequest.ScopeDigest != want {
		t.Errorf("scope digest = %q, want the projection onto uniqueWithin (%q)", fa.gotRequest.ScopeDigest, want)
	}
}

// The digest must also carry the caller's project. Two projects that each have
// a network named `default` are two address spaces, and the digest is the only
// thing that says so: the allocation search and the overlap exclusion
// constraint both key on (pool_key, scope_digest) and neither mentions an
// owner, so a tenant-blind digest makes two projects drawing from one shared
// platform pool unable to hold the same address — the exact case uniqueWithin
// exists to permit.
func TestCreateScopesUniquenessToTheTenant(t *testing.T) {
	digestFor := func(project string) string {
		r, fa, _, _ := newTestREST()
		claim := newClaim()
		if _, err := r.Create(projectContext(project), claim, nil, &metav1.CreateOptions{}); err != nil {
			t.Fatalf("Create as %q returned error: %v", project, err)
		}
		return fa.gotRequest.ScopeDigest
	}

	alpha, beta := digestFor("acme"), digestFor("globex")
	if alpha == beta {
		t.Errorf("two projects claiming the same network name share address space %q", alpha)
	}
}

// A claim short a role its class requires is a bad request naming the role. The
// alternative — comparing against a wider space — would look correct while
// refusing addresses the narrow comparison was meant to allow, and would reach
// an operator as unexplained exhaustion.
func TestCreateRejectsMissingScopeRole(t *testing.T) {
	r, fa, _, _ := newTestREST()
	claim := newClaim()
	delete(claim.Spec.Scope, "network")

	_, err := r.Create(projectContext("acme"), claim, nil, &metav1.CreateOptions{})
	if err == nil {
		t.Fatal("expected a bad request, got nil")
	}
	if !strings.Contains(err.Error(), "network") {
		t.Errorf("the error must name the missing role, got %v", err)
	}
	if fa.allocateN != 0 {
		t.Error("nothing must be allocated for a claim that cannot be satisfied")
	}
}

// The claim's requested size must fall inside the class's allowed range, and a
// size outside it is the client's error rather than an allocation of the wrong
// shape.
func TestCreateRejectsPrefixLengthOutsideClassRange(t *testing.T) {
	r, _, _, _ := newTestREST()
	claim := newClaim()
	length := int32(24)
	claim.Spec.PrefixLength = &length

	_, err := r.Create(projectContext("acme"), claim, nil, &metav1.CreateOptions{})
	if err == nil || !strings.Contains(err.Error(), "allowedPrefixLengths") {
		t.Fatalf("expected an allowedPrefixLengths error, got %v", err)
	}
}

// Exhaustion is 507, and the 507 must survive the mapping — a claim that
// exhausted its pool must not read as a server fault.
func TestCreateMapsExhaustionTo507(t *testing.T) {
	r, fa, _, ftx := newTestREST()
	fa.allocErr = allocator.ErrPoolExhausted

	_, err := r.Create(projectContext("acme"), newClaim(), nil, &metav1.CreateOptions{})
	if err == nil {
		t.Fatal("expected an error")
	}
	if got := errStatusCode(err); got != 507 {
		t.Errorf("status code = %d, want 507", got)
	}
	// Asserted as "nothing was committed" rather than an exact rollback count:
	// the reclaim probe opens and rolls back a transaction of its own on every
	// create, so the count is an implementation detail while "no claim was
	// persisted" is the property.
	if ftx.commits != 0 {
		t.Errorf("an exhausted claim must persist nothing, commits = %d", ftx.commits)
	}
	if ftx.rollbacks == 0 {
		t.Error("the allocation transaction must be rolled back")
	}
}

// A claim naming a specific address somebody else holds is a conflict, not
// exhaustion: the pool may have plenty of room and the caller asked for one
// address.
func TestCreateMapsTakenAddressToConflict(t *testing.T) {
	r, fa, _, _ := newTestREST()
	fa.allocErr = allocator.ErrAddressTaken

	_, err := r.Create(projectContext("acme"), newClaim(), nil, &metav1.CreateOptions{})
	if got := errStatusCode(err); got != 409 {
		t.Errorf("status code = %d, want 409 (got err %v)", got, err)
	}
}

// The reclaim policy recorded on the allocation is what the release path reads,
// so the claim's override must reach it. Before this was fixed the policy was
// never a parameter at all and every allocation was written as Delete.
func TestCreateRecordsEffectiveReclaimPolicy(t *testing.T) {
	t.Run("claim override wins", func(t *testing.T) {
		r, fa, _, _ := newTestREST()
		claim := newClaim()
		claim.Spec.ReclaimPolicy = ipam.ReclaimRetain

		if _, err := r.Create(projectContext("acme"), claim, nil, &metav1.CreateOptions{}); err != nil {
			t.Fatalf("Create returned error: %v", err)
		}
		if fa.gotRequest.ReclaimPolicy != string(ipam.ReclaimRetain) {
			t.Errorf("reclaimPolicy = %q, want Retain", fa.gotRequest.ReclaimPolicy)
		}
	})

	t.Run("class default applies when the claim says nothing", func(t *testing.T) {
		r, fa, fr, _ := newTestREST()
		fr.class.Spec.ReclaimPolicy = v1alpha1.ReclaimRetain

		if _, err := r.Create(projectContext("acme"), newClaim(), nil, &metav1.CreateOptions{}); err != nil {
			t.Fatalf("Create returned error: %v", err)
		}
		if fa.gotRequest.ReclaimPolicy != string(ipam.ReclaimRetain) {
			t.Errorf("reclaimPolicy = %q, want the class default Retain", fa.gotRequest.ReclaimPolicy)
		}
	})
}

// Retention works by not unbinding. The allocation object must survive with its
// claim reference cleared, rather than being deleted and later re-created —
// which would open a window in which the address is loose.
func TestDeleteRetainedAllocationIsUnboundNotDeleted(t *testing.T) {
	r, fa, _, _ := newTestREST()
	id := tenant.Identity{Kind: "Project", Name: "acme"}
	allocName := allocationNameFor("default", "claim-1")
	allocKey := allocationObjectKey(id, "default", allocName)
	claimKey := claimObjectKey(id, "default", "claim-1")

	claim := newClaim()
	claim.Status.BoundAllocationRef = &ipam.LocalRef{Name: allocName}
	fa.outcomes = []allocator.ReleaseOutcome{{AllocationKey: allocKey, Retained: true}}

	// unbindAllocation reads the allocation object through the transaction; the
	// fake transaction cannot serve that read, so it reports the object as
	// already gone and the handler proceeds. What this test pins is the branch:
	// the retained allocation's object is never handed to DeleteObject.
	if err := r.releaseAndDelete(projectContext("acme"), claim, claimKey, id); err == nil {
		for _, key := range fa.gotDeleteKeys {
			if key == allocKey {
				t.Fatal("a retained allocation's object must not be deleted")
			}
		}
	}
}

func TestDeletedAllocationIsRemoved(t *testing.T) {
	r, fa, _, _ := newTestREST()
	id := tenant.Identity{Kind: "Project", Name: "acme"}
	allocName := allocationNameFor("default", "claim-1")
	allocKey := allocationObjectKey(id, "default", allocName)
	claimKey := claimObjectKey(id, "default", "claim-1")

	claim := newClaim()
	claim.Status.BoundAllocationRef = &ipam.LocalRef{Name: allocName}
	fa.outcomes = []allocator.ReleaseOutcome{{AllocationKey: allocKey, Retained: false}}

	if err := r.releaseAndDelete(projectContext("acme"), claim, claimKey, id); err != nil {
		t.Fatalf("releaseAndDelete returned error: %v", err)
	}
	var sawAlloc, sawClaim bool
	for _, key := range fa.gotDeleteKeys {
		switch key {
		case allocKey:
			sawAlloc = true
		case claimKey:
			sawClaim = true
		}
	}
	if !sawAlloc {
		t.Error("a Delete-policy allocation's object must be removed")
	}
	if !sawClaim {
		t.Error("the claim row must be removed")
	}
}

// allocationNameFor must be a pure function of the claim's identity: that is
// what lets a replacement instance filling the same slot find the allocation
// its predecessor held.
func TestAllocationNameIsDeterministic(t *testing.T) {
	a := allocationNameFor("default", "hello-sandbox-americas-us-central-1-0-eth0-ipv4")
	b := allocationNameFor("default", "hello-sandbox-americas-us-central-1-0-eth0-ipv4")
	if a != b {
		t.Fatalf("allocation name is not deterministic: %q vs %q", a, b)
	}
	if c := allocationNameFor("other", "hello-sandbox-americas-us-central-1-0-eth0-ipv4"); c == a {
		t.Error("claims in different namespaces must not share an allocation name")
	}
}

func TestSingleAddressForm(t *testing.T) {
	v4 := &v1alpha1.IPClass{Spec: v1alpha1.IPClassSpec{IPFamily: v1alpha1.IPv4}}
	v6 := &v1alpha1.IPClass{Spec: v1alpha1.IPClassSpec{IPFamily: v1alpha1.IPv6}}

	if got := singleAddressForm("198.51.100.11/32", v4); got != "198.51.100.11" {
		t.Errorf("v4 host address = %q", got)
	}
	if got := singleAddressForm("fd20:a1b:2c3d:1::1/128", v6); got != "fd20:a1b:2c3d:1::1" {
		t.Errorf("v6 host address = %q", got)
	}
	// A /96 endpoint block has no single-address form. Reporting its first
	// address as the allocation would misrepresent what the holder got: the
	// interface receives a block and assigns within it.
	if got := singleAddressForm("fd20:a1b:2c3d:1:0:1::/96", v6); got != "" {
		t.Errorf("a block must have no single-address form, got %q", got)
	}
	if got := singleAddressForm("10.128.0.0/24", v4); got != "" {
		t.Errorf("a block must have no single-address form, got %q", got)
	}
}

// errStatusCode extracts the HTTP status an apierror maps to, or 0.
func errStatusCode(err error) int32 {
	var status interface{ Status() metav1.Status }
	if errors.As(err, &status) {
		return status.Status().Code
	}
	return 0
}

// --- class-consumption authorization ----------------------------------------

// allowChecker / denyChecker stand in for the SAR.
type staticClassChecker struct {
	allow bool
	asked []string
}

func (c *staticClassChecker) CanUseClass(_ context.Context, className string) (bool, error) {
	c.asked = append(c.asked, className)
	return c.allow, nil
}

// The class name is the only authorization boundary a claim crosses, and the
// design requires it to fail closed. A nil checker is a missing boundary, not an
// absent requirement.
func TestCreateDeniesWhenNoClassCheckerIsConfigured(t *testing.T) {
	r, fa, fr, _ := newTestREST()
	fr.class.Spec.Visibility = v1alpha1.VisibilityConsumer
	r.classChecker = nil

	_, err := r.Create(projectContext("acme"), newClaim(), nil, &metav1.CreateOptions{})
	if got := errStatusCode(err); got != 403 {
		t.Fatalf("status = %d, want 403 (err %v)", got, err)
	}
	if fa.allocateN != 0 {
		t.Error("nothing must be allocated for a caller who may not consume the class")
	}
	if fr.resolveN != 0 {
		t.Error("the pool must not be resolved before the class gate passes — the cascade provisions durable pools")
	}
}

func TestCreateEnforcesClassVisibility(t *testing.T) {
	tests := []struct {
		name       string
		visibility string
		allow      bool
		wantCode   int32
	}{
		{"an unmarked class is platform-only", "", true, 403},
		{"a platform class is never nameable by a tenant", v1alpha1.VisibilityPlatform, true, 403},
		{"a consumer class with a grant is allowed", v1alpha1.VisibilityConsumer, true, 0},
		{"a consumer class without a grant is denied", v1alpha1.VisibilityConsumer, false, 403},
		{"a shared class with a grant is allowed", v1alpha1.VisibilityShared, true, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r, _, fr, _ := newTestREST()
			fr.class.Spec.Visibility = tt.visibility
			r.classChecker = &staticClassChecker{allow: tt.allow}

			_, err := r.Create(projectContext("acme"), newClaim(), nil, &metav1.CreateOptions{})
			if tt.wantCode == 0 {
				if err != nil {
					t.Fatalf("expected success, got %v", err)
				}
				return
			}
			if got := errStatusCode(err); got != tt.wantCode {
				t.Fatalf("status = %d, want %d (err %v)", got, tt.wantCode, err)
			}
		})
	}
}

// A container class provisions pools for the classes below it and is nobody's to
// claim — binding a claim to one would take a whole subnet out of the space the
// endpoints below it draw from. That is a correctness rule, so it applies to
// platform callers too.
func TestCreateRejectsClaimsOnContainerClasses(t *testing.T) {
	for _, platform := range []bool{false, true} {
		r, fa, fr, _ := newTestREST()
		fr.class.Spec.PoolPer = []string{"network"}
		fr.class.Spec.Visibility = v1alpha1.VisibilityShared
		r.classChecker = &staticClassChecker{allow: true}

		ctx := projectContext("acme")
		if platform {
			ctx = platformContext()
		}
		_, err := r.Create(ctx, newClaim(), nil, &metav1.CreateOptions{})
		if got := errStatusCode(err); got != 403 {
			t.Errorf("platform=%v: status = %d, want 403 (err %v)", platform, got, err)
		}
		if fa.allocateN != 0 {
			t.Errorf("platform=%v: a container class must allocate nothing", platform)
		}
	}
}

// The platform authored the catalog, so it clears the visibility and SAR gates.
// Otherwise the service could not bootstrap.
//
// "The platform" is the configured platform project, not the absence of a
// tenant. The platform's own tooling authenticates as a project like everything
// else does.
func TestPlatformCallersBypassVisibilityAndSAR(t *testing.T) {
	r, _, fr, _ := newTestREST()
	fr.class.Spec.Visibility = v1alpha1.VisibilityPlatform
	r.classChecker = nil

	if _, err := r.Create(platformContext(), newClaim(), nil, &metav1.CreateOptions{}); err != nil {
		t.Fatalf("a platform caller must reach a platform class, got %v", err)
	}
}

// The other half of the same boundary, and the half that was inverted.
//
// A caller with no parent extras used to satisfy IsPlatform(), so it cleared
// the visibility gate and the SAR — while cmd/ipam's platformConsumerGuard
// already denies exactly that caller's claim CREATEs for having no derivable
// consumer. The bypass was held by the callers the service refuses to serve,
// and lost by the platform tooling that needs it.
//
// Note what makes this strict: classChecker is nil, which
// AuthorizeClassConsumption treats as a denial rather than a bypass. If an
// untenanted caller ever reads as the platform again, gate 2 and gate 3 are
// both skipped and this create succeeds.
func TestUntenantedCallersDoNotBypassVisibilityAndSAR(t *testing.T) {
	r, fa, fr, _ := newTestREST()
	fr.class.Spec.Visibility = v1alpha1.VisibilityPlatform
	r.classChecker = nil

	_, err := r.Create(untenantedContext(), newClaim(), nil, &metav1.CreateOptions{})
	if got := errStatusCode(err); got != 403 {
		t.Fatalf("status = %d, want 403: a caller with no tenant is not the platform (err %v)", got, err)
	}
	if fa.allocateN != 0 {
		t.Errorf("a denied caller allocated %d times; the gate must run before allocation", fa.allocateN)
	}
}

// A tenant whose project is not the configured platform project stays a tenant,
// including when the server has no platform project configured at all. The
// second case is the fail-closed direction: an operator who has not set the flag
// must not have handed the bypass to everyone.
func TestOnlyTheConfiguredProjectIsPlatform(t *testing.T) {
	for _, tc := range []struct {
		name string
		ctx  context.Context
	}{
		{name: "a different project", ctx: projectContext("acme")},
		{
			name: "the platform project name with no configuration",
			ctx: genericapirequest.WithUser(
				genericapirequest.WithNamespace(context.Background(), "default"),
				&user.DefaultInfo{
					Name: "system:serviceaccount:milo:network",
					Extra: map[string][]string{
						tenant.ExtraParentAPIGroup: {"resourcemanager.miloapis.com"},
						tenant.ExtraParentType:     {"Project"},
						tenant.ExtraParentName:     {testPlatformProject},
					},
				}),
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, _, fr, _ := newTestREST()
			fr.class.Spec.Visibility = v1alpha1.VisibilityPlatform
			r.classChecker = nil

			_, err := r.Create(tc.ctx, newClaim(), nil, &metav1.CreateOptions{})
			if got := errStatusCode(err); got != 403 {
				t.Fatalf("status = %d, want 403 (err %v)", got, err)
			}
		})
	}
}

// --- 507 detail --------------------------------------------------------------

// Nobody names a pool on the way in, so a bare "exhausted" leaves the caller
// unable to say what filled up and unable to find out. These are the facts only
// the failing request has.
func TestExhaustionReportsTheResolvedPool(t *testing.T) {
	r, fa, _, _ := newTestREST()
	fa.allocErr = &allocator.ExhaustionError{
		PoolKey:               "/ipam.miloapis.com/ippools/tenant-v4-us-central-1",
		RequestedPrefixLength: 32,
		UtilizationPercent:    98,
	}

	_, err := r.Create(projectContext("acme"), newClaim(), nil, &metav1.CreateOptions{})
	if got := errStatusCode(err); got != 507 {
		t.Fatalf("status = %d, want 507 (err %v)", got, err)
	}

	var statusErr interface{ Status() metav1.Status }
	if !errors.As(err, &statusErr) {
		t.Fatal("expected a StatusError")
	}
	details := statusErr.Status().Details
	if details == nil {
		t.Fatal("507 carries no Details; the caller cannot learn which pool ran out")
	}
	if details.Name != "tenant-v4-us-central-1" {
		t.Errorf("details.Name = %q, want the resolved pool name", details.Name)
	}
	causes := map[string]string{}
	for _, c := range details.Causes {
		causes[c.Field] = c.Message
	}
	for field, want := range map[string]string{
		registryerrors.CausePoolName:           "tenant-v4-us-central-1",
		registryerrors.CauseClassName:          "tenant-endpoint-ipv4",
		registryerrors.CauseRequestedPrefix:    "32",
		registryerrors.CauseUtilizationPercent: "98",
	} {
		if causes[field] != want {
			t.Errorf("cause %q = %q, want %q", field, causes[field], want)
		}
	}
}

// --- tracing -----------------------------------------------------------------

// Restored from the removed storage_tracing_test.go. The tenant-prefixing tests
// above cover correctness; these cover whether an operator can reconstruct what
// happened from a trace, which is a different property and the one that is
// otherwise unprotected.
func TestCreateEmitsAllocationSpan(t *testing.T) {
	recorder := tracetest.NewSpanRecorder()
	defer withTracerProvider(recorder)()

	r, _, _, _ := newTestREST()

	if _, err := r.Create(projectContext("acme"), newClaim(), nil, &metav1.CreateOptions{}); err != nil {
		t.Fatalf("Create returned error: %v", err)
	}

	span := findSpan(t, recorder, tracing.SpanClaimAllocate)
	attrs := spanAttrs(span)
	if attrs[tracing.AttrTenantProject] != "acme" {
		t.Errorf("%s = %q, want acme", tracing.AttrTenantProject, attrs[tracing.AttrTenantProject])
	}
	if attrs[tracing.AttrTenantScope] != "project" {
		t.Errorf("%s = %q, want project", tracing.AttrTenantScope, attrs[tracing.AttrTenantScope])
	}
	// The class is the one consumer-facing name in the model, and it is bounded
	// by the operator catalog — so it is safe on a span where a claim or pool
	// name would not be, and it is what an incident starts from.
	if attrs[tracing.AttrClassName] != "tenant-endpoint-ipv4" {
		t.Errorf("%s = %q, want the resolved class", tracing.AttrClassName, attrs[tracing.AttrClassName])
	}
	if attrs[tracing.AttrPoolName] != "tenant-v4-us-central-1" {
		t.Errorf("%s = %q, want the resolved pool", tracing.AttrPoolName, attrs[tracing.AttrPoolName])
	}
	if attrs[tracing.AttrDryRun] != "false" {
		t.Errorf("%s = %q, want false", tracing.AttrDryRun, attrs[tracing.AttrDryRun])
	}
}

// A failure has to be classifiable from the trace alone, because the reason a
// claim was refused is the first question during an incident and the API error
// is deliberately vaguer than the span.
func TestCreateRecordsFailureReasonOnSpan(t *testing.T) {
	tests := []struct {
		name       string
		setup      func(*AllocatingREST, *fakeAllocator, *fakeResolver)
		wantReason string
	}{
		{
			name: "exhaustion",
			setup: func(_ *AllocatingREST, fa *fakeAllocator, _ *fakeResolver) {
				fa.allocErr = allocator.ErrPoolExhausted
			},
			wantReason: tracing.ReasonExhausted,
		},
		{
			name: "class denied",
			setup: func(r *AllocatingREST, _ *fakeAllocator, fr *fakeResolver) {
				fr.class.Spec.Visibility = v1alpha1.VisibilityConsumer
				r.classChecker = &staticClassChecker{allow: false}
			},
			wantReason: tracing.ReasonClassDenied,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			recorder := tracetest.NewSpanRecorder()
			defer withTracerProvider(recorder)()

			r, fa, fr, _ := newTestREST()
			tt.setup(r, fa, fr)

			if _, err := r.Create(projectContext("acme"), newClaim(), nil, &metav1.CreateOptions{}); err == nil {
				t.Fatal("expected an error")
			}
			span := findSpan(t, recorder, tracing.SpanClaimAllocate)
			if got := spanAttrs(span)[tracing.AttrErrorReason]; got != tt.wantReason {
				t.Errorf("%s = %q, want %q", tracing.AttrErrorReason, got, tt.wantReason)
			}
		})
	}
}

func findSpan(t *testing.T, recorder *tracetest.SpanRecorder, name string) sdktrace.ReadOnlySpan {
	t.Helper()
	for _, s := range recorder.Ended() {
		if s.Name() == name {
			return s
		}
	}
	t.Fatalf("no span named %q was recorded", name)
	return nil
}

func spanAttrs(span sdktrace.ReadOnlySpan) map[string]string {
	out := map[string]string{}
	for _, kv := range span.Attributes() {
		out[string(kv.Key)] = kv.Value.Emit()
	}
	return out
}

// withTracerProvider installs a recording provider globally and returns a
// restore function. IPAM's spans come from the global provider — serve.go
// publishes one at startup — so a test that wants to see them has to replace it
// rather than inject one.
func withTracerProvider(recorder *tracetest.SpanRecorder) func() {
	previous := otel.GetTracerProvider()
	otel.SetTracerProvider(sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder)))
	return func() { otel.SetTracerProvider(previous) }
}

// A release that releases nothing must not report success.
//
// If the claim reports an address but no allocation row matched, the claim and
// the allocation table disagree — and deleting the claim anyway leaves the
// address held by a row nothing references, permanently and silently. This is
// the same failure the IPAllocation delete path guards, reached from the other
// end; both are a release succeeding having released nothing.
func TestReleaseThatFindsNothingRefusesToDeleteTheClaim(t *testing.T) {
	r, fa, _, _ := newTestREST()
	id := tenant.Identity{Kind: "Project", Name: "acme"}
	claimKey := claimObjectKey(id, "default", "claim-1")

	claim := newClaim()
	claim.Status.AllocatedCIDR = "10.128.0.2/32"
	claim.Status.BoundAllocationRef = &ipam.LocalRef{Name: allocationNameFor("default", "claim-1")}
	fa.outcomes = nil // the allocator found no row to release

	err := r.releaseAndDelete(projectContext("acme"), claim, claimKey, id)
	if err == nil {
		t.Fatal("expected the release to be refused when nothing was released")
	}
	if !strings.Contains(err.Error(), "10.128.0.2/32") {
		t.Errorf("the error must name the address that would have leaked, got %v", err)
	}
	for _, key := range fa.gotDeleteKeys {
		if key == claimKey {
			t.Fatal("the claim must not be deleted when its address was not released")
		}
	}
}

// The guard must not fire on a claim that legitimately holds nothing — a claim
// that never bound has no address to leak, and refusing to delete it would make
// it undeletable.
func TestReleaseOfAnUnboundClaimIsNotRefused(t *testing.T) {
	r, fa, _, _ := newTestREST()
	id := tenant.Identity{Kind: "Project", Name: "acme"}
	claimKey := claimObjectKey(id, "default", "claim-1")

	claim := newClaim()
	claim.Status.AllocatedCIDR = ""
	fa.outcomes = nil

	if err := r.releaseAndDelete(projectContext("acme"), claim, claimKey, id); err != nil {
		t.Fatalf("an unbound claim must delete cleanly, got %v", err)
	}
	var sawClaim bool
	for _, key := range fa.gotDeleteKeys {
		if key == claimKey {
			sawClaim = true
		}
	}
	if !sawClaim {
		t.Error("the claim row must be deleted")
	}
}
