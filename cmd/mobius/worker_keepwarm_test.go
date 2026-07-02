package main

import (
	"testing"
	"time"

	"github.com/deepnoodle-ai/mobius/mobius"
)

func TestParseKeepWarm(t *testing.T) {
	cases := []struct {
		in      string
		want    time.Duration
		wantErr bool
	}{
		{in: "", want: 0},
		{in: "on-demand", want: mobius.KeepWarmOnDemand},
		{in: "OnDemand", want: mobius.KeepWarmOnDemand},
		{in: "forever", want: mobius.KeepWarmForever},
		{in: "lifetime", want: mobius.KeepWarmForever},
		{in: "always", want: mobius.KeepWarmForever},
		// Compat with the old MOBIUS_WORKER_KEEP_WARM boolean toggle.
		{in: "true", want: mobius.KeepWarmForever},
		{in: "1", want: mobius.KeepWarmForever},
		{in: "false", want: 0},
		{in: "5m", want: 5 * time.Minute},
		{in: "90s", want: 90 * time.Second},
		// An explicit zero window means on-demand.
		{in: "0s", want: mobius.KeepWarmOnDemand},
		{in: "banana", wantErr: true},
		{in: "-5m", wantErr: true},
	}
	for _, tc := range cases {
		got, err := parseKeepWarm(tc.in)
		if tc.wantErr {
			if err == nil {
				t.Errorf("parseKeepWarm(%q): expected error, got %v", tc.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("parseKeepWarm(%q): unexpected error %v", tc.in, err)
			continue
		}
		if got != tc.want {
			t.Errorf("parseKeepWarm(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

func TestKeepWarmDescription(t *testing.T) {
	if got := keepWarmDescription(0); got != "default" {
		t.Errorf("keepWarmDescription(0) = %q", got)
	}
	if got := keepWarmDescription(mobius.KeepWarmOnDemand); got != "on-demand" {
		t.Errorf("on-demand = %q", got)
	}
	if got := keepWarmDescription(mobius.KeepWarmForever); got != "forever" {
		t.Errorf("forever = %q", got)
	}
	if got := keepWarmDescription(5 * time.Minute); got != "5m0s" {
		t.Errorf("5m = %q", got)
	}
}
