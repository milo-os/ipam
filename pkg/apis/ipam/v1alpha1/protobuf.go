package v1alpha1

// This file provides minimal protobuf Marshal/Unmarshal methods for the IPAM
// types so they can be served to clients that request the protobuf content
// type (e.g., the kube-apiserver's namespace garbage collector).
//
// The k8s.io/apimachinery protobuf serializer wraps objects that implement
// Marshal() ([]byte, error) in a runtime.Unknown envelope. We delegate to
// JSON encoding since our types don't have generated protobuf definitions.
// This is the standard approach for aggregated apiservers that don't want
// to generate protobuf bindings.

import "encoding/json"

// --- IPPool ---

func (in *IPPool) Marshal() ([]byte, error)        { return json.Marshal(in) }
func (in *IPPool) Unmarshal(data []byte) error     { return json.Unmarshal(data, in) }
func (in *IPPoolList) Marshal() ([]byte, error)    { return json.Marshal(in) }
func (in *IPPoolList) Unmarshal(data []byte) error { return json.Unmarshal(data, in) }

// --- IPAllocation ---

func (in *IPAllocation) Marshal() ([]byte, error)        { return json.Marshal(in) }
func (in *IPAllocation) Unmarshal(data []byte) error     { return json.Unmarshal(data, in) }
func (in *IPAllocationList) Marshal() ([]byte, error)    { return json.Marshal(in) }
func (in *IPAllocationList) Unmarshal(data []byte) error { return json.Unmarshal(data, in) }

// --- IPClaim ---

func (in *IPClaim) Marshal() ([]byte, error)        { return json.Marshal(in) }
func (in *IPClaim) Unmarshal(data []byte) error     { return json.Unmarshal(data, in) }
func (in *IPClaimList) Marshal() ([]byte, error)    { return json.Marshal(in) }
func (in *IPClaimList) Unmarshal(data []byte) error { return json.Unmarshal(data, in) }
