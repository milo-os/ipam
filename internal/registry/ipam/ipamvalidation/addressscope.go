package ipamvalidation

import (
	"k8s.io/apimachinery/pkg/util/validation/field"

	"go.miloapis.com/ipam/pkg/apis/ipam"
)

func AddressScope(s ipam.AddressScope, path *field.Path) field.ErrorList {
	switch s {
	case "", ipam.AddressScopePublic, ipam.AddressScopePrivate:
		return nil
	}
	return field.ErrorList{field.NotSupported(path, s,
		[]string{string(ipam.AddressScopePublic), string(ipam.AddressScopePrivate)})}
}
