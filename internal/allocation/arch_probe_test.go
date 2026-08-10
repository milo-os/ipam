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

// The architecture probe that settled #35.
//
// #35 was reported as heap corruption in this package: three fatal signatures
// ("found pointer to free object", SIGSEGV, and a nil dereference in math/big
// with a torn slice header), always on this path, at ~300 carves in one pool,
// with serial traffic. This package is stdlib-only with no unsafe and no cgo,
// so it could not be the culprit — and it was not the victim of anything in our
// code either.
//
// **It was QEMU.** The deployed image is built with GOARCH from an ARG that
// defaults to amd64 (Dockerfile) and nothing overrode it, so on an arm64 host
// the apiserver binary was amd64 running under emulation. Emulated Go binaries
// produce exactly those signatures, because Go's atomics, memory model and
// signal-based preemption are hard to emulate faithfully.
//
// Compile this for two architectures and run both in Docker to see it:
//
//	GOOS=linux GOARCH=arm64 go test -c -o probe-arm64 ./internal/allocation/
//	GOOS=linux GOARCH=amd64 go test -c -o probe-amd64 ./internal/allocation/
//	docker run --rm --platform linux/arm64 -e QEMU_PROBE=2000 ... /probe-arm64 -test.run TestArchProbe
//	docker run --rm --platform linux/amd64 -e QEMU_PROBE=2000 ... /probe-amd64 -test.run TestArchProbe
//
// Native arm64 survives 2000 carves every time. Emulated amd64 faults within
// ~100, at a different address each run. No apiserver, no database, no HTTP, no
// concurrency — the emulation alone is sufficient.
//
// Kept, and skipped unless QEMU_PROBE is set, because "our pure-Go package is
// corrupting the heap" is a conclusion that will be reached again, and this is
// the ten-second way to rule the architecture in or out before hunting for a
// memory bug that is not there.
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
		if _, ok := LargestFreePrefixLen(parents, existing); !ok {
			t.Fatalf("carve %d: no largest free prefix", i)
		}
		_ = UtilizationPercent(parents, existing)

		if i%500 == 499 {
			var m runtime.MemStats
			runtime.ReadMemStats(&m)
			fmt.Printf("  %d carves ok, heap=%dMB gc=%d\n", i+1, m.HeapAlloc>>20, m.NumGC)
		}
	}
	fmt.Printf("SURVIVED %d carves on %s\n", n, runtime.GOARCH)
}
