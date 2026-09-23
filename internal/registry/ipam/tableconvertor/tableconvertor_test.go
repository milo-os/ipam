package tableconvertor

import (
	"context"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apiserver/pkg/registry/rest"

	"go.miloapis.com/ipam/pkg/apis/ipam"
)

func cellsByColumn(t *testing.T, table *metav1.Table) map[string]interface{} {
	t.Helper()
	if len(table.Rows) != 1 {
		t.Fatalf("rows = %d, want 1", len(table.Rows))
	}
	if len(table.Rows[0].Cells) != len(table.ColumnDefinitions) {
		t.Fatalf("row has %d cells but %d columns are defined; a column would print under the wrong header",
			len(table.Rows[0].Cells), len(table.ColumnDefinitions))
	}
	out := make(map[string]interface{}, len(table.ColumnDefinitions))
	for i, col := range table.ColumnDefinitions {
		out[col.Name] = table.Rows[0].Cells[i]
	}
	return out
}

// A provisioned pool reports the range it was carved, a root the range it
// declares, and the family follows the same fallback.
func TestIPPoolPrefersTheCarvedRange(t *testing.T) {
	for _, tc := range []struct {
		name       string
		pool       *ipam.IPPool
		wantCIDR   string
		wantFamily string
	}{
		{
			name: "provisioned pool: status wins",
			pool: &ipam.IPPool{
				ObjectMeta: metav1.ObjectMeta{Name: "child"},
				Spec:       ipam.IPPoolSpec{CIDR: "10.0.0.0/8", IPFamily: ipam.IPv4},
				Status:     ipam.IPPoolStatus{AllocatedCIDR: "10.1.0.0/20", IPFamily: ipam.IPv4},
			},
			wantCIDR: "10.1.0.0/20", wantFamily: "IPv4",
		},
		{
			name: "root pool: spec is all there is",
			pool: &ipam.IPPool{
				ObjectMeta: metav1.ObjectMeta{Name: "root"},
				Spec:       ipam.IPPoolSpec{CIDR: "2001:db8::/32", IPFamily: ipam.IPv6},
			},
			wantCIDR: "2001:db8::/32", wantFamily: "IPv6",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			table, err := IPPools().ConvertToTable(context.Background(), tc.pool, nil)
			if err != nil {
				t.Fatalf("ConvertToTable: %v", err)
			}
			cells := cellsByColumn(t, table)
			if got := cells["CIDR"]; got != tc.wantCIDR {
				t.Errorf("CIDR = %v, want %v", got, tc.wantCIDR)
			}
			if got := cells["Family"]; got != tc.wantFamily {
				t.Errorf("Family = %v, want %v", got, tc.wantFamily)
			}
		})
	}
}

// Age is the elapsed-time string kubectl prints, not a timestamp. This is the
// whole reason the convertor exists, so it is asserted directly.
func TestAgeIsRelativeNotATimestamp(t *testing.T) {
	pool := &ipam.IPPool{ObjectMeta: metav1.ObjectMeta{
		Name:              "p",
		CreationTimestamp: metav1.NewTime(time.Now().Add(-49 * time.Hour)),
	}}
	table, err := IPPools().ConvertToTable(context.Background(), pool, nil)
	if err != nil {
		t.Fatalf("ConvertToTable: %v", err)
	}
	if got := cellsByColumn(t, table)["Age"]; got != "2d1h" {
		t.Errorf("Age = %v, want 2d1h", got)
	}
}

// An object created before the fix that repaired them has a zero timestamp.
// It must print as unknown rather than as an age measured from year one.
func TestZeroCreationTimestampPrintsUnknown(t *testing.T) {
	table, err := IPPools().ConvertToTable(context.Background(),
		&ipam.IPPool{ObjectMeta: metav1.ObjectMeta{Name: "p"}}, nil)
	if err != nil {
		t.Fatalf("ConvertToTable: %v", err)
	}
	if got := cellsByColumn(t, table)["Age"]; got != "<unknown>" {
		t.Errorf("Age = %v, want <unknown>", got)
	}
}

// The wide columns must be marked, or a client shows every one of them by
// default and the listing is wider than the two-column default it replaced.
func TestDefaultColumnsAreTheIdentifyingOnes(t *testing.T) {
	for _, tc := range []struct {
		resource  string
		convertor rest.TableConvertor
		obj       runtime.Object
		want      []string
	}{
		{"ippools", IPPools(), &ipam.IPPool{}, []string{"Name", "CIDR", "Family", "Class", "Utilization", "Age"}},
		{"ipclaims", IPClaims(), &ipam.IPClaim{}, []string{"Name", "Class", "Family", "Allocated", "Pool", "Phase", "Age"}},
		{"ipallocations", IPAllocations(), &ipam.IPAllocation{}, []string{"Name", "Allocated", "Pool", "Class", "Purpose", "Phase", "Age"}},
		{"ipclasses", IPClasses(), &ipam.IPClass{}, []string{"Name", "Family", "Parent", "Prefix", "Pool Per", "Phase", "Age"}},
	} {
		t.Run(tc.resource, func(t *testing.T) {
			table, err := tc.convertor.ConvertToTable(context.Background(), tc.obj, nil)
			if err != nil {
				t.Fatalf("ConvertToTable: %v", err)
			}
			var got []string
			var wideCount int
			for _, c := range table.ColumnDefinitions {
				if c.Priority == 0 {
					got = append(got, c.Name)
				} else {
					wideCount++
				}
			}
			if len(got) != len(tc.want) {
				t.Fatalf("default columns = %v, want %v", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Errorf("default column %d = %q, want %q", i, got[i], tc.want[i])
				}
			}
			if wideCount == 0 {
				t.Error("no wide columns: everything would print by default")
			}
			// Every column must have a cell, in both the default and wide sets.
			if n := len(table.Rows[0].Cells); n != len(table.ColumnDefinitions) {
				t.Errorf("%d cells for %d columns", n, len(table.ColumnDefinitions))
			}
		})
	}
}

// NoHeaders means the client already printed them; sending them again would
// have the server override a client that asked for none.
func TestNoHeadersOmitsColumnDefinitions(t *testing.T) {
	table, err := IPPools().ConvertToTable(context.Background(),
		&ipam.IPPool{ObjectMeta: metav1.ObjectMeta{Name: "p"}},
		&metav1.TableOptions{NoHeaders: true})
	if err != nil {
		t.Fatalf("ConvertToTable: %v", err)
	}
	if len(table.ColumnDefinitions) != 0 {
		t.Errorf("got %d column definitions with NoHeaders, want 0", len(table.ColumnDefinitions))
	}
	if len(table.Rows) != 1 {
		t.Errorf("rows = %d, want 1: NoHeaders drops the headers, not the data", len(table.Rows))
	}
}

// A list must carry its pagination state through, or a client that pages
// through pools with --chunk-size stops after the first page.
func TestListCarriesPaginationState(t *testing.T) {
	list := &ipam.IPPoolList{
		ListMeta: metav1.ListMeta{ResourceVersion: "42", Continue: "next-page"},
		Items: []ipam.IPPool{
			{ObjectMeta: metav1.ObjectMeta{Name: "a"}},
			{ObjectMeta: metav1.ObjectMeta{Name: "b"}},
		},
	}
	table, err := IPPools().ConvertToTable(context.Background(), list, nil)
	if err != nil {
		t.Fatalf("ConvertToTable: %v", err)
	}
	if len(table.Rows) != 2 {
		t.Fatalf("rows = %d, want 2", len(table.Rows))
	}
	if table.ResourceVersion != "42" || table.Continue != "next-page" {
		t.Errorf("list meta = %q/%q, want 42/next-page", table.ResourceVersion, table.Continue)
	}
}

// status.utilizationPercent is rounded to four decimal places, so a real
// allocation out of an IPv6 pool arrives here as a literal 0. Reading emptiness
// off that number reports every fabric-identity and subnet pool in staging as
// untouched when they are not; the capacity counts are exact and settle it.
func TestUtilizationReadsEmptinessFromCapacity(t *testing.T) {
	for _, tc := range []struct {
		name string
		pool ipam.IPPool
		want string
	}{
		{
			name: "never drawn on",
			pool: ipam.IPPool{Status: ipam.IPPoolStatus{
				Capacity: ipam.PoolCapacity{Allocated: "0", Total: "18446744073709551616"},
			}},
			want: "0%",
		},
		{
			name: "drawn on, but rounds to zero: a /48 out of a /32",
			pool: ipam.IPPool{Status: ipam.IPPoolStatus{
				UtilizationPercent: 0,
				Capacity:           ipam.PoolCapacity{Allocated: "1033017668127734890496", Total: "79228162514264337593543950336"},
			}},
			want: "<0.1%",
		},
		{
			name: "measurable",
			pool: ipam.IPPool{Status: ipam.IPPoolStatus{
				UtilizationPercent: 12.34,
				Capacity:           ipam.PoolCapacity{Allocated: "64", Total: "512"},
			}},
			want: "12.3%",
		},
		{
			name: "full",
			pool: ipam.IPPool{Status: ipam.IPPoolStatus{
				UtilizationPercent: 100,
				Capacity:           ipam.PoolCapacity{Allocated: "512", Total: "512"},
			}},
			want: "100.0%",
		},
		{
			name: "no capacity reported at all",
			pool: ipam.IPPool{Status: ipam.IPPoolStatus{}},
			want: "0%",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := poolUtilization(&tc.pool); got != tc.want {
				t.Errorf("poolUtilization = %q, want %q", got, tc.want)
			}
		})
	}
}

// Exactly one of classNames and classRef is set on any given pool. Printing
// only classNames showed "<none>" for every cascade-provisioned pool, which in
// staging is nearly all of them.
func TestClassColumnIsPopulatedOnBothKindsOfPool(t *testing.T) {
	operator := &ipam.IPPool{Spec: ipam.IPPoolSpec{ClassNames: []string{"datum-fabric-identity"}}}
	if got := poolClass(operator); got != "datum-fabric-identity" {
		t.Errorf("operator-authored pool class = %q, want the class it offers itself to", got)
	}
	provisioned := &ipam.IPPool{Spec: ipam.IPPoolSpec{ClassRef: &ipam.LocalRef{Name: "datum-subnet-ipv6"}}}
	if got := poolClass(provisioned); got != "datum-subnet-ipv6" {
		t.Errorf("provisioned pool class = %q, want the class that carved it", got)
	}
	if got := poolClass(&ipam.IPPool{}); got != "<none>" {
		t.Errorf("pool with neither = %q, want <none>", got)
	}
}

// Scope renders as sorted role=name pairs, matching the milo-ipam plugin, so
// the same pool reads identically whichever surface printed it.
func TestScopeRendersSortedPairs(t *testing.T) {
	got := formatScope(map[string]ipam.ScopeRef{
		"location": {Name: "lon1"},
		"cluster":  {Name: "c1"},
	})
	if got != "cluster=c1 location=lon1" {
		t.Errorf("formatScope = %q, want sorted role=name pairs", got)
	}
	if got := formatScope(nil); got != "<none>" {
		t.Errorf("formatScope(nil) = %q, want <none>", got)
	}
}
