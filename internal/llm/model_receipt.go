package llm

import "context"

// ModelReceipt binds a run to the exact inference artifact it used. Digest
// and residency come from the live local inventory; both stay empty/unknown
// for remote providers or when the runtime cannot be reached, so a consumer
// can always distinguish "not offloaded" from "not inspected".
type ModelReceipt struct {
	Name     string
	Digest   string
	Provider string
	Remote   bool
	// VRAMBytes/TotalBytes report weights residency from the runtime's process
	// inventory. VRAMBytes < TotalBytes means part of the model runs on CPU.
	VRAMBytes    int64
	TotalBytes   int64
	OffloadKnown bool
}

// CurrentModelReceipt inspects the active model's verified identity. It is a
// bounded read-only probe: inventory errors degrade to a receipt without
// digest/residency rather than failing the caller's settlement path.
func (m *ModelManager) CurrentModelReceipt(ctx context.Context) ModelReceipt {
	receipt := ModelReceipt{
		Name:     m.CurrentModel(),
		Provider: m.ActiveProviderName(),
		Remote:   m.RemoteProvider(),
	}
	if receipt.Remote || receipt.Name == "" {
		return receipt
	}
	if models, err := m.ListOllamaModels(ctx); err == nil {
		for _, model := range models {
			if model.Name == receipt.Name {
				receipt.Digest = model.Digest
				break
			}
		}
	}
	if running, err := m.ListRunningOllamaModels(ctx); err == nil {
		for _, active := range running {
			if active.Model.Name == receipt.Name {
				receipt.VRAMBytes = active.SizeVRAM
				receipt.TotalBytes = active.Model.SizeBytes
				receipt.OffloadKnown = active.Model.SizeBytes > 0
				break
			}
		}
	}
	return receipt
}
