package apiserver

import (
	"bytes"
	"sync"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"go.miloapis.com/ipam/pkg/apis/ipam"
)

// poolJSON is a v1alpha1 IPPool with classNames + conditions, the shape the
// perf read-spike lists over.
var poolJSON = []byte(`{"apiVersion":"ipam.miloapis.com/v1alpha1","kind":"IPPool",` +
	`"metadata":{"name":"prod-backbone"},` +
	`"spec":{"cidr":"10.0.0.0/16","ipFamily":"IPv4","classNames":["public-egress","internal-ipv4"]},` +
	`"status":{"phase":"Ready","allocatedCIDR":"10.0.0.0/16","ipFamily":"IPv4",` +
	`"conditions":[{"type":"Allocated","status":"True","reason":"AllocationSucceeded","message":"ready","lastTransitionTime":"2020-01-01T00:00:00Z"}]}}`)

// TestConcurrentPoolCodec is the regression guard for the perf read-spike heap
// corruption ("found bad pointer in Go heap" in the IPPool conversion path). It
// exercises the LIST hot path — decode (v1alpha1 JSON → internal) and encode
// (internal → v1alpha1 wire) — through a SINGLE shared codec across many
// goroutines, exactly as the postgres store (GetList) and the apiserver response
// writer do under a read spike, with each goroutine owning its own fresh objects
// (as the real request path does).
//
// Invariant it locks in: with per-request (exclusively-owned) objects, concurrent
// decode+encode through the shared codec/scheme is race-free. The corruption only
// occurs if the SAME top-level object is encoded by two goroutines at once,
// because the versioning codec's encode path mutates the object's TypeMeta in
// place (SetGroupVersionKind + deferred restore) via the unsafe object convertor.
// The fix for the incident is therefore to never share a to-be-encoded object
// across request goroutines — which the store's fresh-object read path already
// guarantees, and this test enforces going forward. Run with -race.
func TestConcurrentPoolCodec(t *testing.T) {
	codec := Codecs.LegacyCodec(Scheme.PrioritizedVersionsAllGroups()...)

	const goroutines = 48
	const iterations = 400

	var wg sync.WaitGroup
	errCh := make(chan error, goroutines)
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < iterations; i++ {
				// Decode: fresh internal object per row, like store.GetList.
				into := &ipam.IPPool{}
				if _, _, err := codec.Decode(poolJSON, nil, into); err != nil {
					errCh <- err
					return
				}
				// Build a list and encode it back to the wire, like the
				// apiserver response writer serializing a LIST.
				list := &ipam.IPPoolList{
					TypeMeta: metav1.TypeMeta{APIVersion: "ipam.miloapis.com/v1alpha1", Kind: "IPPoolList"},
					Items:    []ipam.IPPool{*into, *into, *into},
				}
				var buf bytes.Buffer
				if err := codec.Encode(list, &buf); err != nil {
					errCh <- err
					return
				}
			}
		}()
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Fatalf("codec round-trip error: %v", err)
	}
}
