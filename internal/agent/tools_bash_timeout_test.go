package agent

import "testing"

func TestClampBashTimeoutSecsHonorsAdvertisedMax(t *testing.T) {
	tests := []struct {
		in, want int
	}{
		{35, 35}, // the session-7e86310 footgun: must not shrink to tools.timeout default
		{30, 30},
		{120, 120},
		{121, 120},
		{0, 1},
		{-5, 1},
	}
	for _, tt := range tests {
		if got := clampBashTimeoutSecs(tt.in); got != tt.want {
			t.Errorf("clampBashTimeoutSecs(%d) = %d, want %d", tt.in, got, tt.want)
		}
	}
}
