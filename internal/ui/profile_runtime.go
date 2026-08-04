package ui

import (
	"fmt"
	"strings"

	"github.com/abdul-hamid-achik/sonar/internal/config"
)

type modeContextSetter interface {
	SetModeContext(mode config.ModeContext)
}

func (m *Model) SetAgentProfileSource(agentsDir *config.AgentsDir, baseLoadedContext, activeProfile string) {
	m.agentsDir = agentsDir
	m.baseLoadedContext = baseLoadedContext
	m.setActiveProfileMetadata(activeProfile)
}

func (m *Model) setActiveProfileMetadata(name string) {
	profileSkills := []string(nil)
	if m.agentsDir != nil && name != "" {
		if profile := m.agentsDir.GetAgent(name); profile != nil {
			profileSkills = uniqueSkillNames(profile.Skills)
		}
	}
	// Startup profile skills may already be active before the TUI is built.
	// Infer the remaining active skills as manual contributions exactly once.
	if m.manualSkills == nil {
		m.manualSkills = subtractSkillNames(m.activeSkillNames(), profileSkills)
	}
	m.agentProfile = name
	m.profileSkills = profileSkills
}

func uniqueSkillNames(groups ...[]string) []string {
	seen := make(map[string]struct{})
	result := make([]string, 0)
	for _, group := range groups {
		for _, name := range group {
			if name == "" {
				continue
			}
			if _, exists := seen[name]; exists {
				continue
			}
			seen[name] = struct{}{}
			result = append(result, name)
		}
	}
	return result
}

func subtractSkillNames(names, remove []string) []string {
	removeSet := make(map[string]struct{}, len(remove))
	for _, name := range remove {
		removeSet[name] = struct{}{}
	}
	result := make([]string, 0, len(names))
	for _, name := range names {
		if _, removed := removeSet[name]; !removed {
			result = append(result, name)
		}
	}
	return uniqueSkillNames(result)
}

func (m *Model) activeSkillNames() []string {
	if m.skillMgr == nil {
		return nil
	}
	var names []string
	for _, item := range m.skillMgr.All() {
		if item.Active {
			names = append(names, item.Name)
		}
	}
	return uniqueSkillNames(names)
}

func (m *Model) validateSkillNames(names []string, owner string) error {
	names = uniqueSkillNames(names)
	if len(names) == 0 {
		return nil
	}
	if m.skillMgr == nil {
		return fmt.Errorf("%s skills are unavailable: skill manager is disabled", owner)
	}
	for _, name := range names {
		if !m.skillMgr.Has(name) {
			return fmt.Errorf("%s skill %q is not available", owner, name)
		}
	}
	return nil
}

// setSkillContributions atomically replaces the manual/profile ownership sets
// and projects their union onto the legacy Skill.Active flags.
func (m *Model) setSkillContributions(manual, profile []string) error {
	manual = uniqueSkillNames(manual)
	profile = uniqueSkillNames(profile)
	if err := m.validateSkillNames(uniqueSkillNames(manual, profile), "session"); err != nil {
		return err
	}
	if m.skillMgr != nil {
		if err := m.skillMgr.UpdateActive(m.activeSkillNames(), uniqueSkillNames(manual, profile)); err != nil {
			return fmt.Errorf("update active skills: %w", err)
		}
		if m.agent != nil {
			m.agent.SetSkillContent(m.skillMgr.ActiveContent())
		}
	} else if m.agent != nil {
		m.agent.SetSkillContent("")
	}
	m.manualSkills = append([]string{}, manual...)
	m.profileSkills = append([]string{}, profile...)
	return nil
}

func (m *Model) setManualSkill(name string, active bool) error {
	manual := append([]string{}, m.manualSkills...)
	if active {
		manual = uniqueSkillNames(manual, []string{name})
	} else {
		manual = subtractSkillNames(manual, []string{name})
	}
	return m.setSkillContributions(manual, m.profileSkills)
}

func (m *Model) syncLoadedContext() {
	var parts []string
	if m.baseLoadedContext != "" {
		parts = append(parts, m.baseLoadedContext)
	}
	if m.manualLoadedContext != "" {
		parts = append(parts, m.manualLoadedContext)
	}
	if m.agentsDir != nil && m.agentProfile != "" {
		if profile := m.agentsDir.GetAgent(m.agentProfile); profile != nil && profile.SystemPrompt != "" {
			parts = append(parts, profile.SystemPrompt)
		}
	}

	if m.agent != nil {
		m.agent.SetLoadedContext(strings.Join(parts, "\n\n"))
	}
}

func (m *Model) validateAgentProfile(name string) (*config.AgentProfile, error) {
	if m.agentsDir == nil {
		return nil, fmt.Errorf("no agent profiles available")
	}

	profile := m.agentsDir.GetAgent(name)
	if profile == nil {
		return nil, fmt.Errorf("unknown agent profile: %s", name)
	}
	if err := m.validateSkillNames(uniqueSkillNames(m.manualSkills, m.profileSkills, profile.Skills), "profile"); err != nil {
		return nil, err
	}
	return profile, nil
}

func (m *Model) applyAgentProfileRuntime(name string, profile *config.AgentProfile) error {
	if profile == nil {
		return fmt.Errorf("agent profile %q is unavailable", name)
	}
	if err := m.setSkillContributions(m.manualSkills, profile.Skills); err != nil {
		return fmt.Errorf("update profile skills: %w", err)
	}
	m.agentProfile = name
	m.syncLoadedContext()
	// Scope is committed last, after every fallible profile operation.
	if m.agent != nil {
		m.agent.SetMCPServerScope(profile.MCPServers)
	}
	return nil
}

func (m *Model) applyAgentProfile(name string) error {
	if name == "" {
		// Removing a profile is a real authority transition: remove only the
		// profile-owned skills/prompt, restore the default MCP scope, and release
		// any profile model pin. The already-loaded model may remain resident
		// until normal routing chooses another one.
		if err := m.setSkillContributions(m.manualSkills, nil); err != nil {
			return fmt.Errorf("remove profile skills: %w", err)
		}
		m.agentProfile = ""
		m.syncLoadedContext()
		if m.agent != nil {
			m.agent.SetMCPServerScope(nil)
		}
		m.modelPinned = false
		return nil
	}
	profile, err := m.validateAgentProfile(name)
	if err != nil {
		return err
	}
	if profile.Model != "" {
		if err := m.validateModelAdmission(profile.Model); err != nil {
			return fmt.Errorf("profile model: %w", err)
		}
		if m.modelManager != nil && profile.Model != m.model {
			m.prepareModelSwitch()
			if err := m.modelManager.SetCurrentModel(profile.Model); err != nil {
				return fmt.Errorf("switch profile model: %w", err)
			}
		}
	}
	if err := m.applyAgentProfileRuntime(name, profile); err != nil {
		return err
	}

	if profile.Model != "" {
		m.setCurrentModelProjection(profile.Model)
		m.modelPinned = true
	} else {
		m.modelPinned = false
	}
	return nil
}

func (m *Model) setRouterMode(mode config.ModeContext) {
	if setter, ok := m.router.(modeContextSetter); ok {
		setter.SetModeContext(mode)
	}
}

func (m *Model) prepareModelSwitch() {
	// Join background title inference before model/runtime changes so it
	// cannot hold the ordinary admission lease across the switch.
	m.stopSessionTitleGen()
	if m.agent != nil {
		m.agent.PrepareModelSwitch()
	}
}
