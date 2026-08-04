package main

import (
	"fmt"
	"strings"

	"github.com/abdul-hamid-achik/sonar/internal/config"
)

// restoreManualProviderPreference applies a saved /provider selection to an
// already-validated configuration, and re-validates the result.
//
// Config.Validate runs during load, and privacy.local_only is enforced there
// and only there — validateProviderProfile is called with the local-only flag
// for the ACTIVE profile, while other profiles are shape-checked with it off.
// Restoring a saved name afterwards mutates which profile is active, so
// without a second validation a remembered remote provider silently
// re-attaches on a later launch under privacy.local_only: true and prompts
// leave the machine. Revalidating is cheap and closes that window; on failure
// the configured default stands.
//
// The returned string is a warning for stderr, empty when nothing is wrong.
func restoreManualProviderPreference(cfg *config.Config, preferred string) string {
	preferred = strings.TrimSpace(preferred)
	if cfg == nil || preferred == "" {
		return ""
	}

	restored := *cfg
	if _, _, resolveErr := restored.Provider.ResolveProfile(preferred); resolveErr == nil {
		restored.Provider.Active = preferred
	} else if !restored.Provider.HasProfiles() {
		// Flat catalog: accept known type names as an active preference.
		if !config.IsKnownProviderType(preferred) {
			return fmt.Sprintf("saved provider %q is not available; using configured default", preferred)
		}
		restored.Provider.Type = preferred
		restored.Provider.Active = preferred
	} else {
		return fmt.Sprintf("saved provider %q is not in the catalog; using configured default", preferred)
	}

	if err := restored.Validate(); err != nil {
		return fmt.Sprintf("saved provider %q was not restored: %v", preferred, err)
	}

	cfg.Provider = restored.Provider
	return ""
}
