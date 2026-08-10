package allocation

import (
	"fmt"
	"net"
	"os"
	"runtime"
	"runtime/debug"
	"strconv"
	"testing"
)

// TestArchProbe tells an emulated binary from a real memory bug, in ten
// seconds, before anyone hunts for corruption in this package.
//
// THE SIGNATURES IT EXPLAINS. "found pointer to free object", a SIGSEGV, or a
// nil dereference in math/big with a torn slice header — all on the allocation
// path, at a few hundred carves in one pool, under serial traffic. This package
// is stdlib-only with no unsafe and no cgo, so it cannot corrupt a heap.
//
// Emulated Go binaries produce exactly those signatures, because Go's atomics,
// memory model and signal-based preemption are hard to emulate faithfully. The
// Dockerfile takes GOARCH from an ARG defaulting to amd64, so an image built
// without overriding it runs under QEMU on an arm64 host.
//
// Compile for two architectures and run both in Docker:
//
//	GOOS=linux GOARCH=arm64 go test -c -o probe-arm64 ./internal/allocation/
//	GOOS=linux GOARCH=amd64 go test -c -o probe-amd64 ./internal/allocation/
//	docker run --rm --platform linux/arm64 -e QEMU_PROBE=2000 ... /probe-arm64 -test.run TestArchProbe
//	docker run --rm --platform linux/amd64 -e QEMU_PROBE=2000 ... /probe-amd64 -test.run TestArchProbe
//
// Native arm64 survives 2000 carves every time. Emulated amd64 faults within
// ~100, at a different address each run. No apiserver, no database, no HTTP and
// no concurrency are involved: the emulation alone is sufficient.
//
// Skipped unless QEMU_PROBE is set, so it costs a normal run nothing.
func TestArchProbe(t *testing.T) {
	if os.Getenv("QEMU_PROBE") == "" {
		t.Skip("set QEMU_PROBE=<carves> to run the architecture probe")
	}
	n, _ := strconv.Atoi(os.Getenv("QEMU_PROBE"))
	fmt.Printf("GOARCH=%s GOOS=%s go=%s carves=%d\n", runtime.GOARCH, runtime.GOOS, runtime.Version(), n)

	// Hard GC, which is what the quadratic churn in the real path produces and
	// what any emulation fault needs in order to surface.
	debug.SetGCPercent(20)

	_, parent, err := net.ParseCIDR("fd00::/20")
	if err != nil {
		t.Fatal(err)
	}
	parents := []net.IPNet{*parent}

	var existing []net.IPNet
	for i := range n {
		got, err := FindFirstAvailableBlock(parents, existing, 48, FirstFit)
		if err != nil {
			t.Fatalf("carve %d: %v", i, err)
		}
		existing = append(existing, *got)
		_ = UtilizationPercent(parents, existing)

		if i%500 == 499 {
			var m runtime.MemStats
			runtime.ReadMemStats(&m)
			fmt.Printf("  %d carves ok, heap=%dMB gc=%d\n", i+1, m.HeapAlloc>>20, m.NumGC)
		}
	}
	fmt.Printf("SURVIVED %d carves on %s\n", n, runtime.GOARCH)
}
