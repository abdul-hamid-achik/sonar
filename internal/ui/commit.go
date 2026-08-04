package ui

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/abdul-hamid-achik/sonar/internal/llm"
)

const (
	commitDiffTimeout    = 10 * time.Second
	commitMessageTimeout = 2 * time.Minute
	commitGitTimeout     = time.Minute
	commitDiffPromptCap  = 8 * 1024
	commitOutputCap      = 64 * 1024
)

type commitTimeouts struct {
	diff    time.Duration
	message time.Duration
	commit  time.Duration
}

var defaultCommitTimeouts = commitTimeouts{
	diff:    commitDiffTimeout,
	message: commitMessageTimeout,
	commit:  commitGitTimeout,
}

type commitGit interface {
	StagedDiff(context.Context) (string, error)
	Commit(context.Context, string) error
}

type commandCommitGit struct {
	dir string
}

// ownedGitConfig disables repository-configurable execution surfaces for the
// automated /commit transaction. Identity and ordinary data-only Git config
// remain available, but hooks, fsmonitor helpers, signing programs, and
// detached automatic maintenance must not outlive the owned command.
var ownedGitConfig = []string{
	"core.hooksPath=" + os.DevNull,
	"core.fsmonitor=false",
	"commit.gpgSign=false",
	"commit.template=",
	"maintenance.auto=false",
	"maintenance.autoDetach=false",
	"gc.auto=0",
	"gc.autoDetach=false",
}

type commitEffectRunner func(context.Context, llm.Client, string, string, string, uint64) tea.Cmd

// runCommit gets the staged diff, asks the LLM for a commit message, and
// commits. The caller owns ctx and must wait for the returned tokened receipt
// before allowing graceful process exit.
func runCommit(ctx context.Context, client llm.Client, model, extraMsg, workDir string, token uint64) tea.Cmd {
	return runCommitWithGit(ctx, client, model, extraMsg, token, commandCommitGit{dir: workDir}, defaultCommitTimeouts)
}

func runCommitWithGit(
	ctx context.Context,
	client llm.Client,
	model string,
	extraMsg string,
	token uint64,
	git commitGit,
	timeouts commitTimeouts,
) tea.Cmd {
	return func() (result tea.Msg) {
		defer func() {
			if recovered := recover(); recovered != nil {
				result = CommitResultMsg{Token: token, Err: fmt.Errorf("commit effect panicked: %v", recovered)}
			}
		}()
		if client == nil {
			return CommitResultMsg{Token: token, Err: fmt.Errorf("LLM client is unavailable")}
		}

		diffCtx, cancelDiff := context.WithTimeout(ctx, timeouts.diff)
		diff, err := git.StagedDiff(diffCtx)
		diffContextErr := diffCtx.Err()
		cancelDiff()
		if diffContextErr != nil {
			return CommitResultMsg{Token: token, Err: fmt.Errorf("git diff: %w", diffContextErr)}
		}
		if err != nil {
			return CommitResultMsg{Token: token, Err: fmt.Errorf("git diff: %w", err)}
		}
		if strings.TrimSpace(diff) == "" {
			return CommitResultMsg{Token: token, Err: fmt.Errorf("no staged changes (use `git add` first)")}
		}

		if len(diff) > commitDiffPromptCap {
			diff = diff[:commitDiffPromptCap] + "\n... (truncated)"
		}

		prompt := "Write a concise git commit message for the following staged diff. " +
			"Return ONLY the commit message, no explanation or markdown. " +
			"Use conventional commit style (e.g. feat:, fix:, refactor:). " +
			"Keep the first line under 72 characters."
		if extraMsg != "" {
			prompt += "\n\nAdditional context: " + extraMsg
		}
		prompt += "\n\nDiff:\n" + diff

		var msgBuf strings.Builder
		messageCtx, cancelMessage := context.WithTimeout(ctx, timeouts.message)
		err = client.ChatStream(messageCtx, llm.ChatOptions{
			Messages: []llm.Message{{Role: "user", Content: prompt}},
			System:   "You are a helpful assistant that writes git commit messages.",
		}, func(chunk llm.StreamChunk) error {
			if chunk.Text != "" {
				msgBuf.WriteString(chunk.Text)
			}
			return nil
		})
		messageContextErr := messageCtx.Err()
		cancelMessage()
		if messageContextErr != nil {
			return CommitResultMsg{Token: token, Err: fmt.Errorf("LLM error: %w", messageContextErr)}
		}
		if err != nil {
			if llm.IsRemoteInferenceError(err) {
				// OpenAI-compatible response bodies and SSE errors are useful
				// transient diagnostics inside the provider adapter, but they
				// must not cross into CommitResultMsg: handleCommitResult writes
				// that receipt to the transcript and durable session state.
				return CommitResultMsg{
					Token: token,
					Err:   fmt.Errorf("LLM error: %s", ProviderFailureCopy),
				}
			}
			return CommitResultMsg{Token: token, Err: fmt.Errorf("LLM error: %w", err)}
		}
		if err := ctx.Err(); err != nil {
			return CommitResultMsg{Token: token, Err: fmt.Errorf("commit cancelled before git mutation: %w", err)}
		}

		commitMsg := strings.TrimSpace(msgBuf.String())
		if commitMsg == "" {
			return CommitResultMsg{Token: token, Err: fmt.Errorf("LLM returned empty commit message")}
		}
		commitMsg += fmt.Sprintf("\n\nAssisted-by: sonar (%s)", model)

		commitCtx, cancelCommit := context.WithTimeout(ctx, timeouts.commit)
		err = git.Commit(commitCtx, commitMsg)
		commitContextErr := commitCtx.Err()
		cancelCommit()
		if err != nil {
			if commitContextErr != nil {
				return CommitResultMsg{
					Token: token,
					Err: fmt.Errorf(
						"git commit outcome unknown after cancellation; verify HEAD before retrying: %w",
						commitContextErr,
					),
				}
			}
			return CommitResultMsg{Token: token, Err: fmt.Errorf("git commit: %w", err)}
		}

		return CommitResultMsg{Token: token, Message: commitMsg}
	}
}

func (g commandCommitGit) StagedDiff(ctx context.Context) (string, error) {
	stat, err := g.output(ctx, "diff", "--cached", "--stat", "--no-ext-diff")
	if err != nil {
		return "", err
	}
	diff, err := g.output(ctx, "diff", "--cached", "--no-ext-diff", "--no-textconv")
	if err != nil {
		return "", err
	}
	return stat + "\n" + diff, nil
}

func (g commandCommitGit) Commit(ctx context.Context, msg string) error {
	cmd := g.command(ctx, "commit", "-m", msg)
	var stderr limitedBuffer
	stderr.limit = commitOutputCap
	cmd.Stderr = &stderr
	if err := runOwnedTUICommand(cmd); err != nil {
		if contextErr := ctx.Err(); contextErr != nil {
			return contextErr
		}
		return fmt.Errorf("%w: %s", err, strings.TrimSpace(stderr.String()))
	}
	return nil
}

func (g commandCommitGit) output(ctx context.Context, args ...string) (string, error) {
	cmd := g.command(ctx, args...)
	var stdout, stderr limitedBuffer
	stdout.limit = commitOutputCap
	stderr.limit = commitOutputCap
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := runOwnedTUICommand(cmd); err != nil {
		if contextErr := ctx.Err(); contextErr != nil {
			return "", contextErr
		}
		return "", fmt.Errorf("%w: %s", err, strings.TrimSpace(stderr.String()))
	}
	return stdout.String(), nil
}

func (g commandCommitGit) command(ctx context.Context, args ...string) *exec.Cmd {
	ownedArgs := make([]string, 0, 1+len(ownedGitConfig)*2+len(args))
	ownedArgs = append(ownedArgs, "--no-pager")
	for _, setting := range ownedGitConfig {
		ownedArgs = append(ownedArgs, "-c", setting)
	}
	ownedArgs = append(ownedArgs, args...)
	cmd := exec.CommandContext(ctx, "git", ownedArgs...)
	configureTUICommandProcessGroup(cmd)
	cmd.WaitDelay = 2 * time.Second
	cmd.Dir = g.dir
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	return cmd
}

// runOwnedTUICommand waits for the process-group leader, then performs a final
// bounded sweep of its group. The initial context cancellation kill can race a
// shell or hook that is concurrently forking: a child created just after that
// signal enumeration would otherwise survive cmd.Wait and mutate later.
func runOwnedTUICommand(cmd *exec.Cmd) error {
	err := cmd.Run()
	cleanupTUICommandProcessGroup(cmd)
	return err
}

type limitedBuffer struct {
	bytes.Buffer
	limit int
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	originalLength := len(p)
	remaining := b.limit - b.Len()
	if remaining > 0 {
		if len(p) > remaining {
			p = p[:remaining]
		}
		_, _ = b.Buffer.Write(p)
	}
	return originalLength, nil
}

// handleCommitResult applies a tokened /commit receipt.
func (m *Model) handleCommitResult(msg CommitResultMsg, cmds []tea.Cmd) []tea.Cmd {
	if !m.commitRunning || msg.Token != m.commitToken {
		return cmds
	}
	m.commitRunning = false
	if m.commitCancel != nil {
		m.commitCancel()
		m.commitCancel = nil
	}
	if !m.shuttingDown {
		m.input.Focus()
	}
	if msg.Err != nil {
		m.entries = append(m.entries, ChatEntry{
			Kind:    "error",
			Content: fmt.Sprintf("Commit failed: %v", msg.Err),
		})
	} else {
		m.entries = append(m.entries, ChatEntry{
			Kind:    "system",
			Content: fmt.Sprintf("Committed with message:\n%s", msg.Message),
		})
	}
	m.invalidateEntryCache()
	m.refreshTranscript()
	m.gotoBottomIfFollowing()
	m.appendShutdownQuit(&cmds)
	return cmds
}
