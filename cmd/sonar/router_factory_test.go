package main

import (
	"testing"

	"github.com/abdul-hamid-achik/sonar/internal/config"
)

func TestNewModelRouter_Default(t *testing.T) {
	cfg := config.DefaultModelConfig()
	router := newModelRouter(&cfg, false)
	if _, ok := router.(*config.Router); !ok {
		t.Fatalf("expected default router, got %T", router)
	}
}

func TestNewModelRouter_Qwen(t *testing.T) {
	cfg := config.DefaultModelConfig()
	router := newModelRouter(&cfg, true)
	if _, ok := router.(*config.QwenModelRouter); !ok {
		t.Fatalf("expected qwen router, got %T", router)
	}
}
