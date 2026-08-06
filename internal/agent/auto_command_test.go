package agent

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestAutoScopedCommandAllowsRoutineWorkspaceDevelopment(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("AUTO shell catalog requires a POSIX shell")
	}
	workspace := t.TempDir()
	nested := filepath.Join(workspace, "internal", "queue")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatal(err)
	}
	// Classification must not depend on which development tools happen to be
	// preinstalled on the test host. Resolve every external command used below
	// from a host-owned directory outside the workspace, matching the production
	// provenance check without executing any fixture binary.
	hostBin := t.TempDir()
	for _, name := range []string{
		"bun", "cargo", "date", "go", "gofmt", "grep", "head", "npm", "pnpm", "rg", "sed", "swift", "yarn",
	} {
		if err := os.WriteFile(filepath.Join(hostBin, name), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
			t.Fatalf("install host executable %s: %v", name, err)
		}
	}
	t.Setenv("PATH", hostBin+string(os.PathListSeparator)+os.Getenv("PATH"))
	ag := New(nil, nil, 4096)
	ag.SetWorkDir(workspace)

	for _, command := range []string{
		"go test ./...",
		"cd " + workspace + " && go build ./... 2>&1",
		"go build ./... 2>/dev/null",
		"go vet ./... 1>/dev/null",
		"go test ./... >/dev/null 2>&1",
		"go test ./... 2>/dev/null | head -30",
		"go test ./... > result.txt",
		"swift test > build.log",
		"swift test 2>&1 > build.log",
		"echo done >> notes.md",
		"cd internal/queue && go test ./...",
		"gofmt -w internal/queue/policy.go && go test ./internal/queue",
		"sed -n '1,120p' internal/queue/policy.go",
		"sed -n '$p' internal/queue/policy.go",
		"sed -n '1p' -- internal/queue/policy.go",
		"go test -run 'Test(Foo|Bar)' ./...",
		"go test -run=/subtest ./...",
		"npm test",
		"bun test",
		// `run <script>` carries the same trust as the direct forms above: the
		// workspace manifest already defines what `npm test` executes, so the
		// script name adds no authority. Only the argv shape is policed.
		"npm run build",
		"npm run site:build",
		"npm run build-storybook",
		"npm run test_e2e",
		"bun run lint",
		"pnpm run test",
		"yarn run lint",
		"CI=1 cargo check",
		"date",
	} {
		t.Run(command, func(t *testing.T) {
			if !ag.autoScopedCommandAllowed(command) {
				t.Fatalf("routine command was not admitted in AUTO: %q", command)
			}
		})
	}
}

func TestAutoScopedCommandGatesDynamicDestructiveAndExternalEffects(t *testing.T) {
	workspace := t.TempDir()
	outside := t.TempDir()
	ag := New(nil, nil, 4096)
	ag.SetWorkDir(workspace)

	commands := []string{
		"rm -rf .",
		"git push origin main",
		// `git status --short` and `git diff --stat` moved to the read-only git
		// catalogue: they cannot mutate under any accepted argument, and gating
		// them cost an approval on the most common thing an agent does. Their
		// admission is covered by TestGitReadSubcommandsAreAutoScoped, and every
		// mutating neighbour below still belongs here.
		"git add .",
		"git commit -m done",
		"git tag v0.1.0",
		"git switch feature",
		"git merge feature",
		"git rebase main",
		"git cherry-pick deadbeef",
		"git branch -D old-work",
		"git branch -Dold-work",
		"git tag --delete v0.1.0",
		"git tag -dv0.1.0",
		"git switch --discard-changes main",
		"git switch -fmain",
		"git rebase --exec 'curl https://example.test' main",
		"git rebase '-xcurl https://example.test' main",
		"git diff --ext-diff",
		"git diff --textconv",
		"git grep --open-files-in-pager=less needle",
		"git commit -F/etc/passwd",
		"git tag -F/etc/passwd v0.1.0",
		"make -f" + filepath.Join(outside, "Makefile") + " test",
		"make -ksf" + filepath.Join(outside, "Makefile") + " test",
		"sort input -o" + filepath.Join(outside, "sorted.txt"),
		"sort -bo" + filepath.Join(outside, "sorted-cluster.txt") + " input",
		"sort --files0-from=paths0",
		"sort --files0-fro=paths0",
		"sort --compress-prog=false /dev/null",
		"tree -o" + filepath.Join(outside, "tree.txt") + " .",
		"tree -do" + filepath.Join(outside, "tree-cluster.txt") + " .",
		"find -f" + filepath.Join(outside, "secret.txt"),
		"grep -Hf" + filepath.Join(outside, "patterns.txt") + " /dev/null",
		"grep /etc/hosts -e localhost",
		"rg /etc/hosts -e localhost",
		"grep -f patterns /etc/passwd",
		"rg -f patterns /etc/passwd",
		"grep '' /etc/passwd",
		`rg "" /etc/passwd`,
		"grep -e '' /etc/passwd",
		`rg -e "" /etc/passwd`,
		"rg --files /etc",
		"rg --hidden --no-ignore TOKEN .",
		"rg TOKEN",
		"grep -r TOKEN .",
		"find .",
		"file -bf" + filepath.Join(outside, "files.txt") + " /dev/null",
		"file -f file-list.txt",
		"file --files-fr=file-list.txt",
		"file -m" + filepath.Join(outside, "magic") + " /dev/null",
		"file -m local:/etc/magic target",
		"file -M local:/etc/magic target",
		"file -bz /dev/null",
		"file -bZ /dev/null",
		"file --uncompress-n /dev/null",
		"file -C /dev/null",
		"file -S /dev/null",
		"file --no-sand /dev/null",
		"rg -zP needle /dev/null",
		"rg -Pz needle /dev/null",
		"touch -r" + filepath.Join(outside, "reference") + " target",
		// `run <script>` admits exactly one conservatively named script and
		// nothing else. What the script DOES is workspace-defined and already
		// trusted through the direct `npm test` form; what stays refused is
		// every argv shape that reaches past the manifest — `--` passthrough
		// and extra arguments hand flags to the interpreter or the script, and
		// a name outside the charset can be an option, a path, or quoted spaces.
		"npm run format -- --plugin=file:///private/tmp/plugin.mjs",
		"npm run lint -- --inspect-config",
		"npm test --node-options=--inspect-wait",
		"npm test --node-op=--inspect-wait",
		`npm run "build "`,
		"npm run -s",
		"npm run --silent build",
		"bun run ./script.sh",
		"npm run ../outside",
		"npm test -- --watch",
		"pnpm run test --watch=true",
		"yarn test --watchAll",
		"yarn run test --watch-all",
		"bun run build --watch",
		"bun build",
		"bun build --watch ./src/index.ts",
		"bun test --hot",
		"task release",
		"task site",
		"task docs:serve",
		"make test-watch",
		"task VERIFY",
		`make "test "`,
		"make clean",
		"make test --eval='x:; touch /private/tmp/pwn'",
		"make test -E 'x:; touch /private/tmp/pwn'",
		"make test CMD='touch /private/tmp/pwn'",
		"touch " + filepath.Join(outside, "pwn=foo"),
		"touch /dev/null",
		"touch -r /dev/null target",
		"make test",
		"task site:verify",
		"just test",
		"go generate ./...",
		"mkdir /dev/null",
		"mkdir " + filepath.Join(outside, "dir=x"),
		"sort -o " + filepath.Join(outside, "out=x") + " input",
		"sort --output=../outside.txt input",
		"touch name=../outside.txt",
		"rg --pre=sh pattern script.sh",
		"rg --search-zip pattern archive.zip",
		"rg --hostname-bin=env --hyperlink-format=default needle .",
		"cargo test --config 'target.x86_64-unknown-linux-gnu.runner=\"/private/tmp/evil\"'",
		"cargo doc --open",
		"go build -ldflags='-linkmode=external -extld=/private/tmp/evil' ./...",
		"golangci-lint cache clean",
		"golangci-lint custom",
		"date -s 2030-01-01",
		"curl https://example.test",
		// Workspace redirects are admitted, so every refusal below is about the
		// target, not the operator: /tmp is world-writable (a pre-planted symlink
		// there would let > truncate an arbitrary file), and traversal, home
		// expansion, and workspace-symlink escapes all resolve outside.
		"go test ./... > /tmp/results.txt",
		"echo hi >> /tmp/results.txt",
		"go test ./... > ../escape.log",
		"echo hi > ~/notes.txt",
		"swift test >",
		// The /dev/null sink is admitted only as the byte-exact tokens; a stray
		// suffix or a different sink path is a real file redirect again.
		"go test 2>/dev/nullx",
		"go test 2>/tmp/leak",
		"go test 2>>/dev/null",
		"go env -w GOPROXY=https://example.test",
		"go test -exec=curl ./...",
		"go test -fuzz=FuzzParser ./...",
		"go test -test.fuzz=FuzzParser ./...",
		"go test --test.fuzzworker ./...",
		"go build -toolexec=touch ./...",
		"go test $(cat packages.txt)",
		"sh -c 'go test ./...'",
		"./go test ./...",
		filepath.Join(outside, "go") + " test ./...",
		"cd " + outside + " && go test ./...",
		"go build -o " + filepath.Join(outside, "app") + " ./cmd/app",
		"find . -delete",
		"find -L .",
		"find -XL .",
		"find -EL .",
		"find . -follow",
		"find -files0-from starts.txt",
		"rg -L needle .",
		"rg -nL needle .",
		"rg -NL needle .",
		"rg -UL needle .",
		"tree -l .",
		"tree -dl .",
		"du -L .",
		"du --files0-from=paths.txt",
		"grep -R needle .",
		"grep -nR needle .",
		"grep -rS needle .",
		"grep -Sr needle .",
		"ls -RL .",
		"ls -TRL .",
		"ls -IL .",
		"ls -wL .",
		"wc --files0-from=paths.txt",
		"diff -r . snapshot",
		"diff dir1 dir2",
		"diff -ru . snapshot",
		"diff -l before after",
		"diff -ul before after",
		"eslint -o" + filepath.Join(outside, "report.txt") + " .",
		"eslint --inspect-config eslint.config.js",
		"eslint --init",
		"eslint --mcp",
		"prettier --plugin file:///tmp/plugin.mjs .",
		"prettier --plugin=data:text/javascript,export%20default%20{} .",
		"tail -f app.log",
		"tail -F app.log",
		"tail -nf app.log",
		"tail --follow=name app.log",
		"tail --retry app.log",
		"tsc --watch",
		"tsc -w",
		"tsc -bw",
		"tsc --build --clean",
		"tsc -b --clean",
		"tsc --typeRoots foo,/etc no-such-file.ts",
		"tsc --typeRoots=foo,/etc no-such-file.ts",
		"tsc --rootDirs foo,/etc no-such-file.ts",
		"tsc --rootDirs=foo,/etc no-such-file.ts",
		"tsc @/etc/passwd",
		"swift build --disable-sandbox",
		"swift test -Xcc @/tmp/args.rsp",
		"swift build -Xswiftc -load-plugin-executable",
		"swift build -Xlinker /tmp/linker.args",
		"swift build -Xcxx @/tmp/cxx.rsp",
		"sed -i '' s/old/new/ file.go",
		"sed -i.bak s/old/new/ file.go",
		"sed -n '1,20w leaked.txt' file.go",
		"sed -n 1p file.go -i",
		"sed -n 1p file.go -e 'w leaked.txt'",
		"go get example.test/module",
		"command rm -rf .",
		"LD_PRELOAD=/tmp/inject.dylib go test ./...",
		"cat *.go",
		"cd .\\\n. && touch unexpected.txt",
		"rg --fo\\\nllow needle .",
		"find . -fo\\\nllow -del\\\nete",
		"cat safe\runsafe",
		"cat pivot/../etc/passwd",
		"cd nested ; cat outside-link/passwd",
		"cd nested | cat outside-link/passwd",
		"cd nested || cat outside-link/passwd",
	}
	if runtime.GOOS != "windows" {
		if err := os.MkdirAll(filepath.Join(workspace, "nested"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(outside, filepath.Join(workspace, "outside-link")); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(outside, filepath.Join(workspace, "nested", "outside-link")); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(string(filepath.Separator), filepath.Join(workspace, "pivot")); err != nil {
			t.Fatal(err)
		}
		commands = append(commands, "cd nested && cat outside-link/passwd")
		if err := os.Symlink(outside, filepath.Join(workspace, "outside2>&1")); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(outside, filepath.Join(workspace, "name=value")); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(outside, filepath.Join(workspace, `outside\link`)); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(outside, filepath.Join(workspace, "outside-nbsp\u00a0")); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(outside, filepath.Join(workspace, "outside-space ")); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(outside, filepath.Join(workspace, "outside-newline\n")); err != nil {
			t.Fatal(err)
		}
		commands = append(commands, "cat outside-link/secret.txt")
		commands = append(commands, "echo hi > outside-link/pwn.txt")
		commands = append(commands, "make -foutside-link/Makefile test")
		commands = append(commands, "cat 'outside2>&1/secret.txt'")
		commands = append(commands, "touch 'name=value/new.txt'")
		commands = append(commands, `cat "outside\link/secret.txt"`)
		commands = append(commands, "cat outside-nbsp\u00a0")
		commands = append(commands, `cat "outside-space "`)
		commands = append(commands, "cat 'outside-newline\n'")
	}
	for _, command := range commands {
		t.Run(command, func(t *testing.T) {
			if ag.autoScopedCommandAllowed(command) {
				t.Fatalf("risky command gained AUTO authority: %q", command)
			}
		})
	}
}

func TestAutoScopedCommandRejectsDefaultSecretPaths(t *testing.T) {
	workspace := t.TempDir()
	if err := os.WriteFile(filepath.Join(workspace, ".env"), []byte("TOKEN=secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	ag := New(nil, nil, 4096)
	ag.SetWorkDir(workspace)
	if ag.autoScopedCommandAllowed("cat .env") {
		t.Fatal("AUTO approved a shell read of a default-secret path")
	}
}

func TestAutoScopedCommandGatesRawDirectoryEnumerators(t *testing.T) {
	workspace := t.TempDir()
	hostBin := t.TempDir()
	for _, name := range []string{"du", "ls", "tree"} {
		if err := os.WriteFile(filepath.Join(hostBin, name), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", hostBin+string(os.PathListSeparator)+os.Getenv("PATH"))
	if err := os.MkdirAll(filepath.Join(workspace, "private"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspace, ".env"), []byte("TOKEN=secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspace, "private", "credentials.txt"), []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	ag := New(nil, nil, 4096)
	ag.SetWorkspacePolicy(workspace, "private/**\n")

	for _, command := range []string{
		"tree .", "tree -a .",
		"du .", "du -a .",
		"ls .", "ls -A .", "ls -Ra .", "ls -alR .", "ls --recursive .",
	} {
		if ag.autoScopedCommandAllowed(command) {
			t.Errorf("raw directory enumerator gained AUTO authority: %q", command)
		}
	}
}

func TestSplitStaticShellCommandsRejectsAmbiguousSyntax(t *testing.T) {
	for _, command := range []string{
		"go test &", "go test &&", "'unterminated", "go test < input",
		// Output redirects need a bare operator and a target: a trailing >, a
		// separator before the target, a doubled pending form, and the glued
		// descriptor spelling all stay outside the static subset.
		"go test >", "echo x > | cat", "echo x > > f", "go test 2> log",
	} {
		if _, _, _, ok := splitStaticShellCommands(command); ok {
			t.Fatalf("ambiguous shell syntax accepted: %q", command)
		}
	}
}

func TestAutoCommandPolicyHelpersRejectDelegationAndPersistentModes(t *testing.T) {
	for _, args := range [][]string{
		{"test", "-fuzz=FuzzParser", "./..."},
		{"test", "-test.fuzz=FuzzParser", "./..."},
		{"test", "--test.fuzzworker", "./..."},
	} {
		if autoScopedGoCommandAllowed(args) {
			t.Fatalf("Go fuzz mode admitted: %#v", args)
		}
	}
	for _, args := range [][]string{
		{"build", "--disable-sandbox"},
		{"test", "-Xcc", "@/tmp/args.rsp"},
		{"build", "-Xlinker=/tmp/linker.args"},
	} {
		if autoScopedSwiftCommandAllowed(args) {
			t.Fatalf("Swift delegation admitted: %#v", args)
		}
	}
	for _, args := range [][]string{
		{"@/etc/passwd"},
		{"--watch"},
		{"-bw"},
		{"--build", "--clean"},
		{"--typeRoots=foo,/etc"},
	} {
		if autoScopedTSCCommandAllowed(args) {
			t.Fatalf("TypeScript delegated or persistent mode admitted: %#v", args)
		}
	}
	if autoScopedPackageCommandAllowed([]string{"run", "format", "--", "--plugin=file:///tmp/plugin.mjs"}, "test") {
		t.Fatal("package-script trailing arguments were admitted")
	}
	if !containsLongOptionPrefix([]string{"--files0-fro=paths"}, "--files0-from") ||
		!containsLongOptionPrefix([]string{"--uncompress-n"}, "--uncompress-noreport") {
		t.Fatal("dangerous GNU long-option abbreviation was not recognized")
	}
}

func TestSplitStaticShellCommandsPreservesQuotedEmptyArguments(t *testing.T) {
	commands, separators, _, ok := splitStaticShellCommands(`grep '' README.md && rg -e "" .`)
	if !ok {
		t.Fatal("static command with quoted-empty arguments was rejected")
	}
	if len(separators) != 1 || separators[0] != "&&" {
		t.Fatalf("separators = %#v, want [&&]", separators)
	}
	want := [][]string{{"grep", "", "README.md"}, {"rg", "-e", "", "."}}
	if len(commands) != len(want) {
		t.Fatalf("commands = %#v, want %#v", commands, want)
	}
	for index := range want {
		if len(commands[index]) != len(want[index]) {
			t.Fatalf("commands = %#v, want %#v", commands, want)
		}
		for argument := range want[index] {
			if commands[index][argument] != want[index][argument] {
				t.Fatalf("commands = %#v, want %#v", commands, want)
			}
		}
	}
}

func TestAutoScopedCommandRejectsWorkspaceExecutableShadowing(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("AUTO shell catalog requires a POSIX sh")
	}
	workspace := t.TempDir()
	shadow := filepath.Join(workspace, "go")
	if err := os.WriteFile(shadow, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", workspace+string(os.PathListSeparator)+os.Getenv("PATH"))
	ag := New(nil, nil, 4096)
	ag.SetWorkDir(workspace)
	if ag.autoScopedCommandAllowed("go version") {
		t.Fatal("workspace executable shadow gained AUTO authority through PATH")
	}
}

func TestAutoCommandAssessmentRejectsGenericWorkspaceExecutables(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("AUTO shell catalog requires a POSIX shell")
	}
	workspace := t.TempDir()
	binDir := filepath.Join(workspace, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	minerva := filepath.Join(binDir, "minerva")
	if err := os.WriteFile(minerva, []byte("\x7fELF-sonar-test-fixture"), 0o755); err != nil {
		t.Fatal(err)
	}
	ag := New(nil, nil, 4096)
	ag.SetWorkDir(workspace)

	// A workspace executable controls its real effect; status/help-style argv is
	// not a trustworthy contract. Exact local ecosystem operations must arrive
	// through a host-trusted MCP/MCPHub route instead.
	for _, command := range []string{
		"./bin/minerva --version",
		"./bin/minerva --help 2>&1",
		"./bin/minerva stack check",
		"./bin/minerva skill list",
		"./bin/minerva suggest architecture",
		"CI=1 ./bin/minerva status --json",
		minerva + " doctor",
		"./bin/minerva",
		"./bin/minerva init",
		"./bin/minerva skill activate test-skill",
		"./bin/minerva deploy production",
		"./bin/minerva status --url=https://example.test",
		"./bin/minerva status --host example.test",
		"./bin/minerva status --output report.json",
		"./bin/minerva status /etc/passwd",
		"./bin/minerva status --command='rm -rf /'",
	} {
		t.Run(command, func(t *testing.T) {
			if assessment := ag.assessAutoScopedCommand(command); assessment.admitted() {
				t.Fatalf("generic workspace executable invocation gained AUTO authority: %q (%#v)", command, assessment)
			}
		})
	}
}

func TestAutoCommandAssessmentAllowsExactMinervaWorkspaceQueries(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("AUTO shell catalog requires POSIX executable paths")
	}
	workspace := buildAutoCommandMinervaFixture(t, minervaModulePath, "cmd/minerva")
	ag := New(nil, nil, 4096)
	ag.SetWorkDir(workspace)
	minerva := filepath.Join(workspace, "bin", "minerva")
	for _, command := range []string{
		"./bin/minerva --version",
		"./bin/minerva --help 2>&1",
		"bin/minerva help stack check",
		"./bin/minerva skill list",
		"./bin/minerva skill list --json",
		"./bin/minerva profile list",
		"./bin/minerva stack check",
		"./bin/minerva stack check --json",
		"./bin/minerva analytics",
		"./bin/minerva analytics --json",
		"./bin/minerva suggest",
		"./bin/minerva suggest --json",
		"./bin/minerva template list --json",
		"./bin/minerva template show code-reviewer",
		"./bin/minerva evidence docs",
		"CI=1 ./bin/minerva stack check",
		"cd " + workspace + " && ./bin/minerva skill list",
		"cd " + workspace + " && ./bin/minerva skill list 2>&1 | grep test-skill",
		"./bin/minerva skill list | grep test-skill",
		"./bin/minerva template show code-reviewer | head -20",
		"./bin/minerva template show code-reviewer | head -n 20 | grep Prompt",
		minerva + " profile list --json",
	} {
		t.Run(command, func(t *testing.T) {
			assessment := ag.assessAutoScopedCommand(command)
			if !assessment.admitted() || assessment.effect != autoCommandEffectWorkspaceExecution ||
				!assessment.workspaceExecutable {
				t.Fatalf("exact Minerva query assessment = %#v, want admitted workspace execution", assessment)
			}
		})
	}
}

func TestAutoCommandAssessmentRejectsMinervaMutationAndAmbiguity(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("AUTO shell catalog requires POSIX executable paths")
	}
	workspace := buildAutoCommandMinervaFixture(t, minervaModulePath, "cmd/minerva")
	ag := New(nil, nil, 4096)
	ag.SetWorkDir(workspace)
	for _, command := range []string{
		"./bin/minerva",
		"./bin/minerva init",
		"./bin/minerva skill show agent-browser",
		"./bin/minerva skill compare a b",
		"./bin/minerva skill create a content",
		"./bin/minerva skill activate agent-browser",
		"./bin/minerva skill deactivate agent-browser",
		"./bin/minerva skill delete agent-browser",
		"./bin/minerva profile show default",
		"./bin/minerva profile create default",
		"./bin/minerva stack deep",
		"./bin/minerva stack check /etc/passwd",
		"./bin/minerva stack check --output report.json",
		"./bin/minerva suggest --apply",
		"./bin/minerva template show NOT_VALID",
		"./bin/minerva template apply review",
		"./bin/minerva evidence search minerva",
		"./bin/minerva evidence save artifact",
		"./bin/minerva mcp serve",
		"./bin/minerva help init",
		"../bin/minerva --version",
		"./bin/minerva skill list | grep -f patterns.txt",
		"./bin/minerva skill list | grep ../secret",
		"./bin/minerva skill list | head -n 201",
		"./bin/minerva skill list | head -n +1",
		"./bin/minerva skill list | head --lines=+1",
		"./bin/minerva skill list | head README.md",
		"./bin/minerva skill list | python3 -c 'print(1)'",
		"./bin/minerva skill list ; grep test-skill",
	} {
		t.Run(command, func(t *testing.T) {
			if assessment := ag.assessAutoScopedCommand(command); assessment.admitted() {
				t.Fatalf("mutation or ambiguous Minerva invocation gained AUTO authority: %#v", assessment)
			}
		})
	}
}

func TestAutoCommandAssessmentAllowsExactMinervaBuildThenQuery(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("AUTO shell catalog requires POSIX executable paths")
	}
	workspace := prepareAutoCommandMinervaFixture(t, minervaModulePath, "cmd/minerva")
	ag := New(nil, nil, 4096)
	ag.SetWorkDir(workspace)
	for _, command := range []string{
		"go build -o bin/minerva ./cmd/minerva && ./bin/minerva --version",
		"go build ./cmd/minerva -o=./bin/minerva && ./bin/minerva skill list",
		"go vet ./... && go build -o ./bin/minerva ./cmd/minerva && ./bin/minerva --version",
	} {
		assessment := ag.assessAutoScopedCommand(command)
		if !assessment.admitted() || assessment.effect != autoCommandEffectWorkspaceExecution ||
			!assessment.workspaceExecutable {
			t.Fatalf("exact Minerva build/query assessment for %q = %#v", command, assessment)
		}
	}
}

func TestAutoCommandAssessmentRejectsMinervaReplacementInSameShell(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("AUTO shell catalog requires POSIX executable paths")
	}
	workspace := buildAutoCommandMinervaFixture(t, minervaModulePath, "cmd/minerva")
	otherDir := filepath.Join(workspace, "cmd", "anything")
	if err := os.MkdirAll(otherDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(otherDir, "main.go"), []byte("package main\nfunc main() {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	ag := New(nil, nil, 4096)
	ag.SetWorkDir(workspace)
	for _, command := range []string{
		"go build -o bin/minerva ./cmd/anything && ./bin/minerva --version",
		"touch bin/minerva && ./bin/minerva --version",
		"go build -o bin/minerva ./cmd/minerva && touch bin/minerva && ./bin/minerva --version",
		"go build -o bin/minerva ./cmd/anything || go build -o bin/minerva ./cmd/minerva && ./bin/minerva --version",
	} {
		if assessment := ag.assessAutoScopedCommand(command); assessment.admitted() {
			t.Fatalf("same-shell Minerva replacement gained AUTO authority for %q: %#v", command, assessment)
		}
	}

	withoutBinary := prepareAutoCommandMinervaFixture(t, minervaModulePath, "cmd/minerva")
	anythingDir := filepath.Join(withoutBinary, "cmd", "anything")
	if err := os.MkdirAll(anythingDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(anythingDir, "main.go"), []byte("package main\nfunc main() {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	ag.SetWorkDir(withoutBinary)
	command := "go build -o bin/minerva ./cmd/anything || go build -o bin/minerva ./cmd/minerva && ./bin/minerva --version"
	if assessment := ag.assessAutoScopedCommand(command); assessment.admitted() {
		t.Fatalf("branched planned build without a pre-existing binary gained AUTO authority: %#v", assessment)
	}
}

func TestAutoCommandAssessmentPinsMinervaBuildIdentityAndLocation(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("AUTO shell catalog requires POSIX executable paths")
	}
	workspace := buildAutoCommandMinervaFixture(t, "example.com/lookalike", "cmd/minerva")
	ag := New(nil, nil, 4096)
	ag.SetWorkDir(workspace)
	if assessment := ag.assessAutoScopedCommand("./bin/minerva --version"); assessment.admitted() {
		t.Fatalf("lookalike Go build identity gained AUTO authority: %#v", assessment)
	}

	wrongCommand := buildAutoCommandMinervaFixture(t, minervaModulePath, "other/minerva")
	ag.SetWorkDir(wrongCommand)
	if assessment := ag.assessAutoScopedCommand("./bin/minerva --version"); assessment.admitted() {
		t.Fatalf("wrong Minerva main-package identity gained AUTO authority: %#v", assessment)
	}

	trusted := buildAutoCommandMinervaFixture(t, minervaModulePath, "cmd/minerva")
	ag.SetWorkDir(trusted)
	otherDir := filepath.Join(trusted, "tools")
	if err := os.MkdirAll(otherDir, 0o755); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(trusted, "bin", "minerva"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(otherDir, "minerva"), data, 0o755); err != nil {
		t.Fatal(err)
	}
	if assessment := ag.assessAutoScopedCommand("./tools/minerva --version"); assessment.admitted() {
		t.Fatalf("trusted Minerva identity outside bin/minerva gained AUTO authority: %#v", assessment)
	}
	minerva := filepath.Join(trusted, "bin", "minerva")
	if err := os.Chmod(minerva, 0o777); err != nil {
		t.Fatal(err)
	}
	if assessment := ag.assessAutoScopedCommand("./bin/minerva --version"); assessment.admitted() {
		t.Fatalf("group/world-writable Minerva gained AUTO authority: %#v", assessment)
	}

	symlinked := buildAutoCommandMinervaFixture(t, minervaModulePath, "cmd/minerva")
	realMinerva := filepath.Join(symlinked, "bin", "real-minerva")
	if err := os.Rename(filepath.Join(symlinked, "bin", "minerva"), realMinerva); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(realMinerva, filepath.Join(symlinked, "bin", "minerva")); err != nil {
		t.Fatal(err)
	}
	ag.SetWorkDir(symlinked)
	if assessment := ag.assessAutoScopedCommand("./bin/minerva --version"); assessment.admitted() {
		t.Fatalf("symlinked Minerva gained AUTO authority: %#v", assessment)
	}

	setID := buildAutoCommandMinervaFixture(t, minervaModulePath, "cmd/minerva")
	setIDMinerva := filepath.Join(setID, "bin", "minerva")
	if err := os.Chmod(setIDMinerva, 0o755|os.ModeSetuid); err != nil {
		t.Fatal(err)
	}
	setIDInfo, err := os.Lstat(setIDMinerva)
	if err != nil {
		t.Fatal(err)
	}
	if setIDInfo.Mode()&os.ModeSetuid == 0 {
		t.Log("filesystem did not retain the set-ID bit; provenance check remains covered on supporting filesystems")
	} else {
		ag.SetWorkDir(setID)
		if assessment := ag.assessAutoScopedCommand("./bin/minerva --version"); assessment.admitted() {
			t.Fatalf("set-ID Minerva gained AUTO authority: %#v", assessment)
		}
	}

	oversizedModule := prepareAutoCommandMinervaFixture(t, minervaModulePath, "cmd/minerva")
	moduleBody := "module " + minervaModulePath + "\n// " + strings.Repeat("x", 1<<20)
	if err := os.WriteFile(filepath.Join(oversizedModule, "go.mod"), []byte(moduleBody), 0o600); err != nil {
		t.Fatal(err)
	}
	ag.SetWorkDir(oversizedModule)
	if assessment := ag.assessAutoScopedCommand("go build -o bin/minerva ./cmd/minerva && ./bin/minerva --version"); assessment.admitted() {
		t.Fatalf("oversized Minerva module declaration gained AUTO authority: %#v", assessment)
	}
}

func prepareAutoCommandMinervaFixture(t *testing.T, modulePath, packageDir string) string {
	t.Helper()
	workspace := t.TempDir()
	commandDir := filepath.Join(workspace, filepath.FromSlash(packageDir))
	if err := os.MkdirAll(commandDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspace, "go.mod"), []byte("module "+modulePath+"\n\ngo 1.25\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(commandDir, "main.go"), []byte("package main\nfunc main() {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	binDir := filepath.Join(workspace, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	return workspace
}

func buildAutoCommandMinervaFixture(t *testing.T, modulePath, packageDir string) string {
	t.Helper()
	workspace := prepareAutoCommandMinervaFixture(t, modulePath, packageDir)
	command := exec.Command("go", "build", "-o", filepath.Join(workspace, "bin", "minerva"), "./"+filepath.ToSlash(packageDir))
	command.Dir = workspace
	command.Env = append(os.Environ(), "GOWORK=off", "CGO_ENABLED=0")
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("build Minerva identity fixture %s: %v\n%s", packageDir, err, output)
	}
	return workspace
}

func TestAutoCommandAssessmentRejectsUntrustedWorkspaceExecutableProvenance(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("AUTO shell catalog requires POSIX executable modes and symlinks")
	}
	workspace := t.TempDir()
	outside := t.TempDir()
	binDir := filepath.Join(workspace, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeExecutable := func(path string, mode os.FileMode) {
		t.Helper()
		if err := os.WriteFile(path, []byte("#!/bin/sh\nexit 0\n"), mode); err != nil {
			t.Fatal(err)
		}
	}
	writeExecutable(filepath.Join(binDir, "plain"), 0o644)
	writeExecutable(filepath.Join(binDir, "scripted"), 0o755)
	writeExecutable(filepath.Join(binDir, "git"), 0o755)
	writeExecutable(filepath.Join(binDir, "task"), 0o755)
	outsideExecutable := filepath.Join(outside, "minerva")
	writeExecutable(outsideExecutable, 0o755)
	if err := os.Symlink(outsideExecutable, filepath.Join(binDir, "outside-link")); err != nil {
		t.Fatal(err)
	}
	realDir := filepath.Join(workspace, "real-bin")
	if err := os.MkdirAll(realDir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeExecutable(filepath.Join(realDir, "minerva"), 0o755)
	if err := os.Symlink(realDir, filepath.Join(workspace, "linked-bin")); err != nil {
		t.Fatal(err)
	}
	ag := New(nil, nil, 4096)
	ag.SetWorkDir(workspace)
	for _, command := range []string{
		"./bin/plain status",
		"./bin/scripted status",
		"./bin/git status",
		"./bin/task check",
		"./bin/outside-link status",
		"./linked-bin/minerva status",
		outsideExecutable + " status",
	} {
		if assessment := ag.assessAutoScopedCommand(command); assessment.admitted() {
			t.Errorf("untrusted executable provenance gained AUTO authority: %q (%#v)", command, assessment)
		}
	}
}

func TestAutoCommandAssessmentDoesNotWidenShellWithReadGrants(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("AUTO shell catalog requires a POSIX shell")
	}
	workspace := t.TempDir()
	external := t.TempDir()
	public := filepath.Join(external, "public.txt")
	secret := filepath.Join(external, ".env")
	if err := os.WriteFile(public, []byte("public\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(secret, []byte("TOKEN=secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	hostBin := t.TempDir()
	for _, name := range []string{"cat", "head", "sed", "touch"} {
		if err := os.WriteFile(filepath.Join(hostBin, name), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", hostBin+string(os.PathListSeparator)+os.Getenv("PATH"))

	ag := New(nil, nil, 4096)
	ag.SetWorkDir(workspace)
	addAutoCommandReadGrant(t, ag, external)

	for _, command := range []string{
		"cat " + public,
		"head -n 1 " + public,
		"sed -n '1p' " + public,
		"cat " + public + " | head -n 1",
	} {
		assessment := ag.assessAutoScopedCommand(command)
		if assessment.admitted() || assessment.usesReadGrant {
			t.Errorf("typed read grant widened raw shell authority for %q: %#v", command, assessment)
		}
	}
	for _, command := range []string{
		"cat " + secret,
		"touch " + filepath.Join(external, "new.txt"),
		"go test " + external,
	} {
		if assessment := ag.assessAutoScopedCommand(command); assessment.admitted() {
			t.Errorf("grant escaped read-only or secret boundary: %q (%#v)", command, assessment)
		}
	}

	ungranted := filepath.Join(t.TempDir(), "private.txt")
	if err := os.WriteFile(ungranted, []byte("private"), 0o600); err != nil {
		t.Fatal(err)
	}
	if assessment := ag.assessAutoScopedCommand("cat " + ungranted); assessment.admitted() {
		t.Fatalf("ungranted external read gained AUTO authority: %#v", assessment)
	}

	// An exact grant remains available to host-owned read tools, but raw shell
	// never inherits it, whether or not the target resembles a secret.
	exactSecretAgent := New(nil, nil, 4096)
	exactSecretAgent.SetWorkDir(t.TempDir())
	addAutoCommandReadGrant(t, exactSecretAgent, secret)
	if assessment := exactSecretAgent.assessAutoScopedCommand("cat " + secret); assessment.admitted() {
		t.Fatalf("exact secret grant leaked into raw shell authority: %#v", assessment)
	}
}

func TestAutoCommandAssessmentRejectsStaleExactReadGrant(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("AUTO shell catalog requires a POSIX shell")
	}
	workspace := t.TempDir()
	external := t.TempDir()
	target := filepath.Join(external, "public.txt")
	if err := os.WriteFile(target, []byte("first"), 0o644); err != nil {
		t.Fatal(err)
	}
	ag := New(nil, nil, 4096)
	ag.SetWorkDir(workspace)
	addAutoCommandReadGrant(t, ag, target)
	if assessment := ag.assessAutoScopedCommand("cat " + target); assessment.admitted() || assessment.usesReadGrant {
		t.Fatalf("current exact grant widened raw shell authority: %#v", assessment)
	}
	if err := os.Rename(target, target+".old"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, []byte("replacement"), 0o644); err != nil {
		t.Fatal(err)
	}
	if assessment := ag.assessAutoScopedCommand("cat " + target); assessment.admitted() {
		t.Fatalf("replacement inherited exact read authority: %#v", assessment)
	}
}

func TestAutoCommandAssessmentRejectsShellStateAndEncodingBypasses(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("AUTO shell catalog requires a POSIX shell")
	}
	workspace := t.TempDir()
	hostBin := t.TempDir()
	for _, name := range []string{"go", "printf"} {
		if err := os.WriteFile(filepath.Join(hostBin, name), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", hostBin+string(os.PathListSeparator)+os.Getenv("PATH"))
	ag := New(nil, nil, 4096)
	ag.SetWorkDir(workspace)

	for _, command := range []string{
		"printf -v PATH . && go version",
		"printf -vPATH . && go version",
		string([]byte{'g', 'o', ' ', 'v', 'e', 'r', 's', 'i', 'o', 'n', 0xff}),
	} {
		if assessment := ag.assessAutoScopedCommand(command); assessment.admitted() {
			t.Fatalf("shell state/encoding bypass gained AUTO authority: %q (%#v)", command, assessment)
		}
	}
}

func TestAutoCommandAssessmentClassifiesSortOutputAsMutation(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("AUTO shell catalog requires a POSIX shell")
	}
	workspace := t.TempDir()
	hostBin := t.TempDir()
	if err := os.WriteFile(filepath.Join(hostBin, "sort"), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", hostBin+string(os.PathListSeparator)+os.Getenv("PATH"))
	ag := New(nil, nil, 4096)
	ag.SetWorkDir(workspace)

	for _, command := range []string{"sort -o out.txt input.txt", "sort --output=out.txt input.txt", "sort -boout.txt input.txt"} {
		assessment := ag.assessAutoScopedCommand(command)
		if !assessment.admitted() || assessment.effect != autoCommandEffectWorkspaceMutation {
			t.Fatalf("sort output assessment for %q = %#v, want admitted workspace mutation", command, assessment)
		}
	}
	assessment := ag.assessAutoScopedCommand("sort input.txt")
	if !assessment.admitted() || assessment.effect != autoCommandEffectReadOnly {
		t.Fatalf("read-only sort assessment = %#v", assessment)
	}
}

func TestAutoCommandRedirectsRequireWorkspaceTargets(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("AUTO shell catalog requires a POSIX shell")
	}
	workspace := t.TempDir()
	outside := t.TempDir()
	ag := New(nil, nil, 4096)
	ag.SetWorkDir(workspace)

	// A workspace target is exactly the authority the auto-approved write
	// builtin already has, so the command is admitted and classified as a
	// workspace mutation even when the producing command is read-only.
	mutation := ag.assessAutoScopedCommand("echo x > note.txt")
	if !mutation.admitted() || mutation.effect != autoCommandEffectWorkspaceMutation {
		t.Fatalf("workspace redirect assessment = %#v, want admitted workspace mutation", mutation)
	}
	appendAssessment := ag.assessAutoScopedCommand("echo x >> note.txt")
	if !appendAssessment.admitted() || appendAssessment.effect != autoCommandEffectWorkspaceMutation {
		t.Fatalf("workspace append assessment = %#v, want admitted workspace mutation", appendAssessment)
	}
	// The spaced /dev/null spelling must classify like the glued token: it
	// discards output and mutates nothing.
	discard := ag.assessAutoScopedCommand("echo x > /dev/null")
	if !discard.admitted() || discard.effect != autoCommandEffectReadOnly {
		t.Fatalf("spaced /dev/null redirect assessment = %#v, want admitted read-only", discard)
	}

	if err := os.Symlink(outside, filepath.Join(workspace, "escape-link")); err != nil {
		t.Fatal(err)
	}
	for _, command := range []string{
		"echo x > " + filepath.Join(outside, "pwn.txt"),
		"echo x >> " + filepath.Join(outside, "pwn.txt"),
		"echo x > ../pwn.txt",
		"echo x > ~/pwn.txt",
		"echo x > escape-link/pwn.txt",
	} {
		t.Run(command, func(t *testing.T) {
			assessment := ag.assessAutoScopedCommand(command)
			if assessment.admitted() {
				t.Fatalf("external redirect target gained AUTO authority: %#v", assessment)
			}
			if assessment.reason != autoCommandReasonRedirectTarget {
				t.Fatalf("external redirect refusal reason = %#v, want autoCommandReasonRedirectTarget", assessment)
			}
		})
	}
}

// TestSedProgramArgumentsAreNotPathOperands pins the refusal REASON, not the
// refusal: general sed programs stay approval-gated (a `w` command can create
// files), but an address regex such as `/<\/style>/,/<\/html>/p` is program
// text, and reporting its leading slash as "operand outside the workspace"
// sent the model into futile path shuffles.
func TestSedProgramArgumentsAreNotPathOperands(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("AUTO shell catalog requires a POSIX shell")
	}
	installGrantTestExecutables(t, "sed")
	ag := New(nil, nil, 4096)
	ag.SetWorkDir(t.TempDir())

	regexProgram := ag.assessAutoScopedCommand(`sed -n '/<\/style>/,/<\/html>/p' index.html`)
	if regexProgram.admitted() {
		t.Fatalf("regex sed program gained AUTO authority: %#v", regexProgram)
	}
	if regexProgram.reason == autoCommandReasonPathAuthority {
		t.Fatalf("sed program text was treated as a filesystem operand: %#v", regexProgram)
	}
	if regexProgram.reason != autoCommandReasonArguments {
		t.Fatalf("regex sed program refusal reason = %#v, want autoCommandReasonArguments", regexProgram)
	}

	// A -f value names a script FILE: that is a real path operand and keeps
	// real path authority.
	scriptFile := ag.assessAutoScopedCommand("sed -f /etc/evil.sed index.html")
	if scriptFile.admitted() || scriptFile.reason != autoCommandReasonPathAuthority {
		t.Fatalf("sed -f script path assessment = %#v, want path-authority refusal", scriptFile)
	}
	// So does an external INPUT file, even with an exempt program ahead of it.
	externalInput := ag.assessAutoScopedCommand("sed -n '1,120p' /etc/passwd")
	if externalInput.admitted() || externalInput.reason != autoCommandReasonPathAuthority {
		t.Fatalf("external sed input assessment = %#v, want path-authority refusal", externalInput)
	}
	// And the admitted print form is unchanged.
	if assessment := ag.assessAutoScopedCommand("sed -n '1,120p' index.html"); !assessment.admitted() {
		t.Fatalf("bounded sed print form lost AUTO authority: %#v", assessment)
	}
}

// TestUncataloguedInstalledExecutableReportsExecutableReason pins 5(b): an
// installed executable with no argument contract must say the EXECUTABLE is
// the problem — "arguments outside the host catalog" invited argument
// shuffles that could never succeed.
func TestUncataloguedInstalledExecutableReportsExecutableReason(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("AUTO shell catalog requires a POSIX shell")
	}
	installGrantTestExecutables(t, "xcrun", "go")
	ag := New(nil, nil, 4096)
	ag.SetWorkDir(t.TempDir())

	installed := ag.assessAutoScopedCommand("xcrun simctl list devices")
	if installed.admitted() || installed.reason != autoCommandReasonExecutableUncatalogued {
		t.Fatalf("installed uncatalogued executable assessment = %#v, want autoCommandReasonExecutableUncatalogued", installed)
	}
	// A name that does not resolve at all keeps the original reason.
	missing := ag.assessAutoScopedCommand("definitely-not-installed-fixture --version")
	if missing.admitted() || missing.reason != autoCommandReasonExecutable {
		t.Fatalf("unresolvable executable assessment = %#v, want autoCommandReasonExecutable", missing)
	}
	// A catalogued executable with a refused argument form keeps the
	// arguments reason — there, reshaping arguments genuinely can succeed.
	arguments := ag.assessAutoScopedCommand("go test -exec=curl ./...")
	if arguments.admitted() || arguments.reason != autoCommandReasonArguments {
		t.Fatalf("catalogued-executable argument refusal = %#v, want autoCommandReasonArguments", arguments)
	}
}

func TestAutoCommandAssessmentIsBounded(t *testing.T) {
	ag := New(nil, nil, 4096)
	ag.SetWorkDir(t.TempDir())
	commands := []string{
		strings.Repeat("x", maxAutoCommandBytes+1),
		strings.Repeat("echo x && ", maxAutoCommandSegments) + "echo x",
		"echo " + strings.Repeat("x ", maxAutoCommandWords),
	}
	for _, command := range commands {
		assessment := ag.assessAutoScopedCommand(command)
		if assessment.admitted() || assessment.reason != autoCommandReasonBounds {
			t.Fatalf("unbounded command assessment = %#v", assessment)
		}
	}
}

func addAutoCommandReadGrant(t *testing.T, ag *Agent, path string) {
	t.Helper()
	inspection, err := ag.InspectReadPath(path)
	if err != nil {
		t.Fatalf("inspect read grant: %v", err)
	}
	if _, err := ag.AddInspectedReadGrant(inspection.Grant()); err != nil {
		t.Fatalf("add read grant: %v", err)
	}
}

func TestAutoCommandReasonLabelIsBoundedOperatorFacingText(t *testing.T) {
	tests := []struct {
		name    string
		reason  autoCommandReason
		command string
		want    string
	}{
		{name: "empty", reason: autoCommandReasonEmpty, command: "   ", want: "empty command"},
		{name: "bounds", reason: autoCommandReasonBounds, command: strings.Repeat("x", maxAutoCommandBytes+1), want: "command exceeds the bounded shell subset"},
		{name: "dynamic command substitution in a pipeline", reason: autoCommandReasonDynamicSyntax, command: "go build $(pwd)", want: "dynamic shell syntax ($)"},
		{name: "dynamic command substitution", reason: autoCommandReasonDynamicSyntax, command: "go $(date)", want: "dynamic shell syntax ($)"},
		{name: "dynamic backtick", reason: autoCommandReasonDynamicSyntax, command: "echo `date`", want: "dynamic shell syntax (`)"},
		{name: "dynamic grouping", reason: autoCommandReasonDynamicSyntax, command: "echo {a,b}", want: "dynamic shell syntax ({)"},
		{name: "dynamic syntax without a command", reason: autoCommandReasonDynamicSyntax, command: "", want: "dynamic shell syntax"},
		{name: "ambiguous composition", reason: autoCommandReasonAmbiguousComposition, command: "cd /tmp && ls", want: "ambiguous command composition"},
		{name: "executable", reason: autoCommandReasonExecutable, command: "custom-tool", want: "executable outside the host catalog"},
		{name: "uncatalogued installed executable", reason: autoCommandReasonExecutableUncatalogued, command: "xcrun simctl list", want: "executable installed but outside the host catalog; changing arguments cannot admit it"},
		{name: "arguments", reason: autoCommandReasonArguments, command: "go build -ldflags x", want: "arguments outside the host catalog"},
		{name: "path authority", reason: autoCommandReasonPathAuthority, command: "cat ../secret.txt", want: "operand outside the workspace"},
		{name: "redirect target", reason: autoCommandReasonRedirectTarget, command: "go test ./... > /tmp/out.txt", want: "redirect only into workspace files; this redirect target resolves outside the workspace"},
		{name: "allowed", reason: autoCommandReasonAllowed, command: "go test ./...", want: "admitted by the scoped shell policy"},
		{name: "unknown reason fails closed", reason: autoCommandReason(255), command: "", want: "outside the scoped shell policy"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := autoCommandReasonLabel(tt.reason, tt.command); got != tt.want {
				t.Fatalf("reason label = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestAutoCommandApprovalReasonIsGatedToAUTOAndNonAdmitted(t *testing.T) {
	ag := New(nil, nil, 4096)
	ag.SetWorkDir(t.TempDir())

	tests := []struct {
		name    string
		mode    AuthorityMode
		command string
		want    string
	}{
		{name: "auto command substitution", mode: AuthorityAutoScoped, command: "go test $(pwd)", want: "dynamic shell syntax ($)"},
		{name: "auto admitted command", mode: AuthorityAutoScoped, command: "go test ./...", want: ""},
		{name: "normal never labels", mode: AuthorityNormal, command: "go test $(pwd)", want: ""},
		{name: "plan never labels", mode: AuthorityPlan, command: "go test $(pwd)", want: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ag.autoCommandApprovalReason(tt.mode, tt.command); got != tt.want {
				t.Fatalf("approval reason = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestNonWalkingGrepIsAdmittedAndWalkingGrepIsNot pins where the search
// boundary actually sits: at directory traversal, not at the executable's name.
//
// Refusing every grep cost seven of the nine approvals in session 8c7ca7f, and
// explained itself wrongly — each was `grep -n <pattern> <explicit files>`,
// told it was "raw recursive search" when nothing about it recursed. The
// per-operand loop resolves a named path through the workspace and the host
// secret policy, so the only read that escapes those checks is one the command
// finds for itself.
func TestNonWalkingGrepIsAdmittedAndWalkingGrepIsNot(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("AUTO shell catalog requires a POSIX shell")
	}
	workspace := t.TempDir()
	for _, name := range []string{"app.go", "notes.md", ".env"} {
		if err := os.WriteFile(filepath.Join(workspace, name), []byte("TODO\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(filepath.Join(workspace, "internal"), 0o755); err != nil {
		t.Fatal(err)
	}
	ag := New(nil, nil, 4096)
	ag.SetWorkDir(workspace)

	for _, command := range []string{
		"grep -n TODO app.go",
		"grep -n TODO app.go notes.md",
		"grep -c TODO app.go",
		"grep -in TODO app.go",
		"grep -A2 -B2 TODO app.go",
		"grep -E 'TODO|FIXME' app.go",
		"grep -e TODO -- app.go",
		"cat app.go | grep TODO",
		"sed -n 1,5p app.go && echo ==== && grep -n TODO app.go | head",
	} {
		t.Run("admits/"+command, func(t *testing.T) {
			if assessment := ag.assessAutoScopedCommand(command); !assessment.admitted() {
				t.Fatalf("non-walking grep still costs an approval: %#v", assessment)
			}
		})
	}

	for _, command := range []string{
		// Every spelling of a walk, including inside a POSIX cluster and the
		// forms that reach recursion through --directories rather than -r.
		"grep -r TODO internal",
		"grep -R TODO internal",
		"grep -rn TODO internal",
		"grep -nr TODO internal",
		"grep -Rn TODO internal",
		"grep -rln TODO .",
		"grep --recursive TODO internal",
		"grep --dereference-recursive TODO internal",
		"grep -d recurse TODO internal",
		"grep -nd recurse TODO internal",
		"grep --directories=recurse TODO internal",
		"grep --exclude-dir=vendor -rn TODO .",
		// Path authority is unchanged and independent: an operand the ignore
		// policy or the workspace boundary excludes is refused either way.
		"grep -n SECRET .env",
		"grep -n TODO /etc/hosts",
	} {
		t.Run("refuses/"+command, func(t *testing.T) {
			if assessment := ag.assessAutoScopedCommand(command); assessment.admitted() {
				t.Fatalf("a walking or out-of-bounds grep gained AUTO authority: %#v", assessment)
			}
		})
	}

	// The tools whose whole purpose is enumeration keep their refusal: there is
	// no non-walking form of them to admit, and rg walks with no operand at all.
	for _, command := range []string{"rg TODO", "rg TODO app.go", "find . -name '*.go'", "ls -R .", "tree .", "du -sh ."} {
		t.Run("still-gated/"+command, func(t *testing.T) {
			if assessment := ag.assessAutoScopedCommand(command); assessment.admitted() {
				t.Fatalf("a directory enumerator gained AUTO authority: %#v", assessment)
			}
		})
	}
}

// TestPackageRunScriptsAreScopedToLocalVerification pins the one gap the audit
// found on the deny side. `npm test` was always admitted on the reasoning that
// the script name grants no new authority, and that reasoning silently carried
// `npm run migrate` and `npm run deploy` with it — durable external effect,
// unattended, which is the exact outcome the reasoning existed to prevent.
func TestPackageRunScriptsAreScopedToLocalVerification(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("AUTO shell catalog requires a POSIX shell")
	}
	ag := New(nil, nil, 4096)
	ag.SetWorkDir(t.TempDir())

	for _, command := range []string{
		"npm test", "npm run test", "npm run lint", "npm run build",
		"npm run typecheck", "npm run format", "npm run coverage",
		// Every segment is read, not just the head: real manifests do not put
		// the verb first, and a head-only rule refuses all three of these.
		"npm run build:ci", "npm run test:unit", "npm run lint:fix",
		"npm run site:build", "npm run build-storybook", "npm run test_e2e",
		"npm run type-check", "npm run docs:build",
		"pnpm run check", "yarn run e2e", "bun run tsc",
	} {
		t.Run("admits/"+command, func(t *testing.T) {
			if assessment := ag.assessAutoScopedCommand(command); !assessment.admitted() {
				t.Fatalf("routine verification script costs an approval: %#v", assessment)
			}
		})
	}

	for _, command := range []string{
		"npm run migrate", "npm run deploy", "npm run release", "npm run publish",
		"npm run db:push", "npm run db:migrate", "npm run deploy:prod",
		"pnpm run seed", "yarn run sync-prod", "bun run start",
		// A verification segment does not redeem an effectful one — this is the
		// half of the rule that a first-match reading would get wrong.
		"npm run deploy-check", "npm run migrate-test", "npm run test:migrate",
		"npm run seed:test", "npm run publish-docs",
		// A name that says prod is not one the host can vouch for, even when it
		// is usually a local build. One approval, and `w` makes it durable.
		"npm run build:prod",
		// An unclassifiable name costs one approval rather than being guessed at.
		"npm run smoke",
	} {
		t.Run("refuses/"+command, func(t *testing.T) {
			if assessment := ag.assessAutoScopedCommand(command); assessment.admitted() {
				t.Fatalf("externally effectful script ran unattended: %#v", assessment)
			}
		})
	}
}

// TestListingToolsRequireExplicitFileOperands pins the widening for tools whose
// default IS the walk: they are admitted only in the form that cannot discover
// an entry, so the per-operand checks see everything the command will touch.
func TestListingToolsRequireExplicitFileOperands(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("AUTO shell catalog requires a POSIX shell")
	}
	workspace := t.TempDir()
	for _, name := range []string{"app.go", "notes.md", ".env"} {
		if err := os.WriteFile(filepath.Join(workspace, name), []byte("x\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(filepath.Join(workspace, "src"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(workspace, "src"), filepath.Join(workspace, "link")); err != nil {
		t.Fatal(err)
	}
	ag := New(nil, nil, 4096)
	ag.SetWorkDir(workspace)

	for _, command := range []string{
		"ls -l app.go", "ls -la app.go notes.md", "ls app.go", "du -h app.go", "du app.go notes.md",
	} {
		t.Run("admits/"+command, func(t *testing.T) {
			if assessment := ag.assessAutoScopedCommand(command); !assessment.admitted() {
				t.Fatalf("explicit-file listing costs an approval: %#v", assessment)
			}
		})
	}

	for _, command := range []string{
		// No operand: the tool picks its own targets.
		"ls", "ls -la", "du -sh",
		// A directory operand enumerates, at one level or all of them.
		"ls -l src", "ls .", "du -sh src", "du .",
		// A symlink to a directory enumerates just as well.
		"ls -l link", "du -sh link",
		// Recursion, and a missing path the checks above never saw.
		"ls -R .", "ls -l ghost.go",
		// Path authority is unchanged and independent.
		"ls -l .env", "ls -l /etc",
	} {
		t.Run("refuses/"+command, func(t *testing.T) {
			if assessment := ag.assessAutoScopedCommand(command); assessment.admitted() {
				t.Fatalf("a listing that discovers entries gained AUTO authority: %#v", assessment)
			}
		})
	}
}

// TestInterpretersRunWorkspaceScriptsOnly pins the argv shape as the boundary:
// the script path must be the first word, so every form that makes the
// interpreter itself the program is refused by being unreachable rather than by
// a flag denylist chasing three languages' options.
func TestInterpretersRunWorkspaceScriptsOnly(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("AUTO shell catalog requires a POSIX shell")
	}
	installGrantTestExecutables(t, "node", "python", "python3")
	workspace := t.TempDir()
	for _, name := range []string{"tool.js", "script.py"} {
		if err := os.WriteFile(filepath.Join(workspace, name), []byte("//\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	ag := New(nil, nil, 4096)
	ag.SetWorkDir(workspace)

	for _, command := range []string{
		"node tool.js",
		"node tool.js --verbose",
		"python script.py",
		"python3 script.py",
		// Words after the script belong to the script: `-c foo` lands in
		// sys.argv, it never reaches the interpreter.
		"python3 script.py -c foo",
	} {
		t.Run("admits/"+command, func(t *testing.T) {
			if assessment := ag.assessAutoScopedCommand(command); !assessment.admitted() {
				t.Fatalf("workspace script costs an approval: %#v", assessment)
			}
		})
	}

	for _, command := range []string{
		"node -e 'process.exit(1)'", "node --eval x", "node -p 1+1",
		"python -c 'import os'", "python3 --command x", "python -m http.server",
		"node --require ./preload tool.js", "node -r ./preload tool.js",
		"node --inspect tool.js", "python -i script.py",
		// The bare stdin form and an operand outside the workspace.
		"node", "python3", "node -", "node /tmp/other.js",
	} {
		t.Run("refuses/"+command, func(t *testing.T) {
			if assessment := ag.assessAutoScopedCommand(command); assessment.admitted() {
				t.Fatalf("interpreter ran something other than a workspace script: %#v", assessment)
			}
		})
	}
}

// TestGitListingVerbsAreAdmittedOnlyInExactForms covers the four ref/remote
// verbs that cannot join the subcommand allowlist: each of them mutates through
// a positional operand, so the admitted forms carry none.
func TestGitListingVerbsAreAdmittedOnlyInExactForms(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("AUTO shell catalog requires a POSIX shell")
	}
	ag := New(nil, nil, 4096)
	ag.SetWorkDir(t.TempDir())

	for _, command := range []string{
		"git branch --list", "git branch --list -a", "git branch --list --remotes -v",
		"git tag --list", "git remote", "git remote -v", "git remote --verbose",
		"git stash list", "git worktree list",
	} {
		t.Run("admits/"+command, func(t *testing.T) {
			if assessment := ag.assessAutoScopedCommand(command); !assessment.admitted() {
				t.Fatalf("read-only git listing costs an approval: %#v", assessment)
			}
		})
	}

	for _, command := range []string{
		// A positional operand is how every one of these mutates.
		"git branch newthing", "git tag v1.0", "git branch -D old", "git branch -d old",
		"git tag -d v1.0", "git remote add origin https://example.com/x.git",
		"git remote remove origin", "git stash", "git stash push", "git stash pop",
		"git worktree add ../elsewhere", "git worktree remove ../elsewhere",
		// -l means --list for tag and has meant --create-reflog for branch,
		// and git changed its preference mid-history. One letter, two meanings,
		// version dependent: refused in both.
		"git branch -l", "git tag -l",
		// show contacts the remote.
		"git remote show origin", "git remote update",
		// A listing flag without --list is not a listing.
		"git branch -a", "git branch -v",
	} {
		t.Run("refuses/"+command, func(t *testing.T) {
			if assessment := ag.assessAutoScopedCommand(command); assessment.admitted() {
				t.Fatalf("a mutating or network git form gained AUTO authority: %#v", assessment)
			}
		})
	}
}

// TestSegmentBreakdownMarksTheRefusingPart is the reader-facing half of the
// grant fix. Since 8c7ca7f the offer points at the segment that refused; this
// makes that segment visible instead of leaving the reader to work backwards
// from a prefix and a wall of shell text.
func TestSegmentBreakdownMarksTheRefusingPart(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("AUTO shell catalog requires a POSIX shell")
	}
	installGrantTestExecutables(t, "sed", "grep")
	workspace := t.TempDir()
	if err := os.WriteFile(filepath.Join(workspace, "notes.go"), []byte("package notes\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	ag := New(nil, nil, 4096)
	ag.SetWorkDir(workspace)

	// The audited shape: only the grep recurses, and only the grep refuses.
	got := ag.autoCommandSegmentBreakdown(AuthorityAutoScoped,
		"cd "+workspace+" && sed -n 1,5p notes.go && echo ==== && grep -rn TODO notes.go")
	if got != "cd · sed · echo · grep ←" {
		t.Fatalf("segment breakdown = %q", got)
	}

	// A single segment needs no breakdown: the Command row already is one.
	if got := ag.autoCommandSegmentBreakdown(AuthorityAutoScoped, "grep -rn TODO notes.go"); got != "" {
		t.Fatalf("a single segment produced a breakdown: %q", got)
	}
	// Neither does an admitted command, or one outside AUTO.
	if got := ag.autoCommandSegmentBreakdown(AuthorityAutoScoped, "sed -n 1,5p notes.go && cat notes.go"); got != "" {
		t.Fatalf("an admitted command produced a breakdown: %q", got)
	}
	if got := ag.autoCommandSegmentBreakdown(AuthorityNormal, "sed -n 1,5p notes.go && grep -rn x notes.go"); got != "" {
		t.Fatalf("NORMAL mode produced a breakdown: %q", got)
	}
}

// A leader is model-supplied text on its way to a terminal. It is reduced to a
// base name, stripped of anything that could reflow or escape the modal, and
// bounded — and a long chain is truncated rather than allowed to wrap.
func TestSegmentBreakdownBoundsWhatItPrints(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("AUTO shell catalog requires a POSIX shell")
	}
	for _, tc := range []struct{ word, want string }{
		{"grep", "grep"},
		{"/usr/local/bin/grep", "grep"},
		{"", "?"},
		{strings.Repeat("x", 80), strings.Repeat("x", 24)},
		{"a\tb", "ab"},
	} {
		if got := sanitizeAutoSegmentLeader(tc.word); got != tc.want {
			t.Fatalf("sanitizeAutoSegmentLeader(%q) = %q, want %q", tc.word, got, tc.want)
		}
	}
}
