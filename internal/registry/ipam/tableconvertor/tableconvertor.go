// Package tableconvertor renders IPAM objects as server-side tables.
//
// Without one of these, every kind falls back to rest.NewDefaultTableConvertor,
// whose only columns are the name and an RFC3339 creation timestamp. That is
// what `kubectl get ippools` and `datumctl get ippools` print, and for this API
// it is close to useless: pool names are long and generated, so a listing shows
// a column of digests beside a column of timestamps and nothing that says what
// any of them hold.
//
// The columns here mirror what the milo-ipam plugin already prints for the same
// kinds, so the two surfaces agree, and age is rendered the way every other
// Kubernetes resource renders it.
package tableconvertor

import (
	"context"
	"net/http"

	"k8s.io/apimachinery/pkg/api/meta"
	metatable "k8s.io/apimachinery/pkg/api/meta/table"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	genericapirequest "k8s.io/apiserver/pkg/endpoints/request"
	"k8s.io/apiserver/pkg/registry/rest"
)

// New returns a TableConvertor that prints the given columns. cells is called
// once per object and must return one value per column, in order; name and age
// are supplied already formatted, age as the human-readable duration kubectl
// shows ("3d", "17m").
//
// resource names the kind in the error a non-object triggers, matching what the
// default convertor reports.
func New(
	resource schema.GroupResource,
	columns []metav1.TableColumnDefinition,
	cells func(obj runtime.Object, name, age string) []interface{},
) rest.TableConvertor {
	return &convertor{resource: resource, columns: columns, cells: cells}
}

type convertor struct {
	resource schema.GroupResource
	columns  []metav1.TableColumnDefinition
	cells    func(obj runtime.Object, name, age string) []interface{}
}

func (c *convertor) ConvertToTable(ctx context.Context, obj runtime.Object, tableOptions runtime.Object) (*metav1.Table, error) {
	var table metav1.Table

	rows, err := metatable.MetaToTableRow(obj, func(obj runtime.Object, _ metav1.Object, name, age string) ([]interface{}, error) {
		return c.cells(obj, name, age), nil
	})
	if err != nil {
		// Reported against the resource the request names, falling back to the
		// one this convertor was built for, as the default convertor does.
		resource := c.resource
		if info, ok := genericapirequest.RequestInfoFrom(ctx); ok && info.Resource != "" {
			resource = schema.GroupResource{Group: info.APIGroup, Resource: info.Resource}
		}
		return nil, errNotAcceptable{resource: resource}
	}
	table.Rows = rows

	if m, err := meta.ListAccessor(obj); err == nil {
		table.ResourceVersion = m.GetResourceVersion()
		table.Continue = m.GetContinue()
		table.RemainingItemCount = m.GetRemainingItemCount()
	} else if m, err := meta.CommonAccessor(obj); err == nil {
		table.ResourceVersion = m.GetResourceVersion()
	}

	if opt, ok := tableOptions.(*metav1.TableOptions); !ok || !opt.NoHeaders {
		table.ColumnDefinitions = c.columns
	}
	return &table, nil
}

// errNotAcceptable mirrors the error the default convertor returns for an
// object with no metadata, so a client sees the same 406 it always did.
type errNotAcceptable struct {
	resource schema.GroupResource
}

func (e errNotAcceptable) Error() string {
	return "the resource " + e.resource.String() + " does not support being converted to a Table"
}

func (e errNotAcceptable) Status() metav1.Status {
	return metav1.Status{
		Status:  metav1.StatusFailure,
		Code:    http.StatusNotAcceptable,
		Reason:  metav1.StatusReasonNotAcceptable,
		Message: e.Error(),
	}
}
