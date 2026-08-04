package ice

import "testing"

func TestBudgetConfig_Calculate(t *testing.T) {
	tests := []struct {
		name         string
		cfg          BudgetConfig
		promptTokens int
		wantTotal    int
		wantConv     int
		wantMemory   int
	}{
		{
			name: "normal allocation",
			cfg: BudgetConfig{
				NumCtx:          8192,
				SystemReserve:   1500,
				RecentReserve:   2000,
				ConversationPct: 0.65,
				MemoryPct:       0.35,
			},
			promptTokens: 500,
			// available = int(8192*0.75) - 1500 - 2000 - 500 = 6144 - 4000 = 2144
			wantTotal:  2144,
			wantConv:   1393, // int(2144 * 0.65) = 1393
			wantMemory: 750,  // int(2144 * 0.35) = 750
		},
		{
			name: "large prompt clamps to zero",
			cfg: BudgetConfig{
				NumCtx:          8192,
				SystemReserve:   1500,
				RecentReserve:   2000,
				ConversationPct: 0.65,
				MemoryPct:       0.35,
			},
			promptTokens: 99999,
			wantTotal:    0,
			wantConv:     0,
			wantMemory:   0,
		},
		{
			name: "exact boundary available is zero",
			cfg: BudgetConfig{
				NumCtx:          8192,
				SystemReserve:   1500,
				RecentReserve:   2000,
				ConversationPct: 0.65,
				MemoryPct:       0.35,
			},
			// int(8192*0.75) - 1500 - 2000 = 2644
			promptTokens: 2644,
			wantTotal:    0,
			wantConv:     0,
			wantMemory:   0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b := tt.cfg.Calculate(tt.promptTokens)
			if b.Total != tt.wantTotal {
				t.Errorf("Total = %d, want %d", b.Total, tt.wantTotal)
			}
			if b.Conversation != tt.wantConv {
				t.Errorf("Conversation = %d, want %d", b.Conversation, tt.wantConv)
			}
			if b.Memory != tt.wantMemory {
				t.Errorf("Memory = %d, want %d", b.Memory, tt.wantMemory)
			}
			if b.System != tt.cfg.SystemReserve {
				t.Errorf("System = %d, want %d", b.System, tt.cfg.SystemReserve)
			}
			if b.Recent != tt.cfg.RecentReserve {
				t.Errorf("Recent = %d, want %d", b.Recent, tt.cfg.RecentReserve)
			}
		})
	}
}

func TestBudgetConfig_CalculatePromptRemainder(t *testing.T) {
	cfg := DefaultBudgetConfig(16_384)

	budget := cfg.CalculatePromptRemainder(10_000)
	// The host count already includes system and recent-message tokens:
	// int(16384*0.75) - 10000 = 2288.
	if budget.Total != 2_288 {
		t.Fatalf("Total = %d, want 2288", budget.Total)
	}
	if budget.Conversation != 1487 {
		t.Errorf("Conversation = %d, want 1487", budget.Conversation)
	}
	if budget.Memory != 800 {
		t.Errorf("Memory = %d, want 800", budget.Memory)
	}

	exhausted := cfg.CalculatePromptRemainder(12_288)
	if exhausted.Total != 0 || exhausted.Conversation != 0 || exhausted.Memory != 0 {
		t.Fatalf("exhausted budget = %#v, want zero optional allocation", exhausted)
	}
}

func TestBudgetConfig_NegativePromptDoesNotIncreaseAllocation(t *testing.T) {
	cfg := DefaultBudgetConfig(8_192)
	if got, want := cfg.Calculate(-100).Total, cfg.Calculate(0).Total; got != want {
		t.Fatalf("legacy negative prompt allocation = %d, want %d", got, want)
	}
	if got, want := cfg.CalculatePromptRemainder(-100).Total, cfg.CalculatePromptRemainder(0).Total; got != want {
		t.Fatalf("authoritative negative prompt allocation = %d, want %d", got, want)
	}
}

func TestEstimateTokens(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  int
	}{
		{
			name:  "len/4 heuristic",
			input: "hello world",
			want:  2, // 11/4 = 2
		},
		{
			name:  "single char clamps to 1",
			input: "a",
			want:  1, // 1/4 = 0, clamp to 1
		},
		{
			name:  "empty string",
			input: "",
			want:  0,
		},
		{
			name:  "exactly 4 chars",
			input: "abcd",
			want:  1, // 4/4 = 1
		},
		{
			name:  "three chars clamps to 1",
			input: "abc",
			want:  1, // 3/4 = 0, clamp to 1
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := estimateTokens(tt.input)
			if got != tt.want {
				t.Errorf("estimateTokens(%q) = %d, want %d", tt.input, got, tt.want)
			}
		})
	}
}
