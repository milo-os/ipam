package ipam

// This file provides minimal protobuf Marshal/Unmarshal methods for the IPAM
// internal types. See v1alpha1/protobuf.go for rationale.

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


