package agent

import (
	"testing"

	"github.com/abdul-hamid-achik/sonar/internal/tools"
)

func TestClampBashTimeoutSecsHonorsAdvertisedMax(t *testing.T) {
	tests := []struct {
		in, want int
	}{
		{35, 35}, // the session-7e86310 footgun: must not shrink to tools.timeout default
		{30, 30},
		{tools.BashTimeoutMaxSecs, tools.BashTimeoutMaxSecs},
		{tools.BashTimeoutMaxSecs + 1, tools.BashTimeoutMaxSecs},
		{0, tools.BashTimeoutMinSecs},
		{-5, tools.BashTimeoutMinSecs},
	}
	for _, tt := range tests {
		if got := clampBashTimeoutSecs(tt.in); got != tt.want {
			t.Errorf("clampBashTimeoutSecs(%d) = %d, want %d", tt.in, got, tt.want)
		}
	}
}
