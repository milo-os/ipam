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

// --- ASNPoolClass ---

func (in *ASNPoolClass) Marshal() ([]byte, error)        { return json.Marshal(in) }
func (in *ASNPoolClass) Unmarshal(data []byte) error     { return json.Unmarshal(data, in) }
func (in *ASNPoolClassList) Marshal() ([]byte, error)    { return json.Marshal(in) }
func (in *ASNPoolClassList) Unmarshal(data []byte) error { return json.Unmarshal(data, in) }

// --- ASNPool ---

func (in *ASNPool) Marshal() ([]byte, error)        { return json.Marshal(in) }
func (in *ASNPool) Unmarshal(data []byte) error     { return json.Unmarshal(data, in) }
func (in *ASNPoolList) Marshal() ([]byte, error)    { return json.Marshal(in) }
func (in *ASNPoolList) Unmarshal(data []byte) error { return json.Unmarshal(data, in) }

// --- ASNClaim ---

func (in *ASNClaim) Marshal() ([]byte, error)        { return json.Marshal(in) }
func (in *ASNClaim) Unmarshal(data []byte) error     { return json.Unmarshal(data, in) }
func (in *ASNClaimList) Marshal() ([]byte, error)    { return json.Marshal(in) }
func (in *ASNClaimList) Unmarshal(data []byte) error { return json.Unmarshal(data, in) }
