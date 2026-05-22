package ipam

// This file provides minimal protobuf Marshal/Unmarshal methods for the IPAM
// internal types. See v1alpha1/protobuf.go for rationale.

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
