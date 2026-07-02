package postgres

import (
	"bytes"
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apiserver/pkg/storage"

	ipamapiserver "go.miloapis.com/ipam/internal/apiserver"
	"go.miloapis.com/ipam/pkg/apis/ipam"
)

// TestWatchAndListConvertRace stresses the shared s.codec across every path
// that uses it concurrently — GetList decode, Create/Delete encode+decode,
// the watcher poll-loop decode, and consumer-side re-encode (the apiserver
// watch/list serializer). This is the broadest reproduction attempt for the
// perf-spike heap corruption: if any of these shares unsynchronized mutable
// state, -race names the conflict. Skips without Docker.
func TestWatchAndListConvertRace(t *testing.T) {
	db := startEphemeralPostgres(t)
	codec := ipamapiserver.Codecs.LegacyCodec(ipamapiserver.Scheme.PrioritizedVersionsAllGroups()...)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	store := NewWithWatchExclusions(db, codec, "", nil) // polling-only watcher
	store.SetNewFunc(func() runtime.Object { return &ipam.IPPool{} })
	t.Cleanup(store.Stop)

	poolObj := func(i int) *ipam.IPPool {
		return &ipam.IPPool{
			TypeMeta:   metav1.TypeMeta{APIVersion: "ipam.miloapis.com/v1alpha1", Kind: "IPPool"},
			ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("seed-%02d", i), Labels: map[string]string{"env": "perf"}},
			Spec:       ipam.IPPoolSpec{CIDR: fmt.Sprintf("10.%d.0.0/16", i), IPFamily: ipam.IPv4, ClassNames: []string{"public-egress", "internal-ipv4"}},
			Status: ipam.IPPoolStatus{Phase: ipam.PoolReady, AllocatedCIDR: fmt.Sprintf("10.%d.0.0/16", i), IPFamily: ipam.IPv4,
				Conditions: []metav1.Condition{{Type: "Allocated", Status: metav1.ConditionTrue, Reason: "ok", Message: "ready", LastTransitionTime: metav1.Now()}}},
		}
	}

	// Seed baseline pools.
	const seeds = 8
	for i := 0; i < seeds; i++ {
		out := &ipam.IPPool{}
		if err := store.Create(ctx, fmt.Sprintf("/ipam.miloapis.com/ippools/seed-%02d", i), poolObj(i), out, 0); err != nil {
			t.Fatalf("seed create: %v", err)
		}
	}

	var wg sync.WaitGroup
	var stop atomic.Bool

	// Watchers: each poll goroutine decodes changelog events via the shared
	// codec; consumers re-encode them (the watch serializer path).
	for w := 0; w < 6; w++ {
		iface, err := store.Watch(ctx, "/ipam.miloapis.com/ippools", storage.ListOptions{Recursive: true, Predicate: storage.Everything})
		if err != nil {
			t.Fatalf("watch: %v", err)
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer iface.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case ev, ok := <-iface.ResultChan():
					if !ok {
						return
					}
					if ev.Object != nil {
						var buf bytes.Buffer
						_ = codec.Encode(ev.Object, &buf)
					}
					if stop.Load() {
						return
					}
				}
			}
		}()
	}

	// Readers: GetList decode + convert + encode.
	for r := 0; r < 24; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for !stop.Load() {
				list := &ipam.IPPoolList{}
				if err := store.GetList(ctx, "/ipam.miloapis.com/ippools", storage.ListOptions{Recursive: true, Predicate: storage.Everything}, list); err != nil {
					if ctx.Err() != nil {
						return
					}
					continue
				}
				var buf bytes.Buffer
				_ = codec.Encode(list, &buf)
			}
		}()
	}

	// Writers: churn extra pools (ADDED/DELETED changelog) to feed watchers.
	for wr := 0; wr < 4; wr++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			n := 0
			for !stop.Load() {
				key := fmt.Sprintf("/ipam.miloapis.com/ippools/churn-%d-%d", id, n)
				p := poolObj(id)
				p.Name = fmt.Sprintf("churn-%d-%d", id, n)
				out := &ipam.IPPool{}
				if err := store.Create(ctx, key, p, out, 0); err != nil {
					if ctx.Err() != nil {
						return
					}
					continue
				}
				del := &ipam.IPPool{}
				_ = store.Delete(ctx, key, del, nil, nil, nil, storage.DeleteOptions{})
				n++
			}
		}(wr)
	}

	time.Sleep(6 * time.Second)
	stop.Store(true)
	cancel()
	wg.Wait()
}
