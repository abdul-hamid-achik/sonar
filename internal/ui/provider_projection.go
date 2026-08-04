package ui

import (
	"strings"

	"github.com/abdul-hamid-achik/sonar/internal/llm"
)

// ProviderProfileID is an opaque catalog identity. It is used only to route a
// selection back to the model manager and is never rendered directly.
type ProviderProfileID string

type ProviderLocality uint8

const (
	ProviderLocal ProviderLocality = iota
	ProviderRemote
)

type ProviderCredentialState uint8

const (
	ProviderCredentialNotRequired ProviderCredentialState = iota
	ProviderCredentialReady
	ProviderCredentialMissing
)

// ProviderOptionPresentation is the complete provider-picker boundary. It
// deliberately has no BaseURL, client, header, credential value, or arbitrary
// configuration field.
type ProviderOptionPresentation struct {
	ProfileID      ProviderProfileID
	Label          string
	KindLabel      string
	ModelLabel     string
	Locality       ProviderLocality
	Credential     ProviderCredentialState
	CredentialHint string
	Active         bool
	Selectable     bool
	DisabledReason string
}

// providerOptionPresentations consumes manager descriptors once and
// immediately drops transport configuration. Disabled reasons are host-owned;
// the only credential detail admitted to UI is a validated environment
// variable name, never its value.
func providerOptionPresentations(descriptors []llm.ProviderDescriptor) []ProviderOptionPresentation {
	options := make([]ProviderOptionPresentation, 0, len(descriptors))
	for _, descriptor := range descriptors {
		if strings.TrimSpace(descriptor.Name) == "" {
			continue
		}
		option := ProviderOptionPresentation{
			ProfileID:  ProviderProfileID(descriptor.Name),
			Label:      sanitizeTerminalSingleLine(descriptor.Name),
			KindLabel:  sanitizeTerminalSingleLine(descriptor.Type),
			ModelLabel: sanitizeTerminalSingleLine(descriptor.Model),
			Locality:   ProviderLocal,
			Credential: ProviderCredentialNotRequired,
			Active:     descriptor.Active,
			Selectable: true,
		}
		if option.Label == "" {
			option.Label = "Unnamed provider"
		}
		if descriptor.Remote {
			option.Locality = ProviderRemote
			if descriptor.KeyPresent {
				option.Credential = ProviderCredentialReady
			} else {
				option.Credential = ProviderCredentialMissing
				option.CredentialHint = providerCredentialHint(descriptor.APIKeyEnv)
				option.Selectable = false
				option.DisabledReason = "credential unavailable"
			}
		}
		options = append(options, option)
	}
	return options
}

func providerCredentialHint(value string) string {
	value = strings.TrimSpace(value)
	if value == "" || !validEnvironmentVariableName(value) {
		return ""
	}
	return "$" + value
}

func validEnvironmentVariableName(value string) bool {
	for index := range len(value) {
		character := value[index]
		if index == 0 {
			if (character < 'A' || character > 'Z') &&
				(character < 'a' || character > 'z') && character != '_' {
				return false
			}
			continue
		}
		if (character < 'A' || character > 'Z') &&
			(character < 'a' || character > 'z') &&
			(character < '0' || character > '9') && character != '_' {
			return false
		}
	}
	return value != ""
}
