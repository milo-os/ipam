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

// --- IPPrefixClass ---

func (in *IPPrefixClass) Marshal() ([]byte, error)        { return json.Marshal(in) }
func (in *IPPrefixClass) Unmarshal(data []byte) error     { return json.Unmarshal(data, in) }
func (in *IPPrefixClassList) Marshal() ([]byte, error)    { return json.Marshal(in) }
func (in *IPPrefixClassList) Unmarshal(data []byte) error { return json.Unmarshal(data, in) }

// --- IPPrefix ---

func (in *IPPrefix) Marshal() ([]byte, error)        { return json.Marshal(in) }
func (in *IPPrefix) Unmarshal(data []byte) error     { return json.Unmarshal(data, in) }
func (in *IPPrefixList) Marshal() ([]byte, error)    { return json.Marshal(in) }
func (in *IPPrefixList) Unmarshal(data []byte) error { return json.Unmarshal(data, in) }

// --- IPPrefixClaim ---

func (in *IPPrefixClaim) Marshal() ([]byte, error)        { return json.Marshal(in) }
func (in *IPPrefixClaim) Unmarshal(data []byte) error     { return json.Unmarshal(data, in) }
func (in *IPPrefixClaimList) Marshal() ([]byte, error)    { return json.Marshal(in) }
func (in *IPPrefixClaimList) Unmarshal(data []byte) error { return json.Unmarshal(data, in) }

// --- IPAddress ---

func (in *IPAddress) Marshal() ([]byte, error)        { return json.Marshal(in) }
func (in *IPAddress) Unmarshal(data []byte) error     { return json.Unmarshal(data, in) }
func (in *IPAddressList) Marshal() ([]byte, error)    { return json.Marshal(in) }
func (in *IPAddressList) Unmarshal(data []byte) error { return json.Unmarshal(data, in) }

// --- IPAddressClaim ---

func (in *IPAddressClaim) Marshal() ([]byte, error)        { return json.Marshal(in) }
func (in *IPAddressClaim) Unmarshal(data []byte) error     { return json.Unmarshal(data, in) }
func (in *IPAddressClaimList) Marshal() ([]byte, error)    { return json.Marshal(in) }
func (in *IPAddressClaimList) Unmarshal(data []byte) error { return json.Unmarshal(data, in) }

