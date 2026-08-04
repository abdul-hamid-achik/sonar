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
		"bun", "cargo", "date", "go", "gofmt", "grep", "head", "npm", "rg", "sed",
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
		"cd internal/queue && go test ./...",
		"gofmt -w internal/queue/policy.go && go test ./internal/queue",
		"sed -n '1,120p' internal/queue/policy.go",
		"sed -n '$p' internal/queue/policy.go",
		"sed -n '1p' -- internal/queue/policy.go",
		"go test -run 'Test(Foo|Bar)' ./...",
		"go test -run=/subtest ./...",
		"npm test",
		"bun test",
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
		"npm run deploy",
		"npm run site:deploy",
		"npm run site",
		"bun run site",
		"npm run build:watch",
		"npm run test:watch",
		"npm run site:dev",
		"npm run docs:serve",
		"npm run site:preview",
		"npm run test:ui",
		"npm run test:open",
		"npm run test:debug",
		"npm run lint:inspect",
		"npm run lint:mcp",
		"npm run format -- --plugin=file:///private/tmp/plugin.mjs",
		"npm run lint -- --inspect-config",
		"npm test --node-options=--inspect-wait",
		"npm test --node-op=--inspect-wait",
		"npm run BUILD",
		`npm run "build "`,
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
		"npm run site:build",
		"pnpm run test",
		"yarn run lint",
		"bun run build",
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
		"go test ./... > result.txt",
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
	for _, command := range []string{"go test &", "go test &&", "'unterminated", "go test < input"} {
		if _, _, ok := splitStaticShellCommands(command); ok {
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
	commands, separators, ok := splitStaticShellCommands(`grep '' README.md && rg -e "" .`)
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
