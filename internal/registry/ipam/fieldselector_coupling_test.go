package ipamregistry_test

// Field selectors are declared in two places that nothing forces to agree, and
// both have to be right for a selector to work:
//
//   - each registry's SelectableFields, which says what a LIST may filter on
//   - the scheme's field-label conversions, without which the apiserver rejects
//     the selector before any registry sees it
//
// They disagreed completely: four strategies declared eight selectors between
// them and the scheme registered none, so every one was rejected with
// "not a known field selector". This pins them against each other.
//
// It does NOT prove a selector works end to end — that gap is between the
// declaration and the served API, and only a real LIST closes it. See
// test/e2e/field-selectors, which issues one per selector against a live
// apiserver. This test is the cheap half: it catches the far more likely future
// mistake of adding a selectable field and forgetting the conversion.

import (
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"

	"go.miloapis.com/ipam/internal/registry/ipam/ipallocation"
	"go.miloapis.com/ipam/internal/registry/ipam/ipclaim"
	"go.miloapis.com/ipam/internal/registry/ipam/ipclass"
	"go.miloapis.com/ipam/internal/registry/ipam/ippool"
	"go.miloapis.com/ipam/pkg/apis/ipam"
	"go.miloapis.com/ipam/pkg/apis/ipam/v1alpha1"
)

func TestFieldLabelConversionsMatchSelectableFields(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := v1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("add v1alpha1 to scheme: %v", err)
	}

	// A populated object per kind, so SelectableFields returns every key it can
	// produce rather than only the ones that survive a zero value.
	cases := []struct {
		kind     string
		declared map[string]string
	}{
		{
			kind: "IPPool",
			declared: ippool.SelectableFields(&ipam.IPPool{
				ObjectMeta: metav1.ObjectMeta{Name: "p"},
				Spec: ipam.IPPoolSpec{
					IPFamily:      ipam.IPv4,
					ParentPoolRef: &ipam.LocalRef{Name: "parent"},
				},
				Status: ipam.IPPoolStatus{ScopeDigest: "d"},
			}),
		},
		{
			kind: "IPClass",
			declared: ipclass.SelectableFields(&ipam.IPClass{
				ObjectMeta: metav1.ObjectMeta{Name: "c"},
				Spec:       ipam.IPClassSpec{IPFamily: ipam.IPv4, ParentClassName: "parent"},
			}),
		},
		{
			kind: "IPAllocation",
			declared: ipallocation.SelectableFields(&ipam.IPAllocation{
				ObjectMeta: metav1.ObjectMeta{Name: "a", Namespace: "default"},
				Spec: ipam.IPAllocationSpec{
					IPFamily: ipam.IPv4,
					PoolRef:  ipam.LocalRef{Name: "pool"},
				},
				Status: ipam.IPAllocationStatus{ScopeDigest: "d"},
			}),
		},
		{
			kind: "IPClaim",
			declared: ipclaim.SelectableFields(&ipam.IPClaim{
				ObjectMeta: metav1.ObjectMeta{Name: "cl", Namespace: "default"},
				Spec:       ipam.IPClaimSpec{ClassName: "class"},
				Status:     ipam.IPClaimStatus{PoolRef: &ipam.LocalRef{Name: "pool"}},
			}),
		},
	}

	for _, tc := range cases {
		t.Run(tc.kind, func(t *testing.T) {
			gvk := v1alpha1.SchemeGroupVersion.WithKind(tc.kind)

			if len(tc.declared) == 0 {
				t.Fatalf("%s declares no selectable fields; the fixture is not populated enough to test anything", tc.kind)
			}

			for label := range tc.declared {
				if _, _, err := scheme.ConvertFieldLabel(gvk, label, "x"); err != nil {
					t.Errorf("%s declares %q selectable but the scheme rejects it, so a LIST filtering on it "+
						"fails with \"not a known field selector\" before any registry code runs: %v",
						tc.kind, label, err)
				}
			}

			// The other direction is the quieter failure: a label the scheme
			// converts but no registry populates is ACCEPTED and then matches
			// nothing, so a caller gets an empty list and no error.
			for _, label := range []string{"spec.thisFieldDoesNotExist", "status.alsoNot"} {
				if _, _, err := scheme.ConvertFieldLabel(gvk, label, "x"); err == nil {
					t.Errorf("%s: the scheme accepts %q, which no registry populates; "+
						"a LIST filtering on it would return an empty list rather than an error", tc.kind, label)
				}
			}

			// ObjectMeta selectors are what the apiserver permits by default and
			// what kubectl uses for `--field-selector metadata.name=`; losing
			// them by registering a conversion is a real regression, because
			// registering one REPLACES the default rather than extending it.
			if _, _, err := scheme.ConvertFieldLabel(gvk, "metadata.name", "x"); err != nil {
				t.Errorf("%s no longer accepts metadata.name: %v", tc.kind, err)
			}
		})
	}
}
