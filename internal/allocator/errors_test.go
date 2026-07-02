package allocator

import (
	"errors"
	"strings"
	"testing"
)

// TestUserErrorHidesSentinelText verifies that the user-facing wrappers still
// match their sentinels via errors.Is (so the registry can map them to the
// right HTTP status) but do NOT leak the internal "ipam: ..." sentinel text
// into the message shown to API clients.
func TestUserErrorHidesSentinelText(t *testing.T) {
	cases := []struct {
		name     string
		err      error
		sentinel error
	}{
		{"family mismatch", familyMismatchError("this claim asks for an IPv4 address, but pool \"p\" hands out IPv6 addresses."), ErrFamilyMismatch},
		{"prefix length", prefixLengthError("a prefix length of /40 can't be allocated from an IPv4 pool; choose a length between /0 and /32."), ErrPrefixLengthOutOfRange},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if !errors.Is(tc.err, tc.sentinel) {
				t.Errorf("errors.Is should match the sentinel so callers can map the status")
			}
			if strings.Contains(tc.err.Error(), "ipam:") {
				t.Errorf("user-facing message leaks internal sentinel text: %q", tc.err.Error())
			}
		})
	}
}
