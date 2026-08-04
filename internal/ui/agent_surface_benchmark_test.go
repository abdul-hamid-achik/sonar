package ui

import (
	"fmt"
	"testing"
)

var (
	benchmarkAgentSurfaceSink AgentSurfaceProjection
	benchmarkAgentSurfaceErr  error
)

func BenchmarkAgentSurfaceProjection10K(b *testing.B) {
	const entryCount = 10_000

	b.Run("project_only", func(b *testing.B) {
		model := benchmarkAgentSurfaceModel(entryCount)
		b.ReportAllocs()
		b.ReportMetric(entryCount, "entries/op")
		b.ResetTimer()

		for range b.N {
			benchmarkAgentSurfaceSink, benchmarkAgentSurfaceErr = projectAgentSurface(
				model.entries,
				model.toolEntries,
			)
		}
		if benchmarkAgentSurfaceErr != nil {
			b.Fatal(benchmarkAgentSurfaceErr)
		}
	})

	b.Run("reconcile_only", func(b *testing.B) {
		model := benchmarkAgentSurfaceModel(entryCount)
		if err := model.reconcileTranscriptEntries(); err != nil {
			b.Fatal(err)
		}
		b.ReportAllocs()
		b.ReportMetric(entryCount, "entries/op")
		b.ResetTimer()

		for range b.N {
			benchmarkAgentSurfaceErr = model.reconcileTranscriptEntries()
		}
		if benchmarkAgentSurfaceErr != nil {
			b.Fatal(benchmarkAgentSurfaceErr)
		}
	})

	b.Run("cached_reconcile_and_project", func(b *testing.B) {
		model := benchmarkAgentSurfaceModel(entryCount)
		if _, err := model.reconcileTranscriptEntriesForRender(); err != nil {
			b.Fatal(err)
		}
		b.ReportAllocs()
		b.ReportMetric(entryCount, "entries/op")
		b.ResetTimer()

		for range b.N {
			if _, benchmarkAgentSurfaceErr = model.reconcileTranscriptEntriesForRender(); benchmarkAgentSurfaceErr == nil {
				benchmarkAgentSurfaceSink, benchmarkAgentSurfaceErr = projectAgentSurface(
					model.entries,
					model.toolEntries,
				)
			}
		}
		if benchmarkAgentSurfaceErr != nil {
			b.Fatal(benchmarkAgentSurfaceErr)
		}
	})

	b.Run("expert_progress_reconcile_and_project", func(b *testing.B) {
		model := benchmarkAgentSurfaceModel(entryCount)
		if err := model.reconcileTranscriptEntries(); err != nil {
			b.Fatal(err)
		}
		summaries := [...]string{"1 running, 1 queued", "1 completed, 1 running"}
		b.ReportAllocs()
		b.ReportMetric(entryCount, "entries/op")
		b.ResetTimer()

		for index := range b.N {
			// Expert progress updates the bounded host summary before refreshing
			// the Hub. Alternating it exercises the semantic revision path
			// without admitting raw provider or child-agent payloads.
			model.toolEntries[0].Summary = summaries[index&1]
			benchmarkAgentSurfaceSink, benchmarkAgentSurfaceErr = model.refreshedAgentSurfaceProjection()
		}
		if benchmarkAgentSurfaceErr != nil {
			b.Fatal(benchmarkAgentSurfaceErr)
		}
	})
}

func benchmarkAgentSurfaceModel(entryCount int) *Model {
	entries := make([]ChatEntry, entryCount)
	for index := range entries {
		entries[index] = ChatEntry{
			BlockID:   BlockID(fmt.Sprintf("bench_block_%05d", index)),
			TurnID:    "bench_turn",
			Revision:  1,
			Lifecycle: BlockSettled,
			Kind:      "assistant",
			Content:   "bounded visible transcript content",
		}
	}

	progress := liveAgentProgress("benchmark-agent")
	tools := []ToolEntry{{
		ID:             "provider-call-not-projected",
		Name:           "consult_experts",
		Summary:        "1 running, 1 queued",
		Status:         ToolStatusRunning,
		ExpertProgress: progress,
	}}
	last := len(entries) - 1
	entries[last] = ChatEntry{
		BlockID:   BlockID(fmt.Sprintf("bench_block_%05d", last)),
		TurnID:    "bench_turn",
		Revision:  1,
		Lifecycle: BlockLive,
		Kind:      "tool_group",
		ToolIndex: 0,
	}

	return &Model{
		entries:     entries,
		toolEntries: tools,
	}
}
