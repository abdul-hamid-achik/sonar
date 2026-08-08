package command

import (
	"errors"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/abdul-hamid-achik/sonar/internal/config"
)

// RegisterBuiltins adds all built-in slash commands to the registry.
func RegisterBuiltins(r *Registry) {
	registerGoalActions(r)
	registerScopeActions(r)
	registerImageActions(r)
	registerPermissionsActions(r)

	r.Register(&Command{
		Name:        "help",
		Aliases:     []string{"h", "?"},
		Description: "Show help overlay with shortcuts and commands",
		Usage:       "/help",
		Handler: func(_ *Context, args []string) Result {
			if err := noArguments(args, "/help"); err != "" {
				return Result{Error: err}
			}
			return Result{Action: ActionShowHelp}
		},
	})

	r.Register(&Command{
		Name:        "clear",
		Aliases:     []string{"new"},
		Description: "Clear conversation history",
		Usage:       "/clear",
		Handler: func(_ *Context, args []string) Result {
			if err := noArguments(args, "/clear"); err != "" {
				return Result{Error: err}
			}
			return Result{
				Text:   "Conversation cleared.",
				Action: ActionClear,
			}
		},
	})

	r.Register(&Command{
		Name:        "plan",
		Description: "Open a guided read-only planning form",
		Usage:       "/plan [task]",
		Handler: func(_ *Context, args []string) Result {
			return Result{
				Action: ActionOpenPlan,
				Data:   strings.TrimSpace(strings.Join(args, " ")),
			}
		},
	})

	r.Register(&Command{
		Name:        "goal",
		Aliases:     []string{"g"},
		Description: "Create, inspect, pause, resume, budget, or drop a durable goal",
		Usage:       "/goal [<duration> <prompt>|new [objective]|show|pause|resume|budget|drop]",
		Handler: func(ctx *Context, args []string) Result {
			if ctx == nil {
				ctx = &Context{}
			}
			if len(args) == 0 {
				if ctx.GoalConfigured {
					return Result{Action: ActionShowGoal}
				}
				return Result{Action: ActionOpenGoal}
			}

			if spec, ok := r.MatchAction("goal", args[0]); ok {
				if spec.ID == GoalActionNew {
					if state := resolveActionState(spec, ctx); !state.Enabled {
						return Result{Error: state.DisabledReason}
					}
					promptArgs := args[1:]
					request, err := parseGoalRequest(promptArgs)
					if err != nil {
						return Result{Error: err.Error()}
					}
					if request == nil {
						return Result{Action: spec.Action}
					}
					return Result{Action: spec.Action, Data: request.Prompt, Goal: request}
				}
				if len(args) != 1 {
					return Result{Error: "usage: " + spec.CommandText()}
				}
				if state := resolveActionState(spec, ctx); !state.Enabled {
					return Result{Error: state.DisabledReason}
				}
				return Result{Action: spec.Action}
			}
			switch args[0] {
			default:
				// A free-form suffix is the shortest path from `/goal ship the
				// release` to the reviewed form. Lifecycle subcommands remain
				// closed so a flag typo cannot become a state transition.
				if len(args) == 1 && strings.HasPrefix(args[0], "-") {
					return Result{Error: "usage: /goal [new [objective]|show|pause|resume|budget|drop]"}
				}
				if spec, exists := r.Action(GoalActionNew); exists {
					if state := resolveActionState(spec, ctx); !state.Enabled {
						return Result{Error: state.DisabledReason}
					}
				}
				request, err := parseGoalRequest(args)
				if err != nil {
					return Result{Error: err.Error()}
				}
				return Result{Action: ActionOpenGoal, Data: request.Prompt, Goal: request}
			}
		},
	})

	r.Register(&Command{
		Name:        "model",
		Aliases:     []string{"m", "models", "ml"},
		Description: "Show, switch, or list models",
		Usage:       "/model [name|list|auto]",
		Handler: func(ctx *Context, args []string) Result {
			if len(args) == 0 {
				return Result{Action: ActionShowModelPicker}
			}
			if len(args) != 1 {
				return Result{Error: "usage: /model [name|list|auto]"}
			}

			switch args[0] {
			case "auto":
				return Result{
					Text:   "Automatic model routing enabled",
					Action: ActionEnableAutoModel,
				}
			case "list", "ls":
				var b strings.Builder
				b.WriteString("Available models:\n")
				for _, m := range ctx.ModelList {
					marker := "  "
					if m == ctx.Model {
						marker = "* "
					}
					fmt.Fprintf(&b, "  %s%s\n", marker, m)
				}
				b.WriteString("\n* = current")
				return Result{Text: b.String()}

			default:
				for _, m := range ctx.ModelList {
					if m == args[0] {
						return Result{
							Text:   fmt.Sprintf("Switching to model: %s", m),
							Action: ActionSwitchModel,
							Data:   m,
						}
					}
				}
				return Result{Error: fmt.Sprintf("Unknown model: %s (use /model list to see available)", args[0])}
			}
		},
	})

	r.Register(&Command{
		Name:        "theme",
		Aliases:     []string{"themes", "colors"},
		Description: "Show, switch, or list color schemes",
		Usage:       "/theme [name|list]",
		Handler: func(ctx *Context, args []string) Result {
			if len(args) == 0 {
				return Result{Action: ActionShowThemePicker}
			}
			if len(args) != 1 {
				return Result{Error: "usage: /theme [name|list]"}
			}
			switch args[0] {
			case "list", "ls":
				var b strings.Builder
				b.WriteString("Available themes:\n")
				for _, id := range ctx.ThemeList {
					marker := "  "
					if id == ctx.Theme {
						marker = "* "
					}
					fmt.Fprintf(&b, "  %s%s\n", marker, id)
				}
				b.WriteString("\n* = current")
				return Result{Text: b.String()}
			default:
				// Validation lives with the registry so a typo is reported here
				// instead of silently repainting in the default scheme.
				for _, id := range ctx.ThemeList {
					if id == args[0] {
						return Result{Action: ActionSwitchTheme, Data: id}
					}
				}
				return Result{Error: fmt.Sprintf("Unknown theme: %s (use /theme list to see available)", args[0])}
			}
		},
	})

	r.Register(&Command{
		Name:        "provider",
		Aliases:     []string{"providers", "prov"},
		Description: "Show or switch inference provider profiles",
		Usage:       "/provider [name|list]",
		Handler: func(ctx *Context, args []string) Result {
			if len(args) > 1 {
				return Result{Error: "usage: /provider [name|list]"}
			}
			names := ctx.ProviderList
			if len(names) == 0 {
				names = []string{"ollama"}
			}
			if len(args) == 0 {
				return Result{Action: ActionShowProviderPicker}
			}
			if args[0] == "list" || args[0] == "ls" {
				var b strings.Builder
				b.WriteString("Inference providers:\n")
				for _, name := range names {
					marker := "  "
					if name == ctx.Provider {
						marker = "* "
					}
					fmt.Fprintf(&b, "  %s%s\n", marker, name)
				}
				if ctx.Provider != "" {
					fmt.Fprintf(&b, "\n* = current (%s)\n", ctx.Provider)
				} else {
					b.WriteString("\n* = current\n")
				}
				b.WriteString("Keys stay in the process env (tvault run --only KEY).")
				return Result{Text: b.String()}
			}
			target := args[0]
			for _, name := range names {
				if name == target || strings.EqualFold(name, target) {
					return Result{
						Text:   fmt.Sprintf("Switching to provider: %s", name),
						Action: ActionSwitchProvider,
						Data:   name,
					}
				}
			}
			// A provider type is selectable even when no profile is named after
			// it (the flat catalog). config.IsKnownProviderType is the single
			// answer to which types exist; the list this replaced predated the
			// catalog and rejected "deepseek" — sonar's own default provider —
			// along with the other 39 catalog entries. The switch layer already
			// resolves a bare type through the same predicate, so this gate was
			// the only thing standing in the way.
			if strings.TrimSpace(target) != "" && config.IsKnownProviderType(target) {
				return Result{
					Text:   fmt.Sprintf("Switching to provider: %s", target),
					Action: ActionSwitchProvider,
					Data:   target,
				}
			}
			return Result{Error: fmt.Sprintf("Unknown provider: %s (use /provider list)", target)}
		},
	})

	r.Register(&Command{
		Name:        "recover",
		Description: "Review a paused execution and record typed evidence",
		Usage:       "/recover",
		Handler: func(_ *Context, args []string) Result {
			if len(args) != 0 {
				return Result{Error: "usage: /recover"}
			}
			return Result{Action: ActionRecoverExecution}
		},
	})

	r.Register(&Command{
		Name:        "agent",
		Aliases:     []string{"a"},
		Description: "Show or switch agent profile",
		Usage:       "/agent [name|list]",
		Handler: func(ctx *Context, args []string) Result {
			if len(args) > 1 {
				return Result{Error: "usage: /agent [name|list]"}
			}
			if len(args) == 0 || args[0] == "list" {
				var b strings.Builder
				if len(ctx.AgentList) == 0 {
					b.WriteString("No agent profiles found in ~/.agents/agents/")
					return Result{Text: b.String()}
				}
				b.WriteString("Available agent profiles:\n")
				for _, a := range ctx.AgentList {
					marker := "  "
					if a == ctx.AgentProfile {
						marker = "* "
					}
					fmt.Fprintf(&b, "  %s%s\n", marker, a)
				}
				b.WriteString("\n* = current")
				return Result{Text: b.String()}
			}

			for _, a := range ctx.AgentList {
				if a == args[0] {
					return Result{
						Text:   fmt.Sprintf("Switching to agent: %s", a),
						Action: ActionSwitchAgent,
						Data:   a,
					}
				}
			}
			return Result{Error: fmt.Sprintf("Unknown agent: %s (use /agent list to see available)", args[0])}
		},
	})

	r.Register(&Command{
		Name:        "load",
		Aliases:     []string{"l"},
		Description: "Load a markdown file as context",
		Usage:       "/load <path>",
		Handler: func(_ *Context, args []string) Result {
			if len(args) == 0 {
				return Result{Error: "Usage: /load <path>"}
			}
			path := expandHomePath(strings.TrimSpace(strings.Join(args, " ")))
			return Result{
				Text:   fmt.Sprintf("Loading context from: %s", path),
				Action: ActionLoadContext,
				Data:   path,
			}
		},
	})

	r.Register(&Command{
		Name:        "image",
		Aliases:     []string{"attach"},
		Description: "Attach or manage pending and historical images",
		Usage:       "/image <path>|list|clear|forget-history",
		Handler: func(_ *Context, args []string) Result {
			if len(args) == 0 {
				return Result{Error: "usage: /image <path>|list|clear|forget-history"}
			}

			if spec, ok := r.MatchAction("image", args[0]); ok {
				if len(args) != 1 {
					return Result{Error: "usage: " + spec.CommandText()}
				}
				return Result{Action: spec.Action}
			}

			path := expandHomePath(strings.TrimSpace(strings.Join(args, " ")))
			if path == "" {
				return Result{Error: "usage: /image <path>|list|clear|forget-history"}
			}
			for _, r := range path {
				if unicode.IsControl(r) {
					return Result{Error: "image path cannot contain control characters"}
				}
			}
			return Result{Action: ActionAttachImage, Data: path}
		},
	})

	r.Register(&Command{
		Name:        "scope",
		Description: "List or manage temporary external read-only grants",
		Usage:       "/scope [list|add-read <directory>|remove-read <path>|clear-read]",
		Handler: func(ctx *Context, args []string) Result {
			if ctx == nil {
				ctx = &Context{}
			}
			if len(args) == 0 || (len(args) == 1 && (args[0] == "list" || args[0] == "ls")) {
				grants := append([]ReadGrantInfo(nil), ctx.ReadGrants...)
				if len(grants) == 0 {
					for _, root := range ctx.ReadRoots {
						grants = append(grants, ReadGrantInfo{Path: root, Kind: "directory"})
					}
				}
				if len(grants) == 0 {
					return Result{Text: "No temporary external read-only grants are active. Add a directory with /scope add-read <directory>; exact-file access is requested automatically when an ordinary prompt names an external file."}
				}
				sort.SliceStable(grants, func(i, j int) bool {
					if grants[i].Path != grants[j].Path {
						return grants[i].Path < grants[j].Path
					}
					return grants[i].Kind < grants[j].Kind
				})
				var b strings.Builder
				fmt.Fprintf(&b, "Temporary read-only grants (%d):\n", len(grants))
				for _, grant := range grants {
					kind := "directory"
					if grant.Kind == "exact_file" {
						kind = "exact file"
					}
					fmt.Fprintf(&b, "  - %s · %s\n", kind, grant.Path)
				}
				b.WriteString("\nWrites remain confined to the working directory. Exact-file grants never include siblings. These grants are not persisted.")
				return Result{Text: b.String()}
			}

			spec, ok := r.MatchAction("scope", args[0])
			if !ok {
				return Result{Error: "usage: /scope [list|add-read <directory>|remove-read <path>|clear-read]"}
			}
			switch spec.ID {
			case ScopeActionAddRead:
				if len(args) < 2 {
					return Result{Error: "usage: " + spec.CommandText() + " <directory>"}
				}
				path := expandHomePath(strings.TrimSpace(strings.Join(args[1:], " ")))
				if path == "" {
					return Result{Error: "usage: " + spec.CommandText() + " <directory>"}
				}
				return Result{Action: spec.Action, Data: path}
			case ScopeActionRemoveRead:
				if len(args) < 2 {
					return Result{Error: "usage: " + spec.CommandText() + " <path>"}
				}
				path := expandHomePath(strings.TrimSpace(strings.Join(args[1:], " ")))
				if path == "" {
					return Result{Error: "usage: " + spec.CommandText() + " <path>"}
				}
				return Result{Action: spec.Action, Data: path}
			case ScopeActionClearRead:
				if len(args) != 1 {
					return Result{Error: "usage: " + spec.CommandText()}
				}
				return Result{Action: spec.Action}
			default:
				return Result{Error: "unsupported scope action"}
			}
		},
	})

	r.Register(&Command{
		Name:        "permissions",
		Aliases:     []string{"perms"},
		Description: "Show approval posture, session grants, and workspace rules",
		Usage:       "/permissions [panel|audit|export [path]|import [--replace] <path>|accept-edits on|off|clear|clear-rules|revoke|allow-*|forget-*]",
		Handler: func(ctx *Context, args []string) Result {
			if ctx == nil {
				ctx = &Context{}
			}
			if len(args) == 0 || (len(args) == 1 && (args[0] == "list" || args[0] == "ls" || args[0] == "status" || args[0] == "rules")) {
				return Result{Text: formatPermissionsStatus(ctx)}
			}
			if args[0] == "panel" || args[0] == "ui" || args[0] == "manage" {
				if len(args) != 1 {
					return Result{Error: "usage: /permissions panel"}
				}
				return Result{Action: ActionPermissionsPanel}
			}
			if args[0] == "audit" {
				if len(args) != 1 {
					return Result{Error: "usage: /permissions audit"}
				}
				return Result{Action: ActionPermissionsAudit}
			}
			if args[0] == "export" {
				path := ""
				if len(args) > 1 {
					path = strings.Join(args[1:], " ")
				}
				return Result{Action: ActionPermissionsExport, Data: path}
			}
			if args[0] == "import" {
				if len(args) < 2 {
					return Result{Error: "usage: /permissions import [--replace] <path>"}
				}
				replace := false
				rest := args[1:]
				if rest[0] == "--replace" || rest[0] == "replace" {
					replace = true
					rest = rest[1:]
				}
				if len(rest) == 0 {
					return Result{Error: "usage: /permissions import [--replace] <path>"}
				}
				path := strings.Join(rest, " ")
				data := path
				if replace {
					data = "replace|" + path
				}
				return Result{Action: ActionPermissionsImport, Data: data}
			}
			if args[0] == "clear-rules" {
				if len(args) != 1 {
					return Result{Error: "usage: /permissions clear-rules"}
				}
				return Result{Action: ActionPermissionsClearRules}
			}
			// accept-edits on|off is a two-token form handled before action match
			// so "on"/"off" are not registered as independent slash actions.
			if args[0] == "accept-edits" || args[0] == "accept_edits" {
				if len(args) != 2 {
					return Result{Error: "usage: /permissions accept-edits on|off"}
				}
				switch strings.ToLower(strings.TrimSpace(args[1])) {
				case "on", "true", "1":
					return Result{Action: ActionPermissionsAcceptEdits, Data: "on"}
				case "off", "false", "0":
					return Result{Action: ActionPermissionsAcceptEdits, Data: "off"}
				default:
					return Result{Error: "usage: /permissions accept-edits on|off"}
				}
			}
			// allow-bash / forget-bash / allow-path take the remainder (may contain spaces).
			switch args[0] {
			case "allow-bash":
				if len(args) < 2 {
					return Result{Error: "usage: /permissions allow-bash <pattern>  (e.g. go test  or  git status *)"}
				}
				return Result{Action: ActionPermissionsAllowBash, Data: strings.Join(args[1:], " ")}
			case "forget-bash":
				if len(args) < 2 {
					return Result{Error: "usage: /permissions forget-bash <pattern>"}
				}
				return Result{Action: ActionPermissionsForgetBash, Data: strings.Join(args[1:], " ")}
			case "allow-mcp":
				if len(args) != 2 {
					return Result{Error: "usage: /permissions allow-mcp <server__tool>"}
				}
				return Result{Action: ActionPermissionsAllowMCP, Data: strings.TrimSpace(args[1])}
			case "forget-mcp":
				if len(args) != 2 {
					return Result{Error: "usage: /permissions forget-mcp <server__tool>"}
				}
				return Result{Action: ActionPermissionsForgetMCP, Data: strings.TrimSpace(args[1])}
			case "allow-server":
				if len(args) != 2 {
					return Result{Error: "usage: /permissions allow-server <server>  (every tool, approval only)"}
				}
				return Result{Action: ActionPermissionsAllowMCPServer, Data: strings.TrimSpace(args[1])}
			case "forget-server":
				if len(args) != 2 {
					return Result{Error: "usage: /permissions forget-server <server>"}
				}
				return Result{Action: ActionPermissionsForgetMCPServer, Data: strings.TrimSpace(args[1])}
			case "allow-path":
				if len(args) < 2 {
					return Result{Error: "usage: /permissions allow-path <path>"}
				}
				return Result{Action: ActionPermissionsAllowPath, Data: strings.Join(args[1:], " ")}
			case "forget-path":
				if len(args) < 2 {
					return Result{Error: "usage: /permissions forget-path <path>"}
				}
				return Result{Action: ActionPermissionsForgetPath, Data: strings.Join(args[1:], " ")}
			}
			spec, ok := r.MatchAction("permissions", args[0])
			if !ok {
				return Result{Error: "usage: /permissions [panel|audit|export|import|accept-edits|clear|clear-rules|revoke|allow-*|forget-*]"}
			}
			switch spec.ID {
			case PermissionsActionClear:
				if len(args) != 1 {
					return Result{Error: "usage: " + spec.CommandText()}
				}
				return Result{Action: spec.Action}
			case PermissionsActionRevoke:
				if len(args) > 2 {
					return Result{Error: "usage: " + spec.CommandText() + " [tool]"}
				}
				tool := ""
				if len(args) == 2 {
					tool = strings.TrimSpace(args[1])
				}
				return Result{Action: spec.Action, Data: tool}
			default:
				return Result{Error: "unsupported permissions action"}
			}
		},
	})

	r.Register(&Command{
		Name:        "unload",
		Description: "Remove loaded context file",
		Usage:       "/unload",
		Handler: func(ctx *Context, args []string) Result {
			if err := noArguments(args, "/unload"); err != "" {
				return Result{Error: err}
			}
			if ctx.LoadedFile == "" {
				return Result{Text: "No context file loaded."}
			}
			return Result{
				Text:   "Context unloaded.",
				Action: ActionUnloadContext,
			}
		},
	})

	r.Register(&Command{
		Name:        "skill",
		Aliases:     []string{"sk"},
		Description: "Manage skills (list, activate, deactivate)",
		Usage:       "/skill [list|activate|deactivate] [name]",
		Handler: func(ctx *Context, args []string) Result {
			if len(args) == 0 {
				return skillList(ctx)
			}
			if args[0] == "list" {
				if len(args) != 1 {
					return Result{Error: "usage: /skill list"}
				}
				return skillList(ctx)
			}
			if len(args) != 2 {
				return Result{Error: "usage: /skill [list|activate|deactivate] <name>"}
			}
			switch args[0] {
			case "activate", "on":
				return Result{
					Text:   fmt.Sprintf("Activated skill: %s", args[1]),
					Action: ActionActivateSkill,
					Data:   args[1],
				}
			case "deactivate", "off":
				return Result{
					Text:   fmt.Sprintf("Deactivated skill: %s", args[1]),
					Action: ActionDeactivateSkill,
					Data:   args[1],
				}
			default:
				return Result{Error: fmt.Sprintf("Unknown skill action: %s (use list, activate, or deactivate)", args[0])}
			}
		},
	})

	r.Register(&Command{
		Name:        "servers",
		Description: "List MCP server connection status",
		Usage:       "/servers",
		Handler: func(ctx *Context, args []string) Result {
			if err := noArguments(args, "/servers"); err != "" {
				return Result{Error: err}
			}
			if len(ctx.Servers) == 0 && len(ctx.ServerNames) == 0 {
				return Result{Text: "No MCP servers configured or discovered."}
			}
			servers := append([]ServerInfo(nil), ctx.Servers...)
			if len(servers) == 0 {
				for _, name := range ctx.ServerNames {
					servers = append(servers, ServerInfo{Name: name, Connected: true})
				}
			}
			sort.SliceStable(servers, func(i, j int) bool {
				return strings.ToLower(servers[i].Name) < strings.ToLower(servers[j].Name)
			})
			var b strings.Builder
			fmt.Fprintf(&b, "MCP servers (%d):\n", len(servers))
			for _, server := range servers {
				state := "unavailable"
				if server.Connected {
					state = "connected"
				}
				fmt.Fprintf(&b, "  - %s · %s", server.Name, state)
				if server.Connected && server.ToolCount > 0 {
					fmt.Fprintf(&b, " · %d tools", server.ToolCount)
				}
				b.WriteByte('\n')
			}
			if ctx.MCPToolCount > 0 {
				fmt.Fprintf(&b, "\nMCP tools available: %d", ctx.MCPToolCount)
			} else if ctx.ToolCount > 0 {
				fmt.Fprintf(&b, "\nTotal tools available: %d", ctx.ToolCount)
			}
			return Result{Text: b.String()}
		},
	})

	r.Register(&Command{
		Name:        "mcp",
		Description: "Manage MCP server connections",
		Usage:       "/mcp [reconnect <name> | <name>]",
		Handler: func(ctx *Context, args []string) Result {
			if len(args) >= 2 && args[0] == "reconnect" {
				name := args[1]
				found := false
				for _, s := range ctx.Servers {
					if strings.EqualFold(s.Name, name) {
						found = true
						break
					}
				}
				if !found {
					return Result{Error: fmt.Sprintf("unknown MCP server %q", name)}
				}
				return Result{Action: ActionMCPReconnect, Data: name, Text: fmt.Sprintf("Reconnecting %s…", name)}
			}
			if len(args) == 1 {
				name := args[0]
				for _, s := range ctx.Servers {
					if strings.EqualFold(s.Name, name) {
						var b strings.Builder
						fmt.Fprintf(&b, "MCP server: %s\n", s.Name)
						if s.Connected {
							fmt.Fprintf(&b, "  Status: connected · %d tools\n", s.ToolCount)
						} else {
							b.WriteString("  Status: unavailable\n")
						}
						if s.Detail != "" {
							fmt.Fprintf(&b, "  Detail: %s\n", s.Detail)
						}
						if !s.Connected {
							b.WriteString("\nReconnect with: /mcp reconnect " + s.Name)
						}
						return Result{Text: b.String()}
					}
				}
				return Result{Error: fmt.Sprintf("unknown MCP server %q", name)}
			}
			if len(args) > 0 {
				return Result{Error: "usage: /mcp [reconnect <name> | <name>]"}
			}
			if len(ctx.Servers) == 0 {
				return Result{Text: "No MCP servers configured. Add servers in sonar.yaml or the XDG config."}
			}
			servers := append([]ServerInfo(nil), ctx.Servers...)
			sort.SliceStable(servers, func(i, j int) bool {
				return strings.ToLower(servers[i].Name) < strings.ToLower(servers[j].Name)
			})
			var b strings.Builder
			connected := 0
			for _, s := range servers {
				if s.Connected {
					connected++
				}
			}
			fmt.Fprintf(&b, "MCP servers · %d/%d connected\n\n", connected, len(servers))
			for _, s := range servers {
				if s.Connected {
					fmt.Fprintf(&b, "  ✓ %s · %d tools\n", s.Name, s.ToolCount)
				} else {
					fmt.Fprintf(&b, "  ✗ %s · unavailable", s.Name)
					if s.Detail != "" {
						detail := s.Detail
						if len(detail) > 60 {
							detail = detail[:57] + "..."
						}
						fmt.Fprintf(&b, " · %s", detail)
					}
					b.WriteString("\n")
				}
			}
			b.WriteString("\nDetails: /mcp <name> · Reconnect: /mcp reconnect <name>")
			return Result{Text: b.String()}
		},
	})

	r.Register(&Command{
		Name:        "agents",
		Description: "Show every subagent: status, activity, and its session",
		Usage:       "/agents [doctor|spawn <provider> <prompt>]",
		Handler: func(ctx *Context, args []string) Result {
			if len(args) == 0 {
				return Result{Action: ActionSubagentsStatus}
			}
			switch args[0] {
			case "doctor":
				if len(args) != 1 {
					return Result{Error: "usage: /agents doctor"}
				}
				return Result{Action: ActionSubagentsDoctor}
			case "spawn":
				if len(args) < 3 {
					return Result{Error: "usage: /agents spawn <sonar|claude|codex|grok> <prompt>"}
				}
				return Result{Action: ActionSubagentsSpawn, Data: args[1] + " " + strings.Join(args[2:], " ")}
			default:
				return Result{Error: "usage: /agents [doctor|spawn <provider> <prompt>]"}
			}
		},
	})

	r.Register(&Command{
		Name:        "tools",
		Description: "Browse discovered MCP tools",
		Usage:       "/tools [server]",
		Handler: func(ctx *Context, args []string) Result {
			if len(args) > 1 {
				return Result{Error: "usage: /tools [server]"}
			}
			tools := ctx.MCPTools
			if len(args) == 1 {
				server := args[0]
				var filtered []ToolSummary
				for _, t := range tools {
					if strings.EqualFold(t.Server, server) {
						filtered = append(filtered, t)
					}
				}
				tools = filtered
				if len(tools) == 0 {
					return Result{Text: fmt.Sprintf("No tools discovered for server %q.", server)}
				}
			}
			if len(tools) == 0 {
				return Result{Text: "No MCP tools discovered. Connect MCP servers first (/mcp)."}
			}
			sort.SliceStable(tools, func(i, j int) bool {
				if tools[i].Server != tools[j].Server {
					return tools[i].Server < tools[j].Server
				}
				return tools[i].Name < tools[j].Name
			})
			var b strings.Builder
			fmt.Fprintf(&b, "MCP tools (%d discovered)\n\n", len(tools))
			currentServer := ""
			for _, t := range tools {
				if t.Server != currentServer {
					currentServer = t.Server
					fmt.Fprintf(&b, "  [%s]\n", currentServer)
				}
				name := t.Name
				if idx := strings.Index(name, "__"); idx > 0 {
					name = name[idx+2:]
				}
				if t.Description != "" {
					fmt.Fprintf(&b, "    %s — %s\n", name, t.Description)
				} else {
					fmt.Fprintf(&b, "    %s\n", name)
				}
			}
			return Result{Text: b.String()}
		},
	})

	r.Register(&Command{
		Name:        "ice",
		Description: "Show Infinite Context Engine status",
		Usage:       "/ice",
		Handler: func(ctx *Context, args []string) Result {
			if err := noArguments(args, "/ice"); err != "" {
				return Result{Error: err}
			}
			if !ctx.ICEEnabled {
				return Result{Text: "ICE is not enabled. It is on by default; check that Ollama is running and a workspace is available. To explicitly disable it, add `ice: {enabled: false}` to your config.yaml"}
			}
			var b strings.Builder
			b.WriteString("Infinite Context Engine (ICE)\n")
			fmt.Fprintf(&b, "  Status:        enabled\n")
			fmt.Fprintf(&b, "  Conversations: %d stored\n", ctx.ICEConversations)
			fmt.Fprintf(&b, "  Session ID:    %s\n", ctx.ICESessionID)
			fmt.Fprintf(&b, "  Embed model:   nomic-embed-text\n")
			return Result{Text: b.String()}
		},
	})

	r.Register(&Command{
		Name:        "memory",
		Aliases:     []string{"mem", "memories"},
		Description: "View and manage persistent memories",
		Usage:       "/memory [delete <id>]",
		Handler: func(ctx *Context, args []string) Result {
			if len(args) >= 2 && args[0] == "delete" {
				id, err := strconv.Atoi(args[1])
				if err != nil {
					return Result{Error: fmt.Sprintf("invalid memory ID %q: must be a number", args[1])}
				}
				return Result{Action: ActionDeleteMemory, Text: fmt.Sprintf("%d", id)}
			}
			if len(args) > 0 {
				return Result{Error: "usage: /memory [delete <id>]"}
			}
			if ctx == nil || ctx.MemoryCount == 0 {
				return Result{Text: "No memories stored yet. Memories are saved automatically from conversations (auto) or by asking the agent to remember something."}
			}
			var b strings.Builder
			fmt.Fprintf(&b, "Persistent Memories (%d stored)\n\n", ctx.MemoryCount)
			for _, m := range ctx.Memories {
				origin := "manual"
				if m.Auto {
					origin = "auto"
				}
				tags := ""
				if len(m.Tags) > 0 {
					tags = " [" + strings.Join(m.Tags, ", ") + "]"
				}
				content := m.Content
				if len(content) > 80 {
					content = content[:77] + "..."
				}
				fmt.Fprintf(&b, "  #%d  %s  (%s)%s\n", m.ID, content, origin, tags)
			}
			b.WriteString("\nDelete with: /memory delete <id>")
			return Result{Text: b.String()}
		},
	})

	r.Register(&Command{
		Name:        "sessions",
		Aliases:     []string{"ss", "resume"},
		Description: "Browse and restore saved sessions",
		Usage:       "/sessions",
		Handler: func(_ *Context, args []string) Result {
			if err := noArguments(args, "/sessions"); err != "" {
				return Result{Error: err}
			}
			return Result{Action: ActionShowSessions}
		},
	})

	r.Register(&Command{
		Name:        "artifacts",
		Aliases:     []string{"artifact"},
		Description: "List durable artifacts saved in this session",
		Usage:       "/artifacts",
		Handler: func(ctx *Context, args []string) Result {
			if len(args) != 0 {
				return Result{Error: "usage: /artifacts"}
			}
			if ctx == nil || len(ctx.Artifacts) == 0 {
				return Result{Text: "No saved artifacts in this session."}
			}

			artifacts := ctx.Artifacts
			truncated := ctx.ArtifactsTruncated
			if len(artifacts) > MaxContextArtifacts {
				artifacts = artifacts[:MaxContextArtifacts]
				truncated = true
			}
			var b strings.Builder
			if truncated {
				fmt.Fprintf(&b, "Saved artifacts (%d shown; more omitted):\n", len(artifacts))
			} else {
				fmt.Fprintf(&b, "Saved artifacts (%d):\n", len(artifacts))
			}
			for _, artifact := range artifacts {
				fileLabel := "files"
				if artifact.FileCount == 1 {
					fileLabel = "file"
				}
				fmt.Fprintf(&b, "  %s\n", artifact.URI)
				fmt.Fprintf(&b, "    %d %s · %d bytes · created %s\n", artifact.FileCount, fileLabel, artifact.TotalBytes, artifact.CreatedAt)
				fmt.Fprintf(&b, "    Content SHA-256 (full): %s\n", artifact.ContentSHA256)
				if artifact.SecretsWarning {
					b.WriteString("    Warning: potential secrets need review.\n")
				}
				if artifact.IndexingFailed {
					b.WriteString("    Indexing: incomplete.\n")
				}
			}
			return Result{Text: strings.TrimSuffix(b.String(), "\n")}
		},
	})

	r.Register(&Command{
		Name:        "changes",
		Description: "List files modified by the agent this session",
		Usage:       "/changes",
		Handler: func(ctx *Context, args []string) Result {
			if err := noArguments(args, "/changes"); err != "" {
				return Result{Error: err}
			}
			if len(ctx.FileChanges) == 0 {
				return Result{Text: "No files modified this session."}
			}
			var b strings.Builder
			fmt.Fprintf(&b, "Files modified (%d):\n", len(ctx.FileChanges))
			paths := make([]string, 0, len(ctx.FileChanges))
			for path := range ctx.FileChanges {
				paths = append(paths, path)
			}
			sort.Strings(paths)
			for _, path := range paths {
				count := ctx.FileChanges[path]
				if count > 1 {
					fmt.Fprintf(&b, "  ✎ %s (%dx)\n", path, count)
				} else {
					fmt.Fprintf(&b, "  ✎ %s\n", path)
				}
			}
			return Result{Text: b.String()}
		},
	})

	r.Register(&Command{
		Name:        "commit",
		Aliases:     []string{"ci"},
		Description: "Generate commit message from staged changes and commit",
		Handler: func(_ *Context, args []string) Result {
			return Result{Action: ActionCommit, Data: strings.Join(args, " ")}
		},
	})

	// Dictation has a key binding too, and this exists because a key binding is
	// not reachable everywhere. alt+<letter> never leaves a stock macOS terminal
	// — Option composes a character instead — and where it does, a terminal
	// multiplexer or a leader key can claim it first. A slash command survives
	// both, and completion makes it discoverable without reading the help.
	r.Register(&Command{
		Name:        "voice",
		Aliases:     []string{"listen", "mic"},
		Description: "Dictate into the composer, or inspect and tune spoken output",
		Usage:       "/voice [on|off|view|status|voices|test|profile|provider|model|speak_when|rate|voice|pronounce|<channel> on|off]",
		Handler: func(_ *Context, args []string) Result {
			if len(args) == 0 {
				return Result{Action: ActionVoiceInput}
			}
			switch verb := strings.ToLower(args[0]); verb {
			case "status", "check", "doctor":
				return Result{Action: ActionVoiceStatus}
			case "voices":
				return Result{Action: ActionVoiceVoices}
			case "test", "sample", "preview":
				return Result{Action: ActionVoiceTest}
			case "view", "stage", "screen":
				return Result{Action: ActionVoiceStage}
			case "profile", "provider", "model", "input", "speak_when", "speakwhen", "rate", "voice", "pronounce":
				// Settings, as opposed to the channel toggles below. The UI owns
				// what each one accepts — it is the side that has to rebuild a
				// synthesizer when one changes — so the arguments travel as a
				// verb and a remainder rather than being validated twice.
				return Result{
					Action: ActionVoiceSetting,
					Data:   strings.Join(append([]string{verb}, args[1:]...), " "),
				}
			case "on", "off":
				// The master switch. It is a verb rather than a channel because
				// it decides whether there is a synthesizer at all, and because
				// "on"/"off" alone is what anybody types first.
				return Result{Action: ActionVoiceEnable, Data: verb}
			default:
				// Anything else is read as a channel name. The UI owns the list
				// of channels — it owns the settings they switch — so an unknown
				// name is answered there rather than duplicated into a second
				// list here that would drift from it.
				if len(args) != 2 {
					return Result{Error: "usage: /voice <channel> on|off — try /voice status to see them"}
				}
				switch state := strings.ToLower(args[1]); state {
				case "on", "off":
					return Result{Action: ActionVoiceChannel, Data: verb + " " + state}
				default:
					return Result{Error: "usage: /voice " + verb + " on|off"}
				}
			}
		},
	})

	// The auto/set/save verbs this command carried were local-Ollama num_ctx
	// tuning inherited from local-agent. Every provider sonar can actually run
	// against is hosted and owns its own window, so auto and set could only
	// ever error, and save persisted a value SetNumCtx no-ops for remote
	// providers — a knob that wrote config and changed nothing. Removed rather
	// than documented around.
	r.Register(&Command{
		Name:        "context",
		Aliases:     []string{"ctx"},
		Description: "Visualize how much of the context window this session is using",
		Usage:       "/context [status]",
		Handler: func(ctx *Context, args []string) Result {
			if len(args) == 0 {
				return Result{Action: ActionShowContextDoctor}
			}
			switch strings.ToLower(args[0]) {
			case "status", "show", "analyze":
				if len(args) != 1 {
					return Result{Error: "usage: /context [status]"}
				}
				return Result{Action: ActionContextWindowStatus}
			default:
				return Result{Error: "usage: /context [status]"}
			}
		},
	})

	r.Register(&Command{
		Name:        "stats",
		Description: "Show token usage statistics for this session",
		Usage:       "/stats",
		Handler: func(ctx *Context, args []string) Result {
			if err := noArguments(args, "/stats"); err != "" {
				return Result{Error: err}
			}
			if ctx.SessionTurnCount == 0 {
				return Result{Text: "No token usage recorded yet."}
			}
			var b strings.Builder
			b.WriteString("Session Token Stats\n")
			fmt.Fprintf(&b, "  Model:           %s\n", ctx.CurrentModel)
			fmt.Fprintf(&b, "  Turns:           %d\n", ctx.SessionTurnCount)
			fmt.Fprintf(&b, "  Output tokens:   %d\n", ctx.SessionEvalTotal)
			fmt.Fprintf(&b, "  Prompt processed:%8d\n", ctx.SessionPromptTotal)
			if ctx.LatestPromptTokens > 0 {
				fmt.Fprintf(&b, "  Current request:%8d\n", ctx.LatestPromptTokens)
			}
			if ctx.NumCtx > 0 {
				fmt.Fprintf(&b, "  Context window:  %d\n", ctx.NumCtx)
				if ctx.LatestPromptTokens > 0 {
					pct := min(100, max(0, ctx.LatestPromptTokens*100/ctx.NumCtx))
					fmt.Fprintf(&b, "  Context used:    %d%%\n", pct)
				}
			}
			avgOut := ctx.SessionEvalTotal / ctx.SessionTurnCount
			fmt.Fprintf(&b, "  Avg out/turn:    %d\n", avgOut)
			return Result{Text: b.String()}
		},
	})

	r.Register(&Command{
		Name:        "export",
		Description: "Export readable Markdown with a typed v2 transcript",
		Usage:       "/export [--force] <path>",
		Handler: func(_ *Context, args []string) Result {
			force := len(args) > 0 && args[0] == "--force"
			if force {
				args = args[1:]
			}
			path := expandHomePath(strings.TrimSpace(strings.Join(args, " ")))
			if path == "" {
				return Result{Error: "usage: /export [--force] <filepath>"}
			}
			return Result{
				Text:   fmt.Sprintf("Exporting conversation to: %s", path),
				Action: ActionExport,
				Data:   path,
				Force:  force,
			}
		},
	})

	r.Register(&Command{
		Name:        "import",
		Description: "Import a typed v2 transcript into a fresh session",
		Usage:       "/import [path]",
		Handler: func(_ *Context, args []string) Result {
			path := expandHomePath(strings.TrimSpace(strings.Join(args, " ")))
			if path == "" {
				return Result{Error: "usage: /import <filepath>"}
			}
			return Result{
				Text:   fmt.Sprintf("Importing conversation from: %s", path),
				Action: ActionImport,
				Data:   path,
			}
		},
	})

	r.Register(&Command{
		Name:        "checkpoint",
		Aliases:     []string{"cp"},
		Description: "Save a checkpoint of the current conversation",
		Usage:       "/checkpoint [label]",
		Handler: func(_ *Context, args []string) Result {
			return Result{Action: ActionCheckpoint, Data: strings.Join(args, " ")}
		},
	})

	r.Register(&Command{
		Name:        "checkpoints",
		Description: "List saved checkpoints (use /restore <id> to rewind)",
		Usage:       "/checkpoints",
		Handler: func(_ *Context, args []string) Result {
			if err := noArguments(args, "/checkpoints"); err != "" {
				return Result{Error: err}
			}
			return Result{Action: ActionListCheckpoints}
		},
	})

	r.Register(&Command{
		Name:        "restore",
		Description: "Restore the conversation to a saved checkpoint",
		Usage:       "/restore <id>",
		Handler: func(_ *Context, args []string) Result {
			if len(args) != 1 || args[0] == "" {
				return Result{Error: "usage: /restore <id> — see /checkpoints for ids"}
			}
			id, err := strconv.ParseInt(args[0], 10, 64)
			if err != nil || id <= 0 || strconv.FormatInt(id, 10) != args[0] {
				return Result{Error: "restore id must be a positive decimal integer — see /checkpoints for ids"}
			}
			return Result{Action: ActionRestoreCheckpoint, Data: args[0]}
		},
	})

	r.Register(&Command{
		Name:        "exit",
		Aliases:     []string{"quit", "q"},
		Description: "Quit sonar",
		Usage:       "/exit",
		Handler: func(_ *Context, args []string) Result {
			if err := noArguments(args, "/exit"); err != "" {
				return Result{Error: err}
			}
			return Result{Action: ActionQuit}
		},
	})
}

func formatPermissionsStatus(ctx *Context) string {
	if ctx == nil {
		ctx = &Context{}
	}
	var b strings.Builder
	b.WriteString("Approval posture\n")
	switch strings.TrimSpace(ctx.ApprovalPosture) {
	case "skip_approvals":
		b.WriteString("  Posture: approval prompts skipped (host/tool boundaries still apply)\n")
	case "accept_workspace_edits":
		b.WriteString("  Posture: accept workspace edits (write/edit/mkdir auto in workspace)\n")
	default:
		b.WriteString("  Posture: prompted for approval-gated tools\n")
	}
	b.WriteString("  Toggle:  /permissions accept-edits on|off\n")
	b.WriteString("  Panel:   /permissions panel  ·  Settings → Permissions\n")
	b.WriteString("\n")
	if len(ctx.SessionApprovals) == 0 {
		b.WriteString("Session grants: none (process-local; lost on restart)\n")
	} else {
		fmt.Fprintf(&b, "Session grants (%d, process-local; not persisted):\n", len(ctx.SessionApprovals))
		for _, grant := range ctx.SessionApprovals {
			fmt.Fprintf(&b, "  - %s\n", grant)
		}
		b.WriteString("Revoke with: /permissions revoke [tool] · clear all: /permissions clear\n")
	}
	b.WriteString("\n")
	if len(ctx.WorkspaceBashPrefixes) == 0 && len(ctx.WorkspaceMCPTools) == 0 &&
		len(ctx.WorkspaceMCPServers) == 0 && len(ctx.WorkspaceWritePaths) == 0 {
		b.WriteString("Workspace rules: none\n")
		b.WriteString("Add with: /permissions allow-bash <pattern> · allow-mcp <server__tool> · allow-server <server> · allow-path <path>\n")
	} else {
		b.WriteString("Workspace rules (durable for this workspace):\n")
		if len(ctx.WorkspaceBashPrefixes) == 0 {
			b.WriteString("  bash patterns: none\n")
		} else {
			b.WriteString("  bash patterns (prefix or trailing *):\n")
			for _, prefix := range ctx.WorkspaceBashPrefixes {
				fmt.Fprintf(&b, "    - %q\n", prefix)
			}
		}
		if len(ctx.WorkspaceMCPTools) == 0 {
			b.WriteString("  MCP tools: none\n")
		} else {
			b.WriteString("  MCP tools:\n")
			for _, tool := range ctx.WorkspaceMCPTools {
				fmt.Fprintf(&b, "    - %s\n", tool)
			}
		}
		if len(ctx.WorkspaceMCPServers) != 0 {
			b.WriteString("  MCP servers (every tool, approval only):\n")
			for _, server := range ctx.WorkspaceMCPServers {
				fmt.Fprintf(&b, "    - %s\n", server)
			}
		}
		if len(ctx.WorkspaceWritePaths) == 0 {
			b.WriteString("  write paths: none\n")
		} else {
			b.WriteString("  write paths (write/edit/mkdir):\n")
			for _, path := range ctx.WorkspaceWritePaths {
				fmt.Fprintf(&b, "    - %s\n", path)
			}
		}
		b.WriteString("Remove with: /permissions forget-bash · forget-mcp · forget-server · forget-path · clear-rules\n")
	}
	b.WriteString("\n")
	b.WriteString("Portable transfer: /permissions export [path] · /permissions import [--replace] <path>\n")
	return strings.TrimRight(b.String(), "\n")
}

func noArguments(args []string, usage string) string {
	if len(args) == 0 {
		return ""
	}
	return "usage: " + usage
}

func parseGoalRequest(args []string) (*GoalRequest, error) {
	if len(args) == 0 {
		return nil, nil
	}
	promptStart := 0
	request := &GoalRequest{}
	if duration, err := time.ParseDuration(args[0]); err == nil {
		if duration <= 0 {
			return nil, errors.New("goal duration must be positive")
		}
		request.TimeBudget = duration
		request.TimeExplicit = true
		promptStart = 1
	} else if looksLikeGoalDuration(args[0]) {
		return nil, fmt.Errorf("invalid goal duration %q; use Go duration syntax such as 30m or 1h30m", args[0])
	}
	request.Prompt = strings.TrimSpace(strings.Join(args[promptStart:], " "))
	if request.TimeExplicit && request.Prompt == "" {
		return nil, errors.New("usage: /goal <duration> <prompt>")
	}
	return request, nil
}

// looksLikeGoalDuration distinguishes a mistyped leading duration from a
// free-form objective. It recognizes Go's units plus common long-form units,
// while leaving numeric prompts such as "2026 roadmap" untouched.
func looksLikeGoalDuration(value string) bool {
	runes := []rune(strings.TrimSpace(value))
	if len(runes) == 0 {
		return false
	}
	index := 0
	if runes[index] == '+' || runes[index] == '-' {
		index++
	}
	if index >= len(runes) || runes[index] < '0' || runes[index] > '9' {
		return false
	}

	sawUnit := false
	for index < len(runes) {
		digits := 0
		dots := 0
		for index < len(runes) {
			r := runes[index]
			if r >= '0' && r <= '9' {
				digits++
				index++
				continue
			}
			if r == '.' && dots == 0 {
				dots++
				index++
				continue
			}
			break
		}
		if digits == 0 {
			return sawUnit
		}
		unitStart := index
		for index < len(runes) {
			r := runes[index]
			if (r >= 'a' && r <= 'z') || r == 'µ' || r == 'μ' {
				index++
				continue
			}
			break
		}
		if unitStart == index {
			return sawUnit
		}
		if !isGoalDurationUnit(string(runes[unitStart:index])) {
			return sawUnit
		}
		sawUnit = true
	}
	return sawUnit
}

func isGoalDurationUnit(unit string) bool {
	switch unit {
	case "ns", "us", "µs", "μs", "ms", "s", "m", "h",
		"sec", "secs", "second", "seconds",
		"min", "mins", "minute", "minutes",
		"hr", "hrs", "hour", "hours",
		"d", "day", "days":
		return true
	default:
		return false
	}
}

func expandHomePath(path string) string {
	if strings.HasPrefix(path, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			return home + path[1:]
		}
	}
	return path
}

func skillList(ctx *Context) Result {
	if len(ctx.Skills) == 0 {
		return Result{Text: "No skills found. Add Agent Skills under the configured agents directory at skills/<name>/SKILL.md"}
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Skills (%d):\n", len(ctx.Skills))
	for _, s := range ctx.Skills {
		status := "  "
		if s.Active {
			status = "* "
		}
		fmt.Fprintf(&b, "  %s%s — %s\n", status, s.Name, s.Description)
	}
	b.WriteString("\n* = active")
	return Result{Text: b.String()}
}
