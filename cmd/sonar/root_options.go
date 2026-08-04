package main

import (
	"errors"
	"flag"
	"io"
)

// rootOptions is the side-effect-free projection of the root CLI flags. A
// dedicated FlagSet keeps parsing reusable in tests and lets --version return
// before configuration or runtime initialization without scanning prompt
// values as if they were flags.
type rootOptions struct {
	qwenRouter     bool
	model          string
	agentProfile   string
	prompt         string
	tools          string
	mode           string
	auto           bool
	plan           bool
	skipApprovals  bool
	legacyYolo     bool
	version        bool
	jsonReceipt    bool
	runID          string
	turnID         string
	actor          string
	promptProvided bool
	toolsProvided  bool
	modeProvided   bool
	resume         resumeFlagValue
	arguments      []string
}

func parseRootOptions(program string, args []string, stderr, stdout io.Writer) (rootOptions, error) {
	var options rootOptions
	flags := flag.NewFlagSet(program, flag.ContinueOnError)
	flags.SetOutput(stderr)
	flags.Usage = func() {}
	flags.BoolVar(&options.qwenRouter, "qwen-router", false, "use optimized Qwen model router (experimental)")
	flags.StringVar(&options.model, "model", "", "override Ollama model")
	flags.StringVar(&options.agentProfile, "agent", "", "override agent profile")
	flags.StringVar(&options.prompt, "p", "", "shorthand for --prompt")
	flags.StringVar(&options.prompt, "prompt", "", "run one non-interactive prompt, print the response, and exit")
	flags.StringVar(&options.tools, "tools", "", "expose only these built-in tools in headless mode (comma-separated)")
	flags.StringVar(&options.mode, "mode", "normal", "headless authority: normal, plan, or auto")
	flags.BoolVar(&options.auto, "auto", false, "shortcut for --mode auto (requires --prompt)")
	flags.BoolVar(&options.plan, "plan", false, "shortcut for --mode plan (requires --prompt)")
	flags.BoolVar(&options.skipApprovals, "skip-approvals", false, "skip approval prompts; host, scope, and tool boundaries still apply")
	flags.BoolVar(&options.legacyYolo, "yolo", false, "deprecated alias for --skip-approvals")
	flags.BoolVar(&options.version, "version", false, "print the build version and exit")
	flags.BoolVar(&options.jsonReceipt, "json", false, "emit one machine-readable turn receipt on stdout (requires --prompt)")
	flags.StringVar(&options.runID, "run-id", "", "label the execution ledger with an external run identity (requires --prompt)")
	flags.StringVar(&options.turnID, "turn-id", "", "label this turn with an external turn identity (requires --prompt)")
	flags.StringVar(&options.actor, "actor", "", "record an external actor label in the turn receipt (requires --prompt)")
	flags.Var(&options.resume, "resume", "restore a saved interactive session by positive ID or latest")

	err := flags.Parse(args)
	if errors.Is(err, flag.ErrHelp) {
		writeRootUsage(stdout, program)
	}
	flags.Visit(func(visited *flag.Flag) {
		switch visited.Name {
		case "p", "prompt":
			options.promptProvided = true
		case "tools":
			options.toolsProvided = true
		case "mode":
			options.modeProvided = true
		}
	})
	options.arguments = append([]string(nil), flags.Args()...)
	return options, err
}
