package v1alpha1

import (
	"fmt"

	"k8s.io/apimachinery/pkg/runtime"
)

// Field-label conversions.
//
// # Why this file has to exist
//
// A registry's SelectableFields says which fields a LIST may filter on. It is
// necessary and not sufficient: the apiserver will not even reach it unless the
// scheme carries a field-label conversion for the kind, because a field selector
// arrives in the *versioned* form and has to be converted to the internal one
// before the storage layer sees it. With no conversion registered, the default
// permits `metadata.name` and `metadata.namespace` and rejects everything else.
//
// Nothing was registered here at all, so every selector all four strategies
// declared was rejected at request time:
//
//	$ kubectl get ippools --field-selector spec.ipFamily=IPv6
//	Error from server (BadRequest): "spec.ipFamily" is not a known field selector:
//	only "metadata.name", "metadata.namespace"
//
// Three coordinated pieces of intent pointed the other way — the SelectableFields
// declarations, the FieldIndex values in each strategy, and the SQL expression
// indexes in migration 002, whose comment advertises one of these queries by
// name. The indexes were maintained on every write and served no read. This
// makes the advertised behaviour real rather than deleting the four pieces.
//
// # Keeping this in step with SelectableFields
//
// The two lists must agree, and nothing in the type system makes them. A field
// declared selectable but not converted is rejected by the API; a field
// converted but not selectable is accepted and then silently matches nothing,
// which is the worse direction. TestFieldLabelConversionsMatchSelectableFields
// pins them against each other, and the e2e suite `field-selectors` runs a real
// LIST per selector against a live apiserver — the only check on the far side of
// the declaration/served-API gap this file is about.
//
// ObjectMeta labels are permitted for every kind. `metadata.namespace` is
// included for the namespaced kinds only; on a cluster-scoped kind it is not a
// meaningful filter and accepting it would answer a question the caller cannot
// have meant.

// fieldLabelConversion returns a conversion func accepting the given labels
// plus the ObjectMeta ones, and rejecting everything else with the same shape of
// error the apiserver's default produces.
func fieldLabelConversion(namespaced bool, allowed ...string) func(string, string) (string, string, error) {
	permitted := map[string]struct{}{"metadata.name": {}}
	if namespaced {
		permitted["metadata.namespace"] = struct{}{}
	}
	for _, f := range allowed {
		permitted[f] = struct{}{}
	}
	return func(label, value string) (string, string, error) {
		if _, ok := permitted[label]; ok {
			// The versioned and internal field paths are identical for every
			// selector here — the internal types mirror the versioned ones field
			// for field — so the label passes through unchanged. A field whose
			// json tag ever diverges from its internal name must be translated
			// here rather than passed through, or the selector would silently
			// filter on nothing.
			return label, value, nil
		}
		return "", "", fmt.Errorf("field label not supported: %s", label)
	}
}

func addFieldLabelConversions(scheme *runtime.Scheme) error {
	for kind, conv := range map[string]func(string, string) (string, string, error){
		// Cluster-scoped.
		"IPPool": fieldLabelConversion(false,
			"spec.ipFamily",
			"spec.parentPoolRef.name",
			"status.scopeDigest",
		),
		"IPClass": fieldLabelConversion(false,
			"spec.ipFamily",
			"spec.parentClassName",
		),
		// Namespaced.
		"IPAllocation": fieldLabelConversion(true,
			"spec.ipFamily",
			"spec.poolRef.name",
			"status.scopeDigest",
		),
		"IPClaim": fieldLabelConversion(true,
			"spec.className",
			"status.poolRef.name",
		),
	} {
		if err := scheme.AddFieldLabelConversionFunc(SchemeGroupVersion.WithKind(kind), conv); err != nil {
			return err
		}
	}
	return nil
}
