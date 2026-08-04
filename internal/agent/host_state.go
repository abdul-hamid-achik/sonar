package agent

import (
	"fmt"

	"github.com/abdul-hamid-achik/sonar/internal/config"
	"github.com/abdul-hamid-achik/sonar/internal/llm"
	permissionpkg "github.com/abdul-hamid-achik/sonar/internal/permission"
)

// filesystemContext is a coherent workspace-policy snapshot. A running turn
// pins one snapshot so embedding setters can prepare the next turn without
// changing the authority or prompt of the current turn halfway through.
type filesystemContext struct {
	workDir       string
	ignoreContent string
	version       uint64
}

func (a *Agent) filesystemContext() filesystemContext {
	a.mu.RLock()
	defer a.mu.RUnlock()
	if a.filesystemPinned {
		return a.activeFilesystem
	}
	return filesystemContext{
		workDir:       a.workDir,
		ignoreContent: config.EffectiveIgnoreContent(a.ignoreContent),
		version:       a.filesystemVersion,
	}
}

func (a *Agent) pinTurnFilesystem() filesystemContext {
	a.mu.Lock()
	defer a.mu.Unlock()
	snapshot := filesystemContext{
		workDir:       a.workDir,
		ignoreContent: config.EffectiveIgnoreContent(a.ignoreContent),
		version:       a.filesystemVersion,
	}
	a.activeFilesystem = snapshot
	a.filesystemPinned = true
	return snapshot
}

func (a *Agent) unpinTurnFilesystem() {
	a.mu.Lock()
	a.activeFilesystem = filesystemContext{}
	a.filesystemPinned = false
	a.mu.Unlock()
}

func (a *Agent) activeWorkDir() string {
	return a.filesystemContext().workDir
}

type approvalHostState struct {
	checker           *permissionpkg.Checker
	callback          func(permissionpkg.ApprovalRequest)
	hostVersion       uint64
	filesystemVersion uint64
}

func (a *Agent) approvalStateSnapshot() approvalHostState {
	a.mu.RLock()
	defer a.mu.RUnlock()
	filesystemVersion := a.filesystemVersion
	if a.filesystemPinned {
		filesystemVersion = a.activeFilesystem.version
	}
	return approvalHostState{
		checker:           a.permChecker,
		callback:          a.approvalCallback,
		hostVersion:       a.approvalHostVersion,
		filesystemVersion: filesystemVersion,
	}
}

func (a *Agent) revalidateInteractiveApproval(state approvalHostState, call llm.ToolCall) error {
	current := a.approvalStateSnapshot()
	if current.hostVersion != state.hostVersion || current.filesystemVersion != state.filesystemVersion || current.checker != state.checker {
		return fmt.Errorf("approval host or workspace policy changed while the request was open")
	}
	if state.checker == nil || a.permissionCheckResult(state.checker, call) != permissionpkg.CheckAsk {
		return fmt.Errorf("tool permission policy changed while the request was open")
	}
	return nil
}

func (a *Agent) permissionChecker() *permissionpkg.Checker {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.permChecker
}
