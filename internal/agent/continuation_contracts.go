package agent

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"io"
	"strings"

	"github.com/abdul-hamid-achik/sonar/internal/ecosystem"
	"github.com/abdul-hamid-achik/sonar/internal/llm"
	"github.com/abdul-hamid-achik/sonar/internal/mcp"
)

const maxContinuationContracts = 16

// continuationContract is turn-scoped model-context-only state. Parameters is
// a detached exact schema received from MCPHub; it is used solely to reject
// malformed continuation arguments and never becomes transcript or session
// content or survives into a later turn.
type continuationContract struct {
	definition    llm.ToolDef
	schemaDigest  [sha256.Size]byte
	registryEpoch uint64
	sequence      uint64
}

type continuationContractKey struct {
	Gateway string
	Server  string
	Tool    string
}

func (a *Agent) clearContinuationContracts() {
	if a == nil {
		return
	}
	a.mu.Lock()
	clear(a.continuationContracts)
	a.continuationSequence = 0
	a.mu.Unlock()
}

// rememberContinuationContract records a schema only after the ordinary exact
// MCPHub receipt parser and host trust checks have succeeded. The host may also
// pre-fetch describe for a confident capability route; an absent contract still
// leaves a lazy auto-continuation unsupported.
func (a *Agent) rememberContinuationContract(
	call llm.ToolCall,
	projection ecosystem.ToolProjection,
	structured json.RawMessage,
	sourceSnapshot mcp.ToolSnapshot,
) bool {
	if a == nil || projection.Specialist != "mcphub" || projection.Operation != "mcphub_describe_tool" ||
		projection.Transport != ecosystem.TransportSucceeded || projection.Domain != ecosystem.DomainSucceeded ||
		!projection.DomainTyped || projection.Digest == nil || projection.Digest.Kind != ecosystem.DigestMCPHubDescribe ||
		sourceSnapshot.Epoch == 0 ||
		!a.trustedDirectMCPHubOperation(call, "mcphub_describe_tool") {
		return false
	}
	target, ok := requestedMCPHubDescribeTarget(call.Arguments)
	if !ok || target != projection.Digest.Target {
		return false
	}
	parts := strings.Split(call.Name, "__")
	if len(parts) != 2 || parts[1] != "mcphub_describe_tool" {
		return false
	}
	if !sourceSnapshot.ServerAvailable(parts[0]) {
		return false
	}
	server, tool, exact := strings.Cut(target, "__")
	if !exact || server == "" || tool == "" {
		return false
	}
	var envelope struct {
		Server      string          `json:"server"`
		Tool        string          `json:"tool"`
		Namespaced  string          `json:"namespaced"`
		InputSchema json.RawMessage `json:"input_schema"`
	}
	if json.Unmarshal(structured, &envelope) != nil || len(envelope.InputSchema) == 0 ||
		len(envelope.InputSchema) > maxTransientSchemaBytes {
		return false
	}
	actual, exact := safeMCPNamespacedIdentifier(envelope.Namespaced)
	if !exact || actual != target || envelope.Server != server || envelope.Tool != tool {
		return false
	}
	decoder := json.NewDecoder(bytes.NewReader(envelope.InputSchema))
	decoder.UseNumber()
	var parameters map[string]any
	if decoder.Decode(&parameters) != nil || parameters == nil {
		return false
	}
	var trailing any
	if decoder.Decode(&trailing) != io.EOF {
		return false
	}
	// Resolve once without any external loader. This rejects unsupported or
	// network-backed references before the schema can influence a suggestion.
	definition := llm.ToolDef{Name: target, Parameters: parameters}
	if err := validateMCPToolSchema(definition); err != nil {
		return false
	}
	canonicalSchema, err := json.Marshal(parameters)
	if err != nil || len(canonicalSchema) == 0 || len(canonicalSchema) > maxTransientSchemaBytes {
		return false
	}

	snapshot := a.registry.SnapshotTools()
	if snapshot.Epoch != sourceSnapshot.Epoch {
		return false
	}
	key := continuationContractKey{Gateway: parts[0], Server: server, Tool: tool}
	digest := sha256.Sum256(canonicalSchema)
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.continuationContracts == nil {
		a.continuationContracts = make(map[string]continuationContract)
	}
	a.continuationSequence++
	cacheKey := key.String()
	a.continuationContracts[cacheKey] = continuationContract{
		definition: cloneContinuationDefinition(definition), schemaDigest: digest,
		registryEpoch: sourceSnapshot.Epoch, sequence: a.continuationSequence,
	}
	for len(a.continuationContracts) > maxContinuationContracts {
		oldestKey := ""
		oldestSequence := ^uint64(0)
		for key, contract := range a.continuationContracts {
			if contract.sequence < oldestSequence || contract.sequence == oldestSequence && key < oldestKey {
				oldestKey, oldestSequence = key, contract.sequence
			}
		}
		delete(a.continuationContracts, oldestKey)
	}
	return true
}

func (a *Agent) continuationContract(key continuationContractKey, registryEpoch uint64) (llm.ToolDef, [sha256.Size]byte, bool) {
	a.mu.RLock()
	contract, ok := a.continuationContracts[key.String()]
	a.mu.RUnlock()
	if !ok || registryEpoch == 0 || contract.registryEpoch != registryEpoch {
		return llm.ToolDef{}, [sha256.Size]byte{}, false
	}
	return cloneContinuationDefinition(contract.definition), contract.schemaDigest, true
}

func cloneContinuationDefinition(definition llm.ToolDef) llm.ToolDef {
	copy := definition
	encoded, err := json.Marshal(definition.Parameters)
	if err != nil {
		copy.Parameters = nil
		return copy
	}
	copy.Parameters = nil
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.UseNumber()
	if decoder.Decode(&copy.Parameters) != nil {
		copy.Parameters = nil
	}
	return copy
}

func (key continuationContractKey) String() string {
	return key.Gateway + "\x00" + key.Server + "\x00" + key.Tool
}
