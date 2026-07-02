package postgres

import (
	"bytes"
	"context"
	"fmt"
	"sync"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apiserver/pkg/storage"

	ipamapiserver "go.miloapis.com/ipam/internal/apiserver"
	"go.miloapis.com/ipam/pkg/apis/ipam"
	ipamv1 "go.miloapis.com/ipam/pkg/apis/ipam/v1alpha1"
)

// TestGetListConvertRace reproduces the perf read-spike heap corruption path
// against a real Postgres: many goroutines concurrently drive
// Store.GetList (decode via the shared s.codec) and then encode the resulting
// internal list back to the v1alpha1 wire form (the convert_ipam_IPPool ->
// convert_ipam_IPPoolList path the GC caught). It uses the PRODUCTION codec
// (LegacyCodec over PrioritizedVersionsAllGroups) so the internal<->v1alpha1
// conversion actually runs, unlike the v1alpha1<->v1alpha1 test codec.
//
// Run with -race. Skips when Docker is unavailable.
func TestGetListConvertRace(t *testing.T) {
	db := startEphemeralPostgres(t)

	// Production codec: encode/decode round-trips through the internal type,
	// exercising the hand-written conversions (incl. the IPPool ClassNames /
	// Conditions copies) exactly as the aggregated apiserver does.
	codec := ipamapiserver.Codecs.LegacyCodec(ipamapiserver.Scheme.PrioritizedVersionsAllGroups()...)

	ctx := context.Background()

	// Seed a batch of IPPool rows under a cluster-scoped key prefix, each with
	// classNames + status conditions so the conversion copies real slices.
	const pools = 12
	for i := 0; i < pools; i++ {
		p := &ipamv1.IPPool{
			TypeMeta:   metav1.TypeMeta{APIVersion: "ipam.miloapis.com/v1alpha1", Kind: "IPPool"},
			ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("pool-%02d", i), Labels: map[string]string{"env": "perf"}},
			Spec: ipamv1.IPPoolSpec{
				CIDR:       fmt.Sprintf("10.%d.0.0/16", i),
				IPFamily:   ipamv1.IPv4,
				ClassNames: []string{"public-egress", "internal-ipv4"},
			},
			Status: ipamv1.IPPoolStatus{
				Phase:         ipamv1.PoolReady,
				AllocatedCIDR: fmt.Sprintf("10.%d.0.0/16", i),
				IPFamily:      ipamv1.IPv4,
				Conditions: []metav1.Condition{{
					Type: "Allocated", Status: metav1.ConditionTrue,
					Reason: "AllocationSucceeded", Message: "ready",
					LastTransitionTime: metav1.Now(),
				}},
			},
		}
		var buf bytes.Buffer
		if err := codec.Encode(p, &buf); err != nil {
			t.Fatalf("encode seed pool: %v", err)
		}
		key := fmt.Sprintf("/ipam.miloapis.com/ippools/pool-%02d", i)
		if _, err := db.ExecContext(ctx,
			`INSERT INTO ipam_objects (key, kind, name, data, labels) VALUES ($1, 'IPPool', $2, $3, '{}')`,
			key, fmt.Sprintf("pool-%02d", i), buf.Bytes(),
		); err != nil {
			t.Fatalf("insert seed pool: %v", err)
		}
	}

	s := &Store{db: db, codec: codec, versioner: storage.APIObjectVersioner{}}

	// Protobuf encoder — the k8s aggregation clients request protobuf by
	// default, so exercise that serializer path (flagged as a race candidate)
	// alongside JSON.
	pbInfo, ok := runtime.SerializerInfoForMediaType(ipamapiserver.Codecs.SupportedMediaTypes(), "application/vnd.kubernetes.protobuf")
	if !ok {
		t.Fatal("no protobuf serializer registered")
	}
	pbEncoder := ipamapiserver.Codecs.EncoderForVersion(pbInfo.Serializer, ipamv1.SchemeGroupVersion)

	const goroutines = 64
	const iterations = 60
	var wg sync.WaitGroup
	errCh := make(chan error, goroutines)
	for g := 0; g < goroutines; g++ {
		useProto := g%2 == 0
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < iterations; i++ {
				list := &ipam.IPPoolList{}
				if err := s.GetList(ctx, "/ipam.miloapis.com/ippools",
					storage.ListOptions{Recursive: true, Predicate: storage.Everything},
					list,
				); err != nil {
					errCh <- fmt.Errorf("GetList: %w", err)
					return
				}
				if len(list.Items) != pools {
					errCh <- fmt.Errorf("got %d items, want %d", len(list.Items), pools)
					return
				}
				// Encode the internal list back to the wire form — this is the
				// convert_ipam_IPPoolList_To_v1alpha1 path the crash was in.
				var buf bytes.Buffer
				enc := runtime.Encoder(codec)
				if useProto {
					enc = pbEncoder
				}
				if err := enc.Encode(list, &buf); err != nil {
					errCh <- fmt.Errorf("encode list: %w", err)
					return
				}
			}
		}()
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Fatal(err)
	}
}

var _ = runtime.Object(nil)
