package ipam

import (
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// GroupName is the group name for the IPAM API.
const GroupName = "ipam.miloapis.com"

// SchemeGroupVersion is the group version used to register these objects.
// The internal version uses runtime.APIVersionInternal.
var SchemeGroupVersion = schema.GroupVersion{Group: GroupName, Version: runtime.APIVersionInternal}

var (
	// SchemeBuilder collects functions that add types to a scheme.
	SchemeBuilder = runtime.NewSchemeBuilder(addKnownTypes)
	// AddToScheme adds the types in this group to the given scheme.
	AddToScheme = SchemeBuilder.AddToScheme
)

// Kind takes an unqualified kind and returns a Group-qualified GroupKind.
func Kind(kind string) schema.GroupKind {
	return SchemeGroupVersion.WithKind(kind).GroupKind()
}

// Resource takes an unqualified resource and returns a Group-qualified GroupResource.
func Resource(resource string) schema.GroupResource {
	return SchemeGroupVersion.WithResource(resource).GroupResource()
}

func addKnownTypes(scheme *runtime.Scheme) error {
	scheme.AddKnownTypes(SchemeGroupVersion,
		&IPPool{}, &IPPoolList{},
		&IPAllocation{}, &IPAllocationList{},
		&IPClaim{}, &IPClaimList{},
		&IPClass{}, &IPClassList{},
	)
	return nil
}
