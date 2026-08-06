package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/abdul-hamid-achik/sonar/internal/agent"
	"github.com/abdul-hamid-achik/sonar/internal/command"
	"github.com/abdul-hamid-achik/sonar/internal/config"
	"github.com/abdul-hamid-achik/sonar/internal/db"
	executionpkg "github.com/abdul-hamid-achik/sonar/internal/execution"
	"github.com/abdul-hamid-achik/sonar/internal/goal"
	"github.com/abdul-hamid-achik/sonar/internal/goaladvisor"
	"github.com/abdul-hamid-achik/sonar/internal/ice"
	"github.com/abdul-hamid-achik/sonar/internal/imageasset"
	"github.com/abdul-hamid-achik/sonar/internal/llm"
	"github.com/abdul-hamid-achik/sonar/internal/logging"
	"github.com/abdul-hamid-achik/sonar/internal/mcp"
	"github.com/abdul-hamid-achik/sonar/internal/memory"
	"github.com/abdul-hamid-achik/sonar/internal/permission"
	"github.com/abdul-hamid-achik/sonar/internal/runtimepref"
	"github.com/abdul-hamid-achik/sonar/internal/skill"
	"github.com/abdul-hamid-achik/sonar/internal/supervisor"
	"github.com/abdul-hamid-achik/sonar/internal/ui"
)

var version = "dev"

type availabilityAwareRouter interface {
	SetAvailableModels([]string)
	ResolveAvailableModel(string) string
}

func main() {
	os.Exit(run())
}

// validateHeadlessFlagCombinations rejects flag pairings that cannot mean
// anything, before run() builds a database, a provider, or an MCP registry.
//
// These are the checks that were inline at the top of a 1154-line run(): four
// flags that only exist to shape a headless request, and a prompt that must
// not be blank. Lifting them out is not cosmetic — as inline `return 2` blocks
// they were unreachable from a test, so the exact wording a script sees on
// stderr had no coverage at all. Each message names the flag that was rejected
// and the flag it needs, because the caller is usually a shell script and the
// exit code alone is 2 either way.
func validateHeadlessFlagCombinations(options rootOptions) error {
	if options.promptProvided && strings.TrimSpace(options.prompt) == "" {
		return errors.New("prompt: -p/--prompt requires a non-empty value")
	}
	if options.promptProvided {
		return nil
	}
	// Everything below is meaningless without a headless prompt to shape.
	if options.toolsProvided {
		return errors.New("tools: --tools requires a headless prompt via -p/--prompt")
	}
	if options.jsonReceipt {
		return errors.New("json: --json requires a headless prompt via -p/--prompt")
	}
	if options.runID != "" || options.turnID != "" || options.actor != "" {
		return errors.New("identity: --run-id, --turn-id, and --actor require a headless prompt via -p/--prompt")
	}
	return nil
}

func run() int {
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "init":
			return handleInit(os.Args[2:])
		case "logs":
			return handleLogs(os.Args[2:])
		case "goal":
			return handleGoalCommand(os.Args[2:])
		case "execution":
			return handleExecutionCommand(os.Args[2:])
		case "session":
			return handleSessionCommand(os.Args[2:])
		case "providers":
			return handleProvidersCommand(os.Args[2:])
		case "help":
			return handleRootHelp(os.Args[2:], os.Args[0], os.Stdout, os.Stderr)
		}
	}

	options, err := parseRootOptions(os.Args[0], os.Args[1:], os.Stderr, os.Stdout)
	if errors.Is(err, flag.ErrHelp) {
		return 0
	}
	if err != nil {
		return 2
	}
	if options.version {
		fmt.Println(effectiveVersion())
		return 0
	}
	if len(options.arguments) > 0 {
		fmt.Fprintf(os.Stderr, "unexpected argument %q; pass a headless request with -p/--prompt\n", options.arguments[0])
		return 2
	}
	writeLegacyYoloWarning(os.Stderr, options.legacyYolo)

	qwenRouterFlag := &options.qwenRouter
	modelFlag := &options.model
	agentProfileFlag := &options.agentProfile
	promptFlag := options.prompt
	toolsFlag := options.tools
	modeFlag := &options.mode
	autoFlag := &options.auto
	planFlag := &options.plan
	skipApprovalsFlag := &options.skipApprovals
	legacyYoloFlag := &options.legacyYolo
	resumeFlag := options.resume
	promptProvided := options.promptProvided
	if err := validateHeadlessFlagCombinations(options); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	externalRunID, externalTurnID, externalActor, err := resolveExternalTurnIdentity(options)
	if err != nil {
		fmt.Fprintf(os.Stderr, "identity: %v\n", err)
		return 2
	}
	resumeSelector, resumeRequested, err := resumeFlag.selector()
	if err != nil {
		fmt.Fprintf(os.Stderr, "resume: %v\n", err)
		return 2
	}
	if err := validateResumeInvocation(resumeRequested, promptProvided); err != nil {
		fmt.Fprintf(os.Stderr, "resume: %v\n", err)
		return 2
	}
	resolvedMode, err := resolveAuthorityShortcut(
		*modeFlag,
		options.modeProvided,
		*autoFlag,
		*planFlag,
	)
	if err != nil {
		fmt.Fprintf(os.Stderr, "mode: %v\n", err)
		return 2
	}
	headlessMode, err := parseHeadlessMode(resolvedMode, promptProvided)
	if err != nil {
		fmt.Fprintf(os.Stderr, "mode: %v\n", err)
		return 2
	}
	var narrowedHeadlessToolPolicy agent.ToolPolicy
	if options.toolsProvided {
		modeConfig := ui.DefaultModeConfigs()[headlessMode]
		narrowedHeadlessToolPolicy, err = narrowHeadlessToolPolicy(modeConfig.ToolPolicy, toolsFlag)
		if err != nil {
			fmt.Fprintf(os.Stderr, "tools: %v\n", err)
			return 2
		}
	}

	cfg, agentsDir, err := config.LoadWithAgentsDir()
	if err != nil {
		fmt.Fprintf(os.Stderr, "config: %v\n", err)
		return 1
	}
	if *modelFlag != "" {
		cfg.Ollama.Model = *modelFlag
	}
	if *agentProfileFlag != "" {
		cfg.AgentProfile = *agentProfileFlag
	}
	var modelPreferenceStore *runtimepref.Store
	if preferencePath, preferenceErr := runtimepref.DefaultPath(); preferenceErr != nil {
		fmt.Fprintf(os.Stderr, "warning: model preference persistence unavailable: %v\n", preferenceErr)
	} else {
		modelPreferenceStore = runtimepref.NewStore(preferencePath)
	}
	// Restore last /provider selection when the user did not pass
	// SONAR_PROVIDER for this process. Config profiles remain the catalog;
	// the saved name only chooses among defined profiles.
	if modelPreferenceStore != nil && strings.TrimSpace(os.Getenv("SONAR_PROVIDER")) == "" {
		if preferred, ok, prefErr := modelPreferenceStore.LoadManualProvider(); prefErr != nil {
			fmt.Fprintf(os.Stderr, "warning: saved provider preference ignored: %v\n", prefErr)
		} else if ok {
			if warning := restoreManualProviderPreference(cfg, preferred); warning != "" {
				fmt.Fprintf(os.Stderr, "warning: %s\n", warning)
			}
		}
	}

	// Create router - use Qwen-optimized router if flag is set.
	var router config.ModelRouter
	if *qwenRouterFlag {
		fmt.Fprintf(os.Stderr, "Using Qwen-optimized model router (experimental)\n")
	}
	router = newModelRouter(&cfg.Model, *qwenRouterFlag)

	providerName, providerProfile, providerErr := cfg.Provider.ActiveProfile()
	if providerErr != nil {
		// Empty/default ollama when provider block is absent.
		providerName = "ollama"
		providerProfile = config.ProviderProfile{Type: config.ProviderTypeOllama}.Resolve()
	}
	provider := providerProfile.Resolve()
	modelName := cfg.Ollama.Model
	if provider.IsRemote() && strings.TrimSpace(provider.Model) != "" {
		modelName = provider.Model
	}
	modelPinned := shouldPinStartupModel(*modelFlag, cfg.Model.AutoSelect)
	profileModelPinned := false
	if cfg.AgentProfile != "" && agentsDir != nil {
		if profile := agentsDir.GetAgent(cfg.AgentProfile); profile != nil {
			if profile.Model != "" {
				modelName = profile.Model
				modelPinned = true
				profileModelPinned = true
			}
		}
	}
	if *modelFlag != "" {
		modelName = *modelFlag
		modelPinned = true
	}
	// Ollama owns model availability for the local path. Remote providers use a
	// configured model id and env-sourced API key (typically via TinyVault).
	modelManager := llm.NewModelManager(cfg.Ollama.BaseURL, cfg.Ollama.NumCtx)
	defer modelManager.Close()
	modelManager.ConfigureProviderCatalog(cfg.Provider, cfg.Privacy.LocalOnly, cfg.Ollama.Model)

	var (
		ollamaInventory  []llm.OllamaModel
		discoveryErr     error
		modelList        []string
		autoChatModels   []string
		manualChatModels []string
	)
	if provider.IsRemote() {
		// Provider keys may live in an operator-nominated env file rather than
		// the shell. Load before resolving so the file counts as "set", while an
		// explicit export still wins.
		if applied, warning, loadErr := config.LoadCredentialEnvFile(cfg.Credentials.EnvFile, provider.APIKeyEnv); loadErr != nil {
			fmt.Fprintf(os.Stderr, "credentials: %v\n", loadErr)
		} else {
			if warning != "" {
				fmt.Fprintf(os.Stderr, "warning: %s\n", warning)
			}
			if len(applied) > 0 {
				// Names only. A value must never reach a terminal or a log.
				fmt.Fprintf(os.Stderr, "credentials: loaded %s from %s\n",
					strings.Join(applied, ", "), cfg.Credentials.EnvFile)
			}
		}
		apiKey, keyErr := provider.ResolveAPIKey()
		if keyErr != nil {
			fmt.Fprintf(os.Stderr, "provider: %v\n", keyErr)
			return 1
		}
		remoteClient, clientErr := llm.NewProviderClient(provider.Type, provider.BaseURL, modelName, apiKey)
		if clientErr != nil {
			fmt.Fprintf(os.Stderr, "provider: %v\n", clientErr)
			return 1
		}
		if err := modelManager.ConfigureRemoteProvider(remoteClient, provider.ContextSize, providerName); err != nil {
			fmt.Fprintf(os.Stderr, "provider: %v\n", err)
			return 1
		}
		modelList = []string{modelName}
		modelPinned = true
		// Best-effort Ollama inventory for ICE/embeddings only.
		discoveryCtx, cancelDiscovery := context.WithTimeout(context.Background(), 2*time.Second)
		ollamaInventory, discoveryErr = modelManager.ListOllamaModels(discoveryCtx)
		cancelDiscovery()
		if discoveryErr == nil {
			modelManager.ConfigureOllamaRuntimeInventory(false, ollamaInventory, true)
		}
		if err := modelManager.SetCurrentModel(modelName); err != nil {
			fmt.Fprintf(os.Stderr, "set current model: %v\n", err)
			return 1
		}
		fmt.Fprintf(os.Stderr, "provider: %s (%s) model %s (api key from $%s)\n", providerName, provider.Type, modelName, provider.APIKeyEnv)
	} else {
		discoveryCtx, cancelDiscovery := context.WithTimeout(context.Background(), 2*time.Second)
		ollamaInventory, discoveryErr = modelManager.ListOllamaModels(discoveryCtx)
		cancelDiscovery()
		if discoveryErr == nil {
			enrichCtx, cancelEnrich := context.WithTimeout(context.Background(), 2*time.Second)
			ollamaInventory = enrichOllamaCapabilities(enrichCtx, modelManager, ollamaInventory, &cfg.Model)
			cancelEnrich()
		}
		modelManager.ConfigureOllamaRuntimeInventory(cfg.Privacy.LocalOnly, ollamaInventory, discoveryErr == nil)
		if discoveryErr != nil && cfg.Privacy.LocalOnly && promptFlag != "" {
			fmt.Fprintf(os.Stderr, "model: local-only mode could not verify Ollama local weights: %v\n", discoveryErr)
			return 1
		}
		awareRouter, ok := router.(availabilityAwareRouter)
		if !ok {
			fmt.Fprintln(os.Stderr, "model router does not support local inventory")
			return 1
		}
		manualChatModels = manuallySelectableOllamaChatModels(ollamaInventory, cfg.Privacy.LocalOnly)
		autoChatModels = autoRoutableOllamaChatModels(ollamaInventory, &cfg.Model)
		if modelPreferenceStore != nil && shouldRestoreManualModelPreference(*modelFlag, profileModelPinned) {
			preferred, saved, preferenceErr := modelPreferenceStore.LoadManualModel()
			switch {
			case preferenceErr != nil:
				fmt.Fprintf(os.Stderr, "warning: saved model preference ignored: %v\n", preferenceErr)
			case saved:
				if restored, ok, warning := restoreManualModelPreference(
					preferred, manualChatModels, autoChatModels, discoveryErr == nil,
				); ok {
					modelName = restored
					modelPinned = true
				} else if warning != "" {
					fmt.Fprintf(os.Stderr, "warning: %s\n", warning)
				}
			}
		}
		var resolveErr error
		modelName, modelList, resolveErr = resolveStartupModel(
			modelName,
			modelPinned,
			cfg.Privacy.LocalOnly,
			&cfg.Model,
			manualChatModels,
			autoChatModels,
			discoveryErr == nil,
			awareRouter,
		)
		if resolveErr != nil {
			fmt.Fprintf(os.Stderr, "model: %v\n", resolveErr)
			return 1
		}
		if discoveryErr != nil {
			fmt.Fprintf(os.Stderr, "warning: Ollama model discovery failed: %v\n", discoveryErr)
		}

		if discoveryErr == nil || !cfg.Privacy.LocalOnly {
			if err := modelManager.SetCurrentModel(modelName); err != nil {
				fmt.Fprintf(os.Stderr, "set current model: %v\n", err)
				return 1
			}
		}
	}

	var servers []config.ServerConfig
	if len(cfg.Servers) > 0 {
		servers = cfg.Servers
	} else if agentsDir != nil && agentsDir.HasMCP() {
		servers = agentsDir.GetMCPServers()
	}

	registry := mcp.NewRegistryWithVersion(effectiveVersion(), mcp.WithLocalOnly(cfg.Privacy.LocalOnly))
	if timeout := cfg.MCPCallTimeout(); timeout > 0 {
		registry.SetCallTimeout(timeout)
	}
	defer registry.Close()

	agentContext := cfg.Ollama.NumCtx
	if provider.IsRemote() && provider.ContextSize > 0 {
		agentContext = provider.ContextSize
	}
	ag := agent.New(modelManager, registry, agentContext)
	// Derive any reduced-friction MCP contracts from the same host-owned server
	// configuration the registry will connect. Server annotations and model tool
	// names alone never establish trust.
	ag.SetTrustedLocalMCPServers(servers)
	// The application always runs with durable execution tracking. Embedded
	// package users may opt out, but neither the TUI nor headless mode may send
	// provider work without a scoped SQLite ledger.
	ag.RequireExecutionLedger(true)
	ag.SetToolsConfig(cfg.Tools)
	ag.SetContinuationsConfig(cfg.Continuations)
	// OS-level confinement for shell subprocesses. Requesting it on a platform
	// with no driver is reported rather than ignored: an operator who edited
	// sandbox.enabled believes a boundary exists, and silence would let them
	// keep believing it.
	ag.SetSandboxPosture(agent.SandboxPosture{
		Enabled:      cfg.Sandbox.Enabled,
		AllowNetwork: cfg.Sandbox.AllowNetwork,
	})
	if cfg.Sandbox.Enabled && !ag.SandboxActive() {
		fmt.Fprintln(os.Stderr,
			"sandbox: enabled in config but this platform has no confinement driver; commands run unconfined")
	}
	ag.SetRouter(router)
	// No expert consultation. It selects several distinct models from a local
	// multi-model inventory and runs them side by side; sonar reaches hosted
	// providers, which serve one model per profile and expose no inventory to
	// choose from. This used to warn on every single launch about a feature the
	// operator had no way to enable, and then mark the runtime "setup failed" —
	// reporting the failure of something that was never offered.
	//
	// specs/pending/tui_agents.yml keeps the coverage against the day it is
	// rebuilt on multiple hosted profiles.
	// Cap any single tool result so a runaway read/command can't blow the
	// (small, local) context window. ~96KB is generous for code/output.
	ag.AddToolHook(agent.NewSizeCapHook(96 * 1024))
	if wd, err := os.Getwd(); err == nil {
		if err := applyWorkspaceIgnore(ag, wd); err != nil {
			fmt.Fprintf(os.Stderr, "workspace policy: %v\n", err)
			return 1
		}
	}
	// Open SQLite database for permissions and stats.
	dbStore, err := db.Open()
	if err != nil {
		fmt.Fprintf(os.Stderr, "database: %v\nsonar requires its private execution ledger and will not start without it.\n", err)
		return 1
	}
	ag.SetExecutionLedger(dbStore)
	defer func() {
		if err := dbStore.Close(); err != nil {
			log.Printf("warning: close database: %v", err)
		}
	}()
	// Register this after the database closer: defers run LIFO, so the active
	// turn joins and writes its final execution receipt before SQLite closes.
	defer ag.Close()

	// Set up tool permission checker.
	permChecker := permission.NewChecker(dbStore, *skipApprovalsFlag || *legacyYoloFlag)
	ag.SetPermissionChecker(permChecker)

	// Enable non-destructive compaction + manual checkpoints when the DB is up.
	ag.SetCheckpointStore(dbStore, 0)

	workspace := currentWorkspace()
	iceStorePath := ""
	if cfg.ICE.Enabled {
		iceStorePath, err = resolveICEStorePath(workspace, cfg.ICE.StorePath)
		if err != nil {
			fmt.Fprintf(os.Stderr, "config: %v\n", err)
			return 1
		}
	}
	var memStore *memory.Store
	if workspace == "" {
		log.Printf("warning: project memory disabled: workspace identity is unavailable")
	} else {
		// Pre-workspace memory remains quarantined. Startup only opens the
		// canonical workspace-scoped store; historical inventory is deliberately
		// absent from the everyday transcript and command surface.
		memStore = memory.NewStore(memory.DefaultPathForWorkspace(workspace))
		if err := memStore.Err(); err != nil {
			log.Printf("warning: project memory disabled: %v", err)
			memStore = nil
		} else {
			ag.SetMemoryStore(memStore)
		}
	}

	skillMgr, err := newRuntimeSkillManager(agentsDir, cfg.Agents.AutoLoad)
	if err != nil {
		fmt.Fprintf(os.Stderr, "skills: %v\n", err)
		return 1
	}
	if err := skillMgr.LoadAll(); err != nil {
		fmt.Fprintf(os.Stderr, "skills: %v\n", err)
		return 1
	}
	// One loader setup feeds both headless and TUI runs. Manual/profile
	// activation remains an independent, eager prompt-content mechanism.
	ag.SetSkillLoader(skillMgr)
	baseLoadedContext, err := buildBaseLoadedContext(agentsDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "project instructions: %v\n", err)
		return 1
	}
	baseLoadedContext = appendLoadedContext(baseLoadedContext, buildHostConfigProjection(cfg, agentsDir, servers))

	// Non-interactive / pipe mode: run a single prompt and exit.
	if promptFlag != "" {
		ctx, cancelRun := context.WithCancel(context.Background())
		defer cancelRun()
		headlessSignals := make(chan os.Signal, 1)
		headlessSignalDone := make(chan struct{})
		signal.Notify(headlessSignals, os.Interrupt, syscall.SIGTERM)
		go func() {
			select {
			case <-headlessSignals:
				// One signal requests a graceful cancellation. Restore the OS
				// default immediately so a wedged backend cannot swallow a second.
				signal.Stop(headlessSignals)
				cancelRun()
			case <-headlessSignalDone:
			}
		}()
		defer func() {
			signal.Stop(headlessSignals)
			close(headlessSignalDone)
		}()
		if err := applyInitialAgentProfile(ag, skillMgr, modelManager, agentsDir, baseLoadedContext, cfg.AgentProfile); err != nil {
			fmt.Fprintf(os.Stderr, "agent profile: %v\n", err)
			return 1
		}
		modeConfig := ui.DefaultModeConfigs()[headlessMode]
		if options.toolsProvided {
			modeConfig.ToolPolicy = narrowedHeadlessToolPolicy
			modeConfig.SystemPromptPrefix = narrowHeadlessSystemPrompt(modeConfig.SystemPromptPrefix, narrowedHeadlessToolPolicy)
		}
		if explicitRouter, ok := router.(interface{ SetModeContext(config.ModeContext) }); ok {
			explicitRouter.SetModeContext(modeConfig.RouterMode)
		}
		if !provider.IsRemote() {
			routedModel := selectHeadlessModel(modelName, promptFlag, modelPinned, router, modeConfig.RouterMode)
			if routedModel != modelName {
				modelName = routedModel
				if modelName == "" {
					fmt.Fprintln(os.Stderr, "model routing failed: no compatible local chat model is installed")
					return 1
				}
				if discoveryErr == nil {
					if !containsModel(autoChatModels, modelName) {
						fmt.Fprintf(os.Stderr, "model routing failed: model %q is not admitted for automatic local routing\n", modelName)
						return 1
					}
				}
				if err := modelManager.SetCurrentModel(modelName); err != nil {
					fmt.Fprintf(os.Stderr, "model routing failed: %v\n", err)
					return 1
				}
			}
		}

		// Ping the configured inference provider.
		if modelManager.RemoteProvider() {
			fmt.Fprintf(os.Stderr, "connecting to %s (%s)...\n", modelManager.RemoteProviderLabel(), modelName)
		} else {
			fmt.Fprintf(os.Stderr, "connecting to Ollama (%s)...\n", modelName)
		}
		if err := modelManager.Ping(); err != nil {
			if modelManager.RemoteProvider() {
				// stderr at startup is not a transcript boundary, so the bounded
				// status code can be shown here. Without it a spending-limit 403
				// is indistinguishable from a typo'd key.
				if status, ok := llm.ProviderHTTPStatus(err); ok {
					fmt.Fprintf(os.Stderr, "%s\nHTTP %d — %s\n", ui.ProviderFailureCopy, status, llm.ProviderStatusHint(status))
				} else {
					fmt.Fprintln(os.Stderr, ui.ProviderFailureCopy)
				}
			} else {
				fmt.Fprintf(os.Stderr, "ollama: %v\ntry: ollama serve · ollama pull %s\n", err, modelName)
			}
			return 1
		}

		// A PLAN turn or an explicit --tools narrowing has no MCP authority.
		// Do not launch downstream processes that the turn cannot call.
		if modeConfig.ToolPolicy.AllowMCP {
			var wg sync.WaitGroup
			for _, srv := range servers {
				wg.Add(1)
				go func(s config.ServerConfig) {
					defer wg.Done()
					fmt.Fprintf(os.Stderr, "connecting MCP server %s...\n", s.Name)
					if _, err := registry.ConnectServer(ctx, s); err != nil {
						fmt.Fprintf(os.Stderr, "MCP server %s failed: %v\n", s.Name, err)
					}
				}(srv)
			}
			wg.Wait()
		}

		// ICE setup.
		if cfg.ICE.Enabled && workspace != "" {
			iceEngine, err := ice.NewEngine(modelManager, memStore, resolvedICEEngineConfig(cfg, workspace, iceStorePath))
			if err != nil {
				fmt.Fprintf(os.Stderr, "ICE: %v\n", err)
			} else {
				ag.SetICEEngine(iceEngine)
			}
		} else if cfg.ICE.Enabled {
			fmt.Fprintln(os.Stderr, "ICE: disabled because workspace identity is unavailable")
		}

		// Headless -p is one bounded turn. AUTO uses the same scoped authority as
		// the TUI; it is independent from the --skip-approvals posture.
		ag.SetModeContext(modeConfig.SystemPromptPrefix, modeConfig.ToolPolicy)
		ag.SetAuthorityMode(headlessAuthorityMode(headlessMode))
		if externalRunID != "" {
			if err := ag.SetExecutionRunID(externalRunID); err != nil {
				fmt.Fprintf(os.Stderr, "run-id: %v\n", err)
				return 2
			}
		}
		if workspace == "" {
			fmt.Fprintln(os.Stderr, "sonar: workspace identity is unavailable; refusing to start a headless turn")
			return 1
		}
		var goalRuntime *goal.Runtime
		sessionStateRevision := int64(0)
		executionCursor := int64(0)
		newSession := activeGoalRun == nil
		var session db.Session
		if activeGoalRun != nil {
			var state headlessGoalState
			var record db.SessionStateRecord
			session, goalRuntime, state, record, err = loadHeadlessGoalState(ctx, dbStore, workspace, activeGoalRun.SessionID)
			if err != nil {
				fmt.Fprintf(os.Stderr, "goal run: load session: %v\n", err)
				return 1
			}
			ag.ReplaceMessages(state.Messages)
			if restoreErr := ag.RestoreContextPromptFloor(state.ContextPromptFloor); restoreErr != nil {
				fmt.Fprintf(os.Stderr, "goal run: restore context admission floor: %v\n", restoreErr)
				return 1
			}
			executionCursor = state.ExecutionCursor
			sessionStateRevision = record.Revision
		} else {
			session, err = dbStore.CreateSession(ctx, db.CreateSessionParams{
				Title: headlessSessionTitle(promptFlag), Model: modelName, Mode: modeConfig.Label, WorkspaceID: workspace,
			})
			if err != nil {
				fmt.Fprintf(os.Stderr, "sonar: create execution session: %v\n", err)
				return 1
			}
		}
		executionLease, err := dbStore.AcquireExecutionSessionLease(ctx, session.ID, workspace)
		if err != nil {
			var cleanupErr error
			if newSession {
				cleanupErr = dbStore.DeleteSession(context.Background(), session.ID)
			}
			fmt.Fprintf(os.Stderr, "sonar: lock execution session: %v\n", errors.Join(err, cleanupErr))
			return 1
		}
		defer func() {
			ag.Close()
			if err := executionLease.Close(); err != nil {
				log.Printf("warning: release execution session: %v", err)
			}
		}()
		ag.SetCheckpointSessionID(session.ID)
		ag.SetExecutionSessionID(session.ID, session.PublicID)
		ag.SetExecutionSnapshotCursor(executionCursor)

		// Run the agent synchronously. The goal admission, the execution ledger,
		// and the emitted receipt share one exact turn identity.
		var out headlessTurnOutput = agent.NewHeadlessOutput()
		var jsonOut *agent.JSONOutput
		if options.jsonReceipt {
			jsonOut = agent.NewJSONOutput()
			out = jsonOut
		}
		headlessTurnID := externalTurnID
		var goalTurnLimits agent.TurnLimits
		if goalRuntime != nil {
			if headlessTurnID == "" {
				goalTurnID, idErr := goal.NewGoalID()
				if idErr != nil {
					fmt.Fprintf(os.Stderr, "goal run: create turn identity: %v\n", idErr)
					return 1
				}
				headlessTurnID = "turn_" + goalTurnID
			}
			snapshot, snapshotErr := goalRuntime.Snapshot(ctx)
			if snapshotErr != nil {
				fmt.Fprintf(os.Stderr, "goal run: inspect runtime: %v\n", snapshotErr)
				return 1
			}
			if snapshot.State == goal.StatePaused {
				// An explicit `goal run` is the resume authority; the supervisor
				// then decides on the resumed state.
				if err := goalRuntime.Resume(ctx, "explicit headless goal run"); err != nil {
					fmt.Fprintf(os.Stderr, "goal run: resume goal: %v\n", err)
					return 1
				}
			}
			supervisorIssues, issuesErr := headlessSupervisorIssues(ctx, dbStore, session.ID, workspace)
			if issuesErr != nil {
				fmt.Fprintf(os.Stderr, "goal run: inspect control plane: %v\n", issuesErr)
				return 1
			}
			decision, decideErr := supervisor.Decide(ctx, goalRuntime, supervisor.Observation{
				LeaseOwned:       true,
				PersistenceReady: true,
				// The headless path has no Cortex evaluation loop yet, so a
				// Cortex-linked goal stops honestly instead of running unevaluated.
				AdvisorAvailable: false,
				Manual:           true,
				Issues:           supervisorIssues,
			})
			if decideErr != nil {
				fmt.Fprintf(os.Stderr, "goal run: supervise: %v\n", decideErr)
				return 1
			}
			var admission goal.TurnAdmissionKind
			switch decision.Action {
			case supervisor.ActionDispatchInitial:
				admission = goal.AdmissionInitial
			case supervisor.ActionDispatchManual:
				admission = goal.AdmissionManual
			default:
				reason := string(decision.Reason)
				if reason == "" {
					reason = string(decision.Action)
				}
				fmt.Fprintf(os.Stderr, "goal run: not admitted: %s · %s\n", reason, decision.Detail)
				if jsonOut != nil {
					receipt := agent.TurnReceipt{
						RunID:  ag.ExecutionRunID(),
						TurnID: headlessTurnID,
						Actor:  externalActor,
						Session: &agent.TurnReceiptSession{
							ID: session.ID, PublicID: session.PublicID, Workspace: workspace,
						},
						Model: agent.TurnReceiptModel{
							Name:     modelName,
							NumCtx:   ag.NumCtx(),
							Provider: modelManager.ActiveProviderName(),
							Remote:   modelManager.RemoteProvider(),
						},
						Status:          "not_admitted",
						StopReason:      reason,
						Decision:        receiptDecision(decision),
						ExecutionCursor: executionCursor,
					}
					if err := agent.WriteTurnReceipt(os.Stdout, jsonOut.ComposeReceipt(receipt)); err != nil {
						fmt.Fprintf(os.Stderr, "sonar: %v\n", err)
					}
				}
				return 1
			}
			limits, limitsErr := supervisor.AgentTurnLimits(decision.Goal, time.Now())
			if limitsErr != nil {
				fmt.Fprintf(os.Stderr, "goal run: %v\n", limitsErr)
				return 1
			}
			goalTurnLimits = limits
			if _, err := goalRuntime.BeginTurn(ctx, headlessTurnID, admission); err != nil {
				fmt.Fprintf(os.Stderr, "goal run: admit turn: %v\n", err)
				return 1
			}
		}
		if headlessTurnID == "" {
			mintedTurnID, idErr := executionpkg.NewTurnID()
			if idErr != nil {
				fmt.Fprintf(os.Stderr, "sonar: execution turn identity: %v\n", idErr)
				return 1
			}
			headlessTurnID = mintedTurnID
		}
		ag.AddUserMessage(promptFlag)
		persistHeadlessState := func(saveCtx context.Context, executionCursor int64) error {
			var stateJSON string
			var encodeErr error
			providerIdentity := ui.SessionProviderIdentity{
				Profile: modelManager.ActiveProviderName(),
				Remote:  modelManager.RemoteProvider(),
			}
			if goalRuntime != nil {
				snapshot, snapshotErr := goalRuntime.Snapshot(saveCtx)
				if snapshotErr != nil {
					return snapshotErr
				}
				stateJSON, encodeErr = ui.EncodeHeadlessGoalSessionStateWithProvider(
					ag.Messages(),
					modelName,
					cfg.AgentProfile,
					modelPinned,
					executionCursor,
					snapshot,
					ag.ContextPromptFloor(),
					providerIdentity,
				)
			} else {
				stateJSON, encodeErr = ui.EncodeHeadlessSessionStateWithProvider(
					ag.Messages(),
					modelName,
					cfg.AgentProfile,
					modelPinned,
					executionCursor,
					ag.ContextPromptFloor(),
					providerIdentity,
				)
			}
			if encodeErr != nil {
				return encodeErr
			}
			nextRevision, saveErr := saveHeadlessSessionStateCAS(
				saveCtx, dbStore, session.ID, sessionStateRevision, stateJSON,
			)
			if saveErr != nil {
				return saveErr
			}
			sessionStateRevision = nextRevision
			return nil
		}
		if err := persistHeadlessState(ctx, executionCursor); err != nil {
			leaseErr := executionLease.Close()
			var cleanupErr error
			if newSession {
				cleanupCtx, cancelCleanup := context.WithTimeout(context.Background(), time.Second)
				cleanupErr = dbStore.DeleteSession(cleanupCtx, session.ID)
				cancelCleanup()
			}
			fmt.Fprintf(os.Stderr, "sonar: save execution session before dispatch: %v\n", err)
			if cleanupFailure := errors.Join(leaseErr, cleanupErr); cleanupFailure != nil {
				fmt.Fprintf(os.Stderr, "sonar: remove incomplete execution session: %v\n", cleanupFailure)
			}
			return 1
		}
		runErr := ag.RunTurnWithOptions(ctx, out, headlessTurnID, agent.TurnOptions{Limits: goalTurnLimits})
		if goalRuntime != nil {
			pending, snapshotErr := goalRuntime.Snapshot(context.Background())
			if snapshotErr != nil {
				runErr = errors.Join(runErr, snapshotErr)
			} else if pending.PendingContinuation != nil {
				summary, evalTokens, productive := out.GoalTurnStats()
				report := goal.TurnReport{
					TurnID: pending.PendingContinuation.TurnID, Productive: runErr == nil && productive,
					Summary: summary, EvalTokens: evalTokens,
				}
				if runErr != nil {
					report.Summary = boundedHeadlessGoalError(runErr)
				}
				var unresolved *agent.UnresolvedExecutionError
				if errors.As(runErr, &unresolved) {
					report.OutcomeUnknown = true
					report.OutcomeRef = unresolved.ExecutionID
					if report.OutcomeRef == "" {
						report.OutcomeRef = pending.PendingContinuation.TurnID
					}
				}
				runErr = errors.Join(runErr, goalRuntime.RecordTurn(context.Background(), report))
			}
		}
		saveCtx, cancelSave := context.WithTimeout(context.Background(), 2*time.Second)
		finalCursor, cursorErr := headlessSnapshotExecutionCursor(saveCtx, dbStore, ag, session.ID, workspace, executionCursor)
		saveErr := persistHeadlessState(saveCtx, finalCursor)
		cancelSave()
		saveErr = errors.Join(cursorErr, saveErr)
		if runErr == nil {
			usageCtx, cancelUsage := context.WithTimeout(context.Background(), 2*time.Second)
			promptTokens, evalTokens := out.TokenUsage()
			if usageErr := recordHeadlessTokenUsage(usageCtx, dbStore, session.ID, modelName, promptTokens, evalTokens); usageErr != nil {
				fmt.Fprintf(os.Stderr, "sonar: record token usage: %v\n", usageErr)
			}
			cancelUsage()
		}
		if jsonOut != nil {
			receiptCtx, cancelReceipt := context.WithTimeout(context.Background(), 2*time.Second)
			pendingRecoveries, pendingErr := headlessPendingRecoveryCount(receiptCtx, dbStore, session.ID, workspace, executionCursor)
			cancelReceipt()
			if pendingErr != nil {
				fmt.Fprintf(os.Stderr, "sonar: inspect pending recoveries: %v\n", pendingErr)
			}
			status, stopReason := agent.TurnOutcome(runErr)
			modelCtx, cancelModel := context.WithTimeout(context.Background(), 2*time.Second)
			modelReceipt := modelManager.CurrentModelReceipt(modelCtx)
			cancelModel()
			if modelReceipt.Name == "" {
				modelReceipt.Name = modelName
			}
			receiptModel := agent.TurnReceiptModel{
				Name:     modelReceipt.Name,
				Digest:   modelReceipt.Digest,
				NumCtx:   ag.NumCtx(),
				Provider: modelReceipt.Provider,
				Remote:   modelReceipt.Remote,
			}
			if modelReceipt.OffloadKnown {
				receiptModel.Offload = &agent.TurnReceiptModelOffload{
					VRAMBytes: modelReceipt.VRAMBytes, TotalBytes: modelReceipt.TotalBytes,
				}
			}
			receipt := agent.TurnReceipt{
				RunID:  ag.ExecutionRunID(),
				TurnID: headlessTurnID,
				Actor:  externalActor,
				Session: &agent.TurnReceiptSession{
					ID: session.ID, PublicID: session.PublicID, Workspace: workspace,
				},
				Model:                receiptModel,
				Status:               status,
				StopReason:           stopReason,
				Error:                boundedReceiptError(runErr),
				ExecutionCursor:      finalCursor,
				PendingRecoveryCount: pendingRecoveries,
			}
			if goalRuntime != nil {
				// The post-turn supervision verdict tells an external loop whether
				// another explicit `goal run` would be admitted.
				decisionCtx, cancelDecision := context.WithTimeout(context.Background(), 2*time.Second)
				postIssues, postIssuesErr := headlessSupervisorIssues(decisionCtx, dbStore, session.ID, workspace)
				if postIssuesErr == nil {
					if postDecision, postErr := supervisor.Decide(decisionCtx, goalRuntime, supervisor.Observation{
						LeaseOwned: true, PersistenceReady: true, AdvisorAvailable: false,
						Manual: true, Issues: postIssues,
					}); postErr == nil {
						receipt.Decision = receiptDecision(postDecision)
					} else {
						fmt.Fprintf(os.Stderr, "sonar: post-turn supervision: %v\n", postErr)
					}
				} else {
					fmt.Fprintf(os.Stderr, "sonar: post-turn supervision: %v\n", postIssuesErr)
				}
				cancelDecision()
			}
			if err := agent.WriteTurnReceipt(os.Stdout, jsonOut.ComposeReceipt(receipt)); err != nil {
				fmt.Fprintf(os.Stderr, "sonar: %v\n", err)
				if runErr == nil && saveErr == nil {
					return 1
				}
			}
		}
		if runErr != nil {
			if saveErr != nil {
				fmt.Fprintf(os.Stderr, "sonar: save execution session after failure: %v\n", saveErr)
			}
			fmt.Fprintf(os.Stderr, "sonar: %v\n", runErr)
			return 1
		}
		if saveErr != nil {
			fmt.Fprintf(os.Stderr, "sonar: save completed execution session: %v\n", saveErr)
			return 1
		}
		return 0
	}

	cmdReg := command.NewRegistry()
	command.RegisterBuiltins(cmdReg)

	// Load custom commands from ~/.config/sonar/commands/
	if home, err := os.UserHomeDir(); err == nil {
		customDir := filepath.Join(home, ".config", "sonar", "commands")
		if err := command.RegisterCustomCommands(cmdReg, customDir); err != nil {
			fmt.Fprintf(os.Stderr, "Custom commands: %v\n", err)
		}
	}

	var agentList []string
	if agentsDir != nil {
		for _, a := range agentsDir.ListAgents() {
			agentList = append(agentList, a.Name)
		}
	}

	completer := ui.NewCompleter(cmdReg, modelList, skillMgr.Names(), agentList, registry)
	completer.UpdateProviders(modelManager.ProviderNames())

	logger, logFile, err := logging.NewSessionLogger()
	if err != nil {
		log.Printf("warning: session logger: %v", err)
	}
	if logFile != nil {
		defer func() {
			if err := logFile.Close(); err != nil {
				log.Printf("warning: close log file: %v", err)
			}
		}()
	}

	if logger != nil {
		ag.SetLogger(logger)
		// The registry is built before the logger exists, so it is handed the
		// logger here rather than at construction.
		registry.SetLogger(logger)
	}

	m := ui.New(ag, cmdReg, skillMgr, completer, modelManager, router, logger)
	m.SetConfigSourcePath(cfg.SourcePath)
	m.SetModelRoutingCatalog(cfg.Model.Models)
	m.SetModelPreferenceStore(modelPreferenceStore)
	// The prose measure is a layout constant read from many call sites with no
	// route to configuration, so it is installed once here before any frame
	// renders rather than threaded through the layout code.
	ui.SetProseCap(cfg.UI.EffectiveProseWidth())

	// Theme precedence: an explicit /theme choice outranks the config key,
	// because it is the more recent and more deliberate of the two. An
	// unreadable preference file is not worth failing startup over — the
	// default palette is always legible.
	if theme := strings.TrimSpace(cfg.UI.Theme); theme != "" && !m.SetTheme(theme) {
		log.Printf("warning: unknown ui.theme %q in %s; using the default", theme, cfg.SourcePath)
	}
	if modelPreferenceStore != nil {
		if saved, ok, themeErr := modelPreferenceStore.LoadTheme(); themeErr != nil {
			log.Printf("warning: saved theme preference unreadable: %v", themeErr)
		} else if ok && !m.SetTheme(saved) {
			log.Printf("warning: saved theme %q is not available in this build; using the default", saved)
		}
	}
	if home, homeErr := os.UserHomeDir(); homeErr != nil {
		log.Printf("warning: image attachments unavailable: %v", homeErr)
	} else if imageStore, imageErr := imageasset.NewStore(filepath.Join(home, ".config", "sonar", "images"), imageasset.DefaultLimits()); imageErr != nil {
		log.Printf("warning: image attachments unavailable: %v", imageErr)
	} else {
		m.SetImageStore(imageStore)
	}
	// Spoken output. A request the host cannot honour is reported rather than
	// silently dropped: someone who enabled voice and hears nothing deserves to
	// be told which half is missing.
	if notice := m.StartVoice(cfg.Voice); notice != "" {
		fmt.Fprintln(os.Stderr, notice)
	}
	m.SetVoiceInput(cfg.Voice.Input)
	if permChecker.SkipsApprovals() {
		m.SetApprovalPosture(ui.ApprovalPostureSkipApprovals)
	} else {
		m.SetApprovalPosture(ui.ApprovalPosturePrompted)
	}
	if goalAdvisorConfigured(servers) {
		m.SetGoalAdvisor(goaladvisor.NewCortex(registry, ag.WorkDir(), "sonar", ag))
	}
	defer func() {
		ag.Close()
		if err := m.ReleaseExecutionSessionLease(); err != nil {
			log.Printf("warning: release execution session: %v", err)
		}
	}()
	m.SetModelPinned(modelPinned)
	m.SetSessionStore(dbStore)
	if resumeRequested {
		if err := m.SetStartupSessionResume(resumeSelector); err != nil {
			fmt.Fprintf(os.Stderr, "resume: %v\n", err)
			return 2
		}
	}
	m.SetAgentProfileSource(agentsDir, baseLoadedContext, cfg.AgentProfile)
	// Bubble Tea's built-in signal handler exits before Model.Update sees the
	// event. Own SIGINT/SIGTERM so every OS shutdown follows the same
	// cancel/join/persist path as the in-app quit binding.
	p := tea.NewProgram(m, tea.WithoutSignalHandler())
	m.SetProgram(p)

	// Raw Ctrl+C remains a Bubble Tea key event; terminal-delivered SIGINT and
	// orchestrator SIGTERM are converted to the graceful shutdown message.
	sigCh := make(chan os.Signal, 1)
	signalDone := make(chan struct{})
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	go func() {
		select {
		case <-sigCh:
			// Forward one graceful request, then restore the OS default. If a
			// kernel syscall or third-party transport defeats cancellation, a
			// second signal remains an emergency termination path.
			signal.Stop(sigCh)
			p.Send(ui.ShutdownMsg{})
		case <-signalDone:
		}
	}()

	// Background initialization goroutine.
	initCtx, initCancel := context.WithCancel(context.Background())
	m.SetInitCancel(initCancel)
	initDone := make(chan struct{})

	go func() {
		defer close(initDone)

		// 1. Commit the host-owned profile and MCP scope before any interactive
		// turn can run. Failure closes MCP authority rather than leaving a
		// partially configured agent executable.
		activeAgentProfile := cfg.AgentProfile
		if err := applyInitialAgentProfile(ag, skillMgr, modelManager, agentsDir, baseLoadedContext, cfg.AgentProfile); err != nil {
			ag.DenyAllMCPTools()
			activeAgentProfile = ""
			p.Send(ui.ErrorMsg{Msg: fmt.Sprintf("agent profile: %v", err)})
		}
		admittedModel := modelManager.CurrentModel()
		if admittedModel == "" {
			admittedModel = modelName
		}

		// 2. Ping the admitted inference provider with initCtx cancellation.
		providerLabel := "Ollama (" + admittedModel + ")"
		providerID := "ollama"
		if modelManager.RemoteProvider() {
			providerID = "provider"
			providerLabel = modelManager.RemoteProviderLabel() + " (" + admittedModel + ")"
		}
		p.Send(ui.StartupStatusMsg{ID: providerID, Label: providerLabel, Status: "connecting"})
		pingCtx, cancelPing := context.WithTimeout(initCtx, 2*time.Second)
		pingErr := modelManager.PingContext(pingCtx)
		cancelPing()
		if pingErr != nil {
			if modelManager.RemoteProvider() {
				p.Send(ui.StartupStatusMsg{ID: providerID, Label: providerLabel, Status: "failed", Detail: ui.ProviderFailureCopy})
				p.Send(ui.ErrorMsg{Msg: ui.ProviderFailureCopy})
			} else {
				p.Send(ui.StartupStatusMsg{ID: providerID, Label: providerLabel, Status: "failed", Detail: pingErr.Error()})
				p.Send(ui.ErrorMsg{Msg: fmt.Sprintf("ollama: %v\ntry: ollama serve · ollama pull %s", pingErr, admittedModel)})
			}
			// Continue — non-fatal for TUI, user can see the error.
		} else {
			p.Send(ui.StartupStatusMsg{ID: providerID, Label: providerLabel, Status: "connected"})
		}
		// --resume keeps its existing full-init barrier so session restoration
		// owns the composer before any queued draft can target the wrong session.
		if !resumeRequested && initCtx.Err() == nil {
			coreModels := ui.BuildOllamaModelDescriptors(
				ollamaInventory, nil, admittedModel, cfg.Privacy.LocalOnly,
			)
			for index := range coreModels {
				coreModels[index].EffectiveContext = modelManager.ContextPolicy(coreModels[index].Name).Effective
			}
			p.Send(ui.CoreReadyMsg{
				Model:                    admittedModel,
				ModelList:                modelList,
				OllamaModels:             coreModels,
				OllamaInventoryAttempted: true,
				AgentProfile:             activeAgentProfile,
				NumCtx:                   modelManager.NumCtx(),
			})
		}

		// 3. Connect MCP servers in parallel. This no longer gates ordinary TUI
		// turns; the registry is snapshotted afresh at each turn boundary.
		if initCtx.Err() != nil {
			return
		}
		var wg sync.WaitGroup
		for _, srv := range servers {
			wg.Add(1)
			go func(s config.ServerConfig) {
				defer wg.Done()
				p.Send(ui.StartupStatusMsg{ID: "mcp:" + s.Name, Label: s.Name, Status: "connecting"})
				if initCtx.Err() != nil {
					return
				}
				toolCount, err := registry.ConnectServer(initCtx, s)
				if err != nil {
					p.Send(ui.StartupStatusMsg{ID: "mcp:" + s.Name, Label: s.Name, Status: "failed", Detail: err.Error()})
				} else {
					p.Send(ui.StartupStatusMsg{ID: "mcp:" + s.Name, Label: s.Name, Status: "connected", Detail: fmt.Sprintf("%d tools", toolCount)})
				}
			}(srv)
		}
		wg.Wait()

		if initCtx.Err() != nil {
			return
		}

		// Start background health monitoring so a crashed MCP server is
		// auto-reconnected instead of staying dead until restart. Bound to
		// initCtx, so it stops cleanly when the TUI exits.
		registry.StartHealthMonitor(initCtx, mcp.MonitorConfig{
			OnSnapshot: func(statuses []mcp.ConnectionStatus) {
				p.Send(ui.MCPStatusSnapshotMsg{Servers: projectMCPStatuses(statuses)})
			},
		}, func(s string) {
			if logger != nil {
				logger.Info("mcp health", "detail", s)
			}
		})

		// 4. ICE setup.
		var iceEnabled bool
		var iceConversations int
		var iceSessionID string
		if cfg.ICE.Enabled && workspace != "" {
			p.Send(ui.StartupStatusMsg{ID: "ice", Label: "ICE", Status: "connecting"})
			iceEngine, err := ice.NewEngine(modelManager, memStore, resolvedICEEngineConfig(cfg, workspace, iceStorePath))
			if err != nil {
				p.Send(ui.StartupStatusMsg{ID: "ice", Label: "ICE", Status: "failed", Detail: err.Error()})
			} else {
				ag.SetICEEngine(iceEngine)
				iceEnabled = true
				iceConversations = iceEngine.ScopedEntryCount()
				iceSessionID = iceEngine.SessionID()
				detail := fmt.Sprintf("%d scoped conversations", iceConversations)
				p.Send(ui.StartupStatusMsg{ID: "ice", Label: "ICE", Status: "connected", Detail: detail})
			}
		} else if cfg.ICE.Enabled {
			p.Send(ui.StartupStatusMsg{ID: "ice", Label: "ICE", Status: "failed", Detail: "workspace identity unavailable; legacy retrieval disabled"})
		}

		// 5. Collect optional results and settle the startup presentation.
		var runningModels []llm.OllamaRunningModel
		ollamaVersion := ""
		if discoveryErr == nil {
			metadataCtx, cancelMetadata := context.WithTimeout(initCtx, 2*time.Second)
			runningModels, _ = modelManager.ListRunningOllamaModels(metadataCtx)
			ollamaVersion, _ = modelManager.OllamaVersion(metadataCtx)
			cancelMetadata()
		}
		ollamaModels := ui.BuildOllamaModelDescriptors(
			ollamaInventory, runningModels, admittedModel, cfg.Privacy.LocalOnly,
		)
		for index := range ollamaModels {
			ollamaModels[index].EffectiveContext = modelManager.ContextPolicy(ollamaModels[index].Name).Effective
		}
		failedServers := registry.FailedServers()
		failedServerProjection := make([]ui.FailedServer, 0, len(failedServers))
		for _, failed := range failedServers {
			failedServerProjection = append(failedServerProjection, ui.FailedServer{
				Name: failed.Name, Reason: failed.Reason,
			})
		}
		p.Send(ui.InitCompleteMsg{
			Model:                    admittedModel,
			ModelList:                modelList,
			OllamaModels:             ollamaModels,
			OllamaVersion:            ollamaVersion,
			OllamaInventoryAttempted: true,
			AgentProfile:             activeAgentProfile,
			AgentList:                agentList,
			ToolCount:                ag.ToolCount(),
			ServerCount:              registry.ServerCount(),
			NumCtx:                   modelManager.NumCtx(),
			FailedServers:            failedServerProjection,
			MCPServers:               projectMCPStatuses(registry.ConnectionStatuses()),
			ICEEnabled:               iceEnabled,
			ICEConversations:         iceConversations,
			ICESessionID:             iceSessionID,
		})
	}()

	finalModel, runErr := p.Run()
	signal.Stop(sigCh)
	close(signalDone)
	initCancel()
	<-initDone
	if runErr != nil {
		fmt.Fprintf(os.Stderr, "tui: %v\n", runErr)
		return 1
	}
	writeSessionResumeMessage(os.Stdout, finalModel, runErr)
	return 0
}

func projectMCPStatuses(statuses []mcp.ConnectionStatus) []ui.MCPServerStatus {
	projected := make([]ui.MCPServerStatus, 0, len(statuses))
	for _, status := range statuses {
		projected = append(projected, ui.MCPServerStatus{
			Name: status.Name, Connected: status.Connected,
			ToolCount: status.ToolCount, Detail: status.LastError,
		})
	}
	return projected
}

func newRuntimeSkillManager(agentsDir *config.AgentsDir, autoLoad bool) (*skill.Manager, error) {
	if !autoLoad {
		return skill.NewManager(""), nil
	}
	if agentsDir == nil || strings.TrimSpace(agentsDir.Path) == "" {
		return nil, errors.New("shared agents directory was not loaded")
	}
	// The selected shared agents root owns profiles and skills together. Do
	// not mix it with sonar's private config/state directory.
	return skill.NewManager(filepath.Join(agentsDir.Path, "skills")), nil
}

// currentWorkspace returns the process workspace used to scope local memory.
func currentWorkspace() string {
	workDir, err := os.Getwd()
	if err != nil {
		return ""
	}
	absolute, err := filepath.Abs(workDir)
	if err != nil {
		return ""
	}
	if resolved, resolveErr := filepath.EvalSymlinks(absolute); resolveErr == nil {
		absolute = resolved
	}
	return filepath.Clean(absolute)
}

func headlessSessionTitle(prompt string) string {
	title := ""
	for _, line := range strings.Split(prompt, "\n") {
		candidate := strings.Join(strings.Fields(terminalSafeGoalText(ansi.Strip(line))), " ")
		if candidate != "" {
			title = candidate
			break
		}
	}
	if title == "" {
		title = "Headless session " + time.Now().Format("2006-01-02 15:04")
	}
	runes := []rune(title)
	if len(runes) > 72 {
		title = string(runes[:69]) + "..."
	}
	return title
}

func saveHeadlessSessionStateCAS(ctx context.Context, store *db.Store, sessionID, expectedRevision int64, stateJSON string) (int64, error) {
	record, err := store.SaveSessionStateCAS(ctx, sessionID, expectedRevision, stateJSON)
	if err != nil {
		return expectedRevision, err
	}
	return record.Revision, nil
}

func headlessSnapshotExecutionCursor(ctx context.Context, store *db.Store, ag *agent.Agent, sessionID int64, workspaceID string, current int64) (int64, error) {
	hazards, err := store.ListExecutionRecoveryHazards(ctx, sessionID, workspaceID, current, 100)
	if err != nil {
		return current, fmt.Errorf("inspect execution projection: %w", err)
	}
	messages := ag.Messages()
	for _, state := range hazards {
		if state.Latest.Type != executionpkg.EventCompleted && state.Latest.Type != executionpkg.EventFailed {
			return current, fmt.Errorf(
				"execution %s remains %s/%s and cannot cross the headless snapshot boundary",
				state.Identity.ExecutionID, state.Latest.Type, state.Identity.EffectClass,
			)
		}
		projected := false
		for _, message := range messages {
			resultContent := message.Content
			if message.DurableContent != "" {
				resultContent = message.DurableContent
			}
			if message.Role == "tool" &&
				message.ToolCallID == state.Identity.CanonicalCallID &&
				executionpkg.HashText(resultContent) == state.Latest.ResultSHA256 {
				projected = true
				break
			}
		}
		if !projected {
			return current, fmt.Errorf("%s effect %s is absent from the headless snapshot", state.Latest.Type, state.Identity.ExecutionID)
		}
	}
	latest, err := store.LatestExecutionEventID(ctx, sessionID, workspaceID)
	if err != nil {
		return current, fmt.Errorf("read execution cursor: %w", err)
	}
	return latest, nil
}

func applyWorkspaceIgnore(ag *agent.Agent, workDir string) error {
	ignore, err := config.LoadIgnoreFileWithError(workDir)
	if err != nil {
		return err
	}
	if ag != nil {
		ag.SetWorkspacePolicy(workDir, ignore.Raw())
	}
	return nil
}
