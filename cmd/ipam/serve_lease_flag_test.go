package main

import (
	"strings"
	"testing"
	"time"
)

// The floor exists because every sweep pass takes a row lock on each pool
// holding retained allocations, and claims against those pools wait behind it.
// A sweeper tuned to milliseconds would turn a background job into a source of
// allocation latency.
func TestLeaseSweepIntervalValidation(t *testing.T) {
	tests := []struct {
		name     string
		interval time.Duration
		wantErr  string
	}{
		{
			name:     "the default is accepted",
			interval: defaultLeaseSweepInterval,
		},
		{
			// The value a chainsaw suite needs: short enough to observe the
			// Expiring grace window inside a minute rather than a quarter hour.
			name:     "a test-length interval is accepted",
			interval: 5 * time.Second,
		},
		{
			name:     "exactly the floor is accepted",
			interval: minLeaseSweepInterval,
		},
		{
			// Zero is a real setting, not an unset one — it turns the sweeper
			// off — so the floor must not catch it.
			name:     "zero disables the sweeper and is not floored",
			interval: 0,
		},
		{
			name:     "below the floor is rejected",
			interval: 10 * time.Millisecond,
			wantErr:  "below the 1s minimum",
		},
		{
			name:     "negative is rejected",
			interval: -time.Second,
			wantErr:  "must not be negative",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			o := &IPAMServerOptions{
				PostgresDSN:        "host=localhost",
				PlatformProject:    "milo-platform",
				LeaseSweepInterval: tt.interval,
			}
			err := o.Validate()
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("expected %s to be accepted, got %v", tt.interval, err)
				}
				return
			}
			if err == nil {
				t.Fatalf("expected %s to be rejected", tt.interval)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("error = %v, want it to contain %q", err, tt.wantErr)
			}
		})
	}
}

// Rejected rather than silently clamped: an operator who asked for 10ms and got
// 1s would be running something other than what they configured, and would find
// out from a latency graph rather than from the flag.
func TestBelowFloorIsRejectedNotClamped(t *testing.T) {
	o := &IPAMServerOptions{
		PostgresDSN:        "host=localhost",
		PlatformProject:    "milo-platform",
		LeaseSweepInterval: 10 * time.Millisecond,
	}
	if err := o.Validate(); err == nil {
		t.Fatal("expected rejection")
	}
	if o.LeaseSweepInterval != 10*time.Millisecond {
		t.Errorf("Validate mutated the option to %s; it must reject, not clamp", o.LeaseSweepInterval)
	}
}

// The default must be what NewIPAMServerOptions hands out, or the flag's
// documented default and the actual one diverge.
func TestDefaultLeaseSweepInterval(t *testing.T) {
	if got := NewIPAMServerOptions().LeaseSweepInterval; got != defaultLeaseSweepInterval {
		t.Errorf("default = %s, want %s", got, defaultLeaseSweepInterval)
	}
}
