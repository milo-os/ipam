package v1alpha1

import (
	"fmt"

	"k8s.io/apimachinery/pkg/runtime"
)

// Field-label conversions.
//
// A registry's SelectableFields says which fields a LIST may filter on. That is
// necessary and not sufficient: the apiserver never reaches it unless the scheme
// carries a field-label conversion for the kind, because a selector arrives in
// the VERSIONED form and must be converted to the internal one before the
// storage layer sees it. Without a conversion the default permits only
// metadata.name and metadata.namespace, and rejects every other selector at
// request time.
//
// So this file, each strategy's SelectableFields, and the expression indexes in
// migration 002 have to agree. Nothing in the type system makes them.
// TestFieldLabelConversionsMatchSelectableFields pins the first two against each
// other, and the e2e suite `field-selectors` issues a real LIST per selector
// against a live apiserver.
//
// The two failure directions are not symmetric. A field declared selectable but
// not converted is rejected by the API. A field converted but not selectable is
// accepted, and then matches nothing.
//
// metadata.namespace is permitted for the namespaced kinds only. On a
// cluster-scoped kind it filters on nothing a caller could have meant.

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
			// Pass through unchanged: the internal types mirror the versioned
			// ones field for field, so every path here is identical in both. A
			// field whose json tag diverges from its internal name must be
			// translated instead, or its selector matches nothing.
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
			"spec.source.project",
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
