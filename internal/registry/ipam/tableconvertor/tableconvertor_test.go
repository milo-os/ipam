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
		{"ippools", IPPools(), &ipam.IPPool{}, []string{"Name", "CIDR", "Family", "Classes", "Utilization", "Age"}},
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

// A pool holding one /64 out of a /32 is not empty. Rounding it to 0% would
// say the pool is untouched when it is not.
func TestPercentDistinguishesEmptyFromNearlyEmpty(t *testing.T) {
	for _, tc := range []struct {
		in   float64
		want string
	}{
		{0, "0%"},
		{0.0000001, "<0.1%"},
		{0.04, "<0.1%"},
		{12.34, "12.3%"},
		{100, "100.0%"},
	} {
		if got := percent(tc.in); got != tc.want {
			t.Errorf("percent(%v) = %q, want %q", tc.in, got, tc.want)
		}
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
