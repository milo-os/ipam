package ipclass

import (
	"context"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/validation/field"

	"go.miloapis.com/ipam/internal/allocator"
	"go.miloapis.com/ipam/internal/tenant"
	"go.miloapis.com/ipam/pkg/apis/ipam"
)

// validateOfferAgreement rejects a class that would join a pool already serving
// a class with a different uniqueWithin.
//
// One pool split across two address spaces hands the same address to two
// holders. The pool's own admission catches that from the pool's side; this
// catches it when the class is what arrives, because a pool may name a class
// that does not exist yet, and a class that is deleted and recreated keeps
// every pool that named it.
//
// Only creation needs the check. spec.uniqueWithin is immutable, so a class
// already serving a pool cannot drift into disagreement with the classes
// beside it.
func (s *IPClassStorage) validateOfferAgreement(ctx context.Context, obj runtime.Object) error {
	class, ok := obj.(*ipam.IPClass)
	if !ok {
		return fmt.Errorf("expected *ipam.IPClass, got %T", obj)
	}
	// A reference states no policy, and discovery keys on the definition's own
	// name, so no pool reaches a class through a reference. A class with a
	// parent draws from what its ancestry provisions, not from the pools
	// offering it.
	if class.Spec.Source != nil || class.Spec.ParentClassName != "" {
		return nil
	}

	tx, err := s.db.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin class offer validation transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	poolName, d, err := allocator.ClassJoinsDisagreement(ctx, tx, tenant.FromContext(ctx).Name,
		allocator.ClassSpace{Name: class.Name, UniqueWithin: class.Spec.UniqueWithin})
	if err != nil {
		return fmt.Errorf("resolve the pools offering IPClass %q: %w", class.Name, err)
	}
	if d == nil {
		return nil
	}
	return apierrors.NewInvalid(ipam.Kind("IPClass"), class.Name, field.ErrorList{
		field.Invalid(field.NewPath("spec", "uniqueWithin"), class.Spec.UniqueWithin,
			fmt.Sprintf("IPPool %q already offers itself to this class: %s", poolName, d.Error())),
	})
}
