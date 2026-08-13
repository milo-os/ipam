package registryerrors

import (
	"net/http"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
)

// A claim names a class, never a pool, so a 507 that does not name the pool
// leaves the caller with nothing to widen.
func TestPoolExhaustedNamesThePool(t *testing.T) {
	err := NewPoolExhausted("us-east-v4")

	if got := err.ErrStatus.Code; got != http.StatusInsufficientStorage {
		t.Errorf("code = %d, want %d", got, http.StatusInsufficientStorage)
	}
	if err.ErrStatus.Details == nil {
		t.Fatal("507 carries no Details; the caller cannot tell which pool ran out")
	}
	if got := err.ErrStatus.Details.Name; got != "us-east-v4" {
		t.Errorf("details.name = %q, want the exhausted pool", got)
	}
	// The message is what kubectl prints, so the name belongs there too.
	if got := err.ErrStatus.Message; got == "" || !contains(got, "us-east-v4") {
		t.Errorf("message = %q, want it to name the pool", got)
	}
}

// The helper must keep producing something apierrors recognises as 507, not
// merely a status that happens to carry the number.
func TestPoolExhaustedIsRecognisedByApierrors(t *testing.T) {
	if !apierrors.IsUnexpectedServerError(NewPoolExhausted("p")) &&
		apierrors.APIStatus(NewPoolExhausted("p")).Status().Code != http.StatusInsufficientStorage {
		t.Error("507 is not readable through the apierrors interfaces")
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
