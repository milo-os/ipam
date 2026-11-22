package main

import (
	"strings"
	"testing"
	"time"

	"github.com/spf13/pflag"
)

// The sweeper ships in observe mode once a minute, and its flags refuse what
// would either spin or do something nobody asked for.
func TestRetentionSweepFlags(t *testing.T) {
	for _, tc := range []struct {
		name    string
		args    []string
		wantErr string
	}{
		{name: "defaults"},
		{name: "disabled", args: []string{"--retention-sweep-interval=0"}},
		{name: "at the floor", args: []string{"--retention-sweep-interval=1s", "--retention-sweep-mode=enforce"}},
		{name: "below the floor", args: []string{"--retention-sweep-interval=500ms"}, wantErr: "at least 1s"},
		{name: "negative", args: []string{"--retention-sweep-interval=-1m"}, wantErr: "at least 1s"},
		{name: "unknown mode", args: []string{"--retention-sweep-mode=dry-run"}, wantErr: "retention-sweep-mode"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			o := NewIPAMServerOptions()
			fs := pflag.NewFlagSet("serve", pflag.ContinueOnError)
			o.AddFlags(fs)
			if err := fs.Parse(append([]string{"--postgres-dsn=postgres://x"}, tc.args...)); err != nil {
				t.Fatalf("parse: %v", err)
			}
			err := o.Validate()
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("Validate: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("Validate = %v, want an error mentioning %q", err, tc.wantErr)
			}
		})
	}

	o := NewIPAMServerOptions()
	if o.RetentionSweepInterval != time.Minute || o.RetentionSweepMode != "observe" {
		t.Errorf("defaults = %s/%s, want 1m/observe", o.RetentionSweepInterval, o.RetentionSweepMode)
	}
}
