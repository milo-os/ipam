package ipamregistry_test

// Every allocator.InsertObject call site in the registries must map its error
// through registryerrors.MapWriteError.
//
// This is rule 4a's shape: a verb that writes an object itself, instead of
// delegating to the embedded genericregistry.Store, inherits none of the
// storage layer's guarantees — including its unique-violation mapping. That is
// why creating a duplicate IPClass answered 409 AlreadyExists while creating a
// duplicate IPPool answered 500 carrying `duplicate key value violates unique
// constraint "ipam_objects_pkey" (SQLSTATE 23505)`.
//
// The fix is per-call-site, so the failure mode is a *new* call site written
// the obvious way — `fmt.Errorf("persist thing: %w", err)` — which reads as
// correct and quietly reintroduces the 500. Nothing in the type system objects:
// InsertObject returns a plain error and wrapping it compiles.
//
// So this scans the source. It is a coarse instrument on purpose: it asserts
// only that MapWriteError appears in the handful of lines following each
// InsertObject call, which is enough to fail on the mistake it exists for
// without pretending to understand control flow.
//
// It cannot see whether the mapped error is returned *unwrapped*. That part
// matters just as much — the apiserver renders a status by a direct type switch
// and does not unwrap, so a StatusError inside fmt.Errorf degrades silently to
// a 500 — and it is covered by TestMapWriteError_ReturnsUsableStatusError in
// registryerrors, plus the e2e duplicate-create assertions.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// insertObjectWindow is how many lines after an InsertObject call the mapping
// must appear in. The call sites all follow the same shape (rollback, maybe a
// metrics counter, then the return), and the longest one has a comment in it.
const insertObjectWindow = 12

func TestEveryInsertObjectCallSiteMapsItsError(t *testing.T) {
	// Walk the registry tree rather than listing packages: a new resource
	// directory is exactly the case this guard is for, and a list would not
	// include it.
	root := "."

	var sites int
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		src, rerr := os.ReadFile(path)
		if rerr != nil {
			return rerr
		}
		lines := strings.Split(string(src), "\n")
		for i, line := range lines {
			if !strings.Contains(line, ".InsertObject(ctx,") {
				continue
			}
			sites++
			end := min(i+1+insertObjectWindow, len(lines))
			window := strings.Join(lines[i+1:end], "\n")
			if !strings.Contains(window, "registryerrors.MapWriteError") {
				t.Errorf("%s:%d: InsertObject call site does not map its error through "+
					"registryerrors.MapWriteError within %d lines.\n"+
					"Without it a duplicate object answers HTTP 500 carrying the Postgres "+
					"constraint name instead of 409 AlreadyExists.\nSaw:\n%s",
					path, i+1, insertObjectWindow, window)
				continue
			}
			// The mapped error has to be returned as-is. The apiserver renders a
			// status by a direct type switch on the Status() interface and does not
			// unwrap, so a StatusError inside fmt.Errorf("%w") degrades silently to
			// the 500 this whole change exists to remove — and it compiles, reads as
			// idiomatic, and is invisible in review.
			for _, l := range strings.Split(window, "\n") {
				if !strings.Contains(l, "registryerrors.MapWriteError") {
					continue
				}
				if !strings.HasPrefix(strings.TrimSpace(l), "return") {
					t.Errorf("%s:%d: the mapped error must be returned unwrapped; the "+
						"apiserver type-switches on it and does not unwrap. Saw:\n%s",
						path, i+1, l)
				}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk registry sources: %v", err)
	}

	// A positive control. If the scan stops finding call sites — a rename, a
	// move, a change to the argument list — every assertion above passes
	// vacuously and this guard becomes a test that proves nothing while staying
	// green. There were four at the time of writing.
	if sites < 4 {
		t.Fatalf("found only %d InsertObject call sites; expected at least 4. "+
			"Either they moved out of internal/registry/ipam or the pattern this "+
			"scan matches has changed — in which case the scan is broken, not the code", sites)
	}
}
