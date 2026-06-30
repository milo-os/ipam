package main

import (
	"testing"

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apiserver/pkg/admission"

	ipamapiserver "go.miloapis.com/ipam/internal/apiserver"
)

// fakeConvertorPlugin satisfies admission.Interface (via the embedded Handler)
// and objectConvertorSetter, so the initializer should inject into it.
type fakeConvertorPlugin struct {
	*admission.Handler
	got runtime.ObjectConvertor
}

func (f *fakeConvertorPlugin) SetObjectConvertor(c runtime.ObjectConvertor) { f.got = c }

// plainPlugin satisfies admission.Interface but NOT objectConvertorSetter; the
// initializer must tolerate it without panicking.
type plainPlugin struct{ *admission.Handler }

// TestObjectConvertorInitializer guards the wiring that fixes quota enforcement
// for IPAM's aggregated (internal-typed) objects: without SetObjectConvertor the
// quota plugin renders the ResourceClaim name from an internal object that has
// no "metadata" key and denies every create with an internal error.
func TestObjectConvertorInitializer(t *testing.T) {
	if ipamapiserver.Scheme == nil {
		t.Fatal("ipamapiserver.Scheme is nil")
	}

	p := &fakeConvertorPlugin{Handler: admission.NewHandler(admission.Create)}
	objectConvertorInitializer{convertor: ipamapiserver.Scheme}.Initialize(p)
	if p.got == nil {
		t.Fatal("initializer did not call SetObjectConvertor")
	}

	// Must not panic for a plugin that doesn't implement the setter.
	objectConvertorInitializer{convertor: ipamapiserver.Scheme}.Initialize(
		&plainPlugin{Handler: admission.NewHandler(admission.Create)},
	)
}
