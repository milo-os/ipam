package ipamnames

import (
	"strings"
	"testing"
)

// The values are pinned because existing allocations are stored under them.
// A change here renames nothing that already exists: it makes every retained
// allocation unreachable by name, and lets a recreated claim draw a fresh
// address where it should have hit its predecessor's.
func TestAllocationNameIsPinned(t *testing.T) {
	tests := []struct {
		namespace string
		name      string
		want      string
	}{
		{namespace: "default", name: "example", want: "alloc-17fae90dba6f7d6d"},
		{namespace: "tenant-a", name: "web-eth0", want: "alloc-c7e24ff03ea00955"},
	}
	for _, tt := range tests {
		if got := AllocationName(tt.namespace, tt.name); got != tt.want {
			t.Errorf("AllocationName(%q, %q) = %q, want %q", tt.namespace, tt.name, got, tt.want)
		}
	}
}

func TestAllocationNameSeparatesNamespaceFromName(t *testing.T) {
	if AllocationName("a", "b-c") == AllocationName("a-b", "c") {
		t.Fatal("claims in different namespaces must not share an allocation name")
	}
}

func TestAllocationNameIsFixedLengthForAnyClaimName(t *testing.T) {
	long := strings.Repeat("x", 253)
	got := AllocationName("default", long)
	if want := len(AllocationPrefix) + 16; len(got) != want {
		t.Fatalf("len(AllocationName) = %d, want %d", len(got), want)
	}
	if !strings.HasPrefix(got, AllocationPrefix) {
		t.Fatalf("AllocationName = %q, want prefix %q", got, AllocationPrefix)
	}
}
