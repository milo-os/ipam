package ipclaim

import (
	"net/http"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"go.miloapis.com/ipam/pkg/apis/ipam"
	"go.miloapis.com/ipam/pkg/ipamerrors"
)

func addressClaim(name, address string, prefixLength *int32) *ipam.IPClaim {
	return &ipam.IPClaim{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Spec:       ipam.IPClaimSpec{ClassName: "standard", Address: address, PrefixLength: prefixLength},
	}
}

// #183: the claim is bound to exactly the address it named, not to the next
// free block.
func TestCreateBindsTheRequestedAddress(t *testing.T) {
	r, _ := newPostgresREST(t)

	obj, err := r.Create(claimCtx(testProject), addressClaim("named", "10.0.7.0", nil), nil, &metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if got := obj.(*ipam.IPClaim).Status.AllocatedCIDR; got != "10.0.7.0/24" {
		t.Errorf("allocatedCIDR = %q, want the requested 10.0.7.0/24", got)
	}
	if got := obj.(*ipam.IPClaim).Status.Address; got != "" {
		t.Errorf("status.address = %q, want empty for a /24", got)
	}

	// A CIDR states its own length, ahead of the class default of /24.
	obj, err = r.Create(claimCtx(testProject), addressClaim("cidr", "10.0.9.16/28", nil), nil, &metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("Create CIDR: %v", err)
	}
	c := obj.(*ipam.IPClaim)
	if c.Status.AllocatedCIDR != "10.0.9.16/28" {
		t.Errorf("allocatedCIDR = %q, want 10.0.9.16/28", c.Status.AllocatedCIDR)
	}
	if c.Spec.PrefixLength == nil || *c.Spec.PrefixLength != 28 {
		t.Errorf("spec.prefixLength = %v, want 28 taken from the CIDR", c.Spec.PrefixLength)
	}
}

// status.address carries the bare address of every single-address claim,
// whether or not it named one.
func TestCreateReportsTheSingleAddress(t *testing.T) {
	r, _ := newPostgresREST(t)
	host := int32(32)

	obj, err := r.Create(claimCtx(testProject), addressClaim("named-host", "10.0.3.9", &host), nil, &metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("Create named host: %v", err)
	}
	if c := obj.(*ipam.IPClaim); c.Status.AllocatedCIDR != "10.0.3.9/32" || c.Status.Address != "10.0.3.9" {
		t.Errorf("named host = %q / %q, want 10.0.3.9/32 / 10.0.3.9", c.Status.AllocatedCIDR, c.Status.Address)
	}

	obj, err = r.Create(claimCtx(testProject), addressClaim("auto-host", "", &host), nil, &metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("Create automatic host: %v", err)
	}
	c := obj.(*ipam.IPClaim)
	if c.Status.Address == "" || c.Status.Address+"/32" != c.Status.AllocatedCIDR {
		t.Errorf("automatic host = %q / %q, want status.address to be the /32's address", c.Status.AllocatedCIDR, c.Status.Address)
	}
}

func TestCreateRefusesARequestedAddress(t *testing.T) {
	r, db := newPostgresREST(t)
	if _, err := r.Create(claimCtx(testProject), addressClaim("holder", "10.0.1.0", nil), nil, &metav1.CreateOptions{}); err != nil {
		t.Fatalf("seed holder: %v", err)
	}
	holderAlloc := allocationNameFor("default", "holder")

	tests := []struct {
		name       string
		claim      *ipam.IPClaim
		wantCode   int32
		wantReason ipamerrors.Reason
	}{
		{"held", addressClaim("dup", "10.0.1.0", nil), http.StatusConflict, ipamerrors.ReasonAddressInUse},
		{"outside every pool", addressClaim("outside", "10.1.0.0", nil), http.StatusBadRequest, ipamerrors.ReasonAddressOutsidePool},
		{"unaligned", addressClaim("unaligned", "10.0.2.5", nil), http.StatusBadRequest, ipamerrors.ReasonAddressUnaligned},
		{"wrong family", addressClaim("family", "fd00::", nil), http.StatusBadRequest, ipamerrors.ReasonFamilyMismatch},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			before := countRows(t, db, `SELECT COUNT(*) FROM ipam_cidr_allocations`)
			_, err := r.Create(claimCtx(testProject), tc.claim, nil, &metav1.CreateOptions{})
			if err == nil {
				t.Fatal("Create succeeded")
			}
			status, ok := err.(apierrors.APIStatus)
			if !ok || status.Status().Code != tc.wantCode {
				t.Fatalf("error %v, want HTTP %d", err, tc.wantCode)
			}
			if got := ipamerrors.ReasonFor(err); got != tc.wantReason {
				t.Errorf("reason = %q, want %q", got, tc.wantReason)
			}
			if apierrors.IsAlreadyExists(err) {
				t.Error("refusal reads as AlreadyExists")
			}
			if after := countRows(t, db, `SELECT COUNT(*) FROM ipam_cidr_allocations`); after != before {
				t.Errorf("refusal wrote %d allocation rows", after-before)
			}
			if n := countRows(t, db, `SELECT COUNT(*) FROM ipam_objects WHERE kind = 'IPClaim' AND name = $1`, tc.claim.Name); n != 0 {
				t.Errorf("refused claim %q was stored", tc.claim.Name)
			}
		})
	}

	_, err := r.Create(claimCtx(testProject), addressClaim("dup2", "10.0.1.0", nil), nil, &metav1.CreateOptions{})
	holder, ok := ipamerrors.AddressHolder(err)
	if !ok || holder.Name != holderAlloc || holder.Namespace != "default" || holder.Retained {
		t.Errorf("AddressHolder = %+v, %v; want default/%s, live", holder, ok, holderAlloc)
	}
}

// A claim created with generateName is named by the server, as the generic
// Store would name it. Before, Create skipped the generation and BeforeCreate
// refused the claim with "metadata.name was not generated".
func TestCreateGeneratesANameFromGenerateName(t *testing.T) {
	r, _ := newPostgresREST(t)
	claim := &ipam.IPClaim{
		ObjectMeta: metav1.ObjectMeta{GenerateName: "dns-", Namespace: "default"},
		Spec:       ipam.IPClaimSpec{ClassName: "standard", Address: "10.0.53.0"},
	}
	obj, err := r.Create(claimCtx(testProject), claim, nil, &metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	got := obj.(*ipam.IPClaim)
	if len(got.Name) <= len("dns-") || got.Name[:4] != "dns-" {
		t.Errorf("name = %q, want one generated from dns-", got.Name)
	}
	if got.Status.AllocatedCIDR != "10.0.53.0/24" {
		t.Errorf("allocatedCIDR = %q, want 10.0.53.0/24", got.Status.AllocatedCIDR)
	}
}
