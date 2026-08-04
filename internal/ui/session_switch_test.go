package ui

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/abdul-hamid-achik/sonar/internal/db"
)

func TestSessionSwitchBoundaryProtectsDraftAndImagesOnCancel(t *testing.T) {
	m, _ := newImageTestModel(t)
	// Hex handles are 7 characters, so the compact decision row needs a bit more
	// width than the old S-prefix labels to keep "1 image" fully visible.
	updated, _ := m.Update(tea.WindowSizeMsg{Width: 40, Height: 12})
	m = updated.(*Model)
	path := filepath.Join(t.TempDir(), "switch.png")
	writeImageAttachmentFixture(t, path)
	attachImageFixture(t, m, path, "")
	wantImage := clonePendingImages(m.pendingImages)
	draft := "first line\nsecond line"
	m.setComposerDraftAtRune(draft, 5)

	if cmd := m.beginSessionSwitch(42, "aaaaa2a", "Saved work"); cmd != nil {
		t.Fatal("nonempty composer bypassed the session-switch decision")
	}
	if m.pendingSessionSwitch == nil || m.composerEditable() {
		t.Fatalf("session decision ownership = pending %#v editable %v", m.pendingSessionSwitch, m.composerEditable())
	}
	view := ansi.Strip(m.View().Content)
	for _, want := range []string{"Open aaaaa2a?", "2 lines", "1 image", "keep both", "discard both", "esc"} {
		if !strings.Contains(view, want) {
			t.Fatalf("minimum switch decision omitted %q:\n%s", want, view)
		}
	}
	if got := len(strings.Split(strings.TrimRight(m.View().Content, "\n"), "\n")); got > m.height {
		t.Fatalf("minimum switch decision rendered %d rows, want <= %d", got, m.height)
	}

	updated, _ = m.Update(escKey())
	m = updated.(*Model)
	if m.pendingSessionSwitch != nil || m.input.Value() != draft ||
		textareaCursorRuneOffset(m.input.Value(), m.input.Line(), m.input.Column()) != 5 {
		t.Fatalf("cancelled switch lost draft/cursor: pending=%#v draft=%q cursor=%d", m.pendingSessionSwitch, m.input.Value(), textareaCursorRuneOffset(m.input.Value(), m.input.Line(), m.input.Column()))
	}
	assertPendingImagesEqual(t, m.pendingImages, wantImage)
}

func TestSessionSwitchPromptKeepsTargetIdentityAcrossWidths(t *testing.T) {
	for _, width := range []int{30, 40, 80} {
		m := newTestModel(t)
		updated, _ := m.Update(tea.WindowSizeMsg{Width: width, Height: 24})
		m = updated.(*Model)
		m.input.SetValue("carry this draft")
		m.beginSessionSwitch(42, "aaaaa2a", "Saved work")

		prompt := ansi.Strip(m.renderSessionSwitchPrompt(m.chatPaneWidth()))
		if !strings.Contains(prompt, "aaaaa2a") {
			t.Fatalf("width %d switch prompt hid target handle: %q", width, prompt)
		}
		if width >= 72 && !strings.Contains(prompt, "Saved work") {
			t.Fatalf("width %d switch prompt hid target title: %q", width, prompt)
		}
		for _, line := range strings.Split(prompt, "\n") {
			if got := lipgloss.Width(line); got > m.chatPaneWidth() {
				t.Fatalf("width %d prompt line is %d cells, pane %d: %q", width, got, m.chatPaneWidth(), line)
			}
		}
	}
}

func TestPendingSessionSwitchRejectsUnboundReceipts(t *testing.T) {
	tests := []struct {
		name    string
		choice  sessionSwitchChoice
		token   uint64
		session int64
		wantErr bool
	}{
		{name: "exact keep", choice: sessionSwitchKeep, token: 7, session: 42},
		{name: "exact discard", choice: sessionSwitchDiscard, token: 7, session: 42},
		{name: "unsettled choice", choice: sessionSwitchUndecided, token: 7, session: 42, wantErr: true},
		{name: "missing token", choice: sessionSwitchKeep, session: 42, wantErr: true},
		{name: "wrong token", choice: sessionSwitchKeep, token: 8, session: 42, wantErr: true},
		{name: "wrong target", choice: sessionSwitchDiscard, token: 7, session: 43, wantErr: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			m := newTestModel(t)
			m.pendingSessionSwitch = &pendingSessionSwitch{
				TargetSessionID: 42,
				Choice:          test.choice,
				LoadToken:       7,
			}
			err := m.validatePendingSessionSwitch(SessionLoadedMsg{LoadToken: test.token, SessionID: test.session})
			if (err != nil) != test.wantErr {
				t.Fatalf("validatePendingSessionSwitch() error = %v, wantErr %v", err, test.wantErr)
			}
		})
	}
}

func TestSessionSwitchTargetMismatchRestoresOriginalDraft(t *testing.T) {
	for _, choice := range []sessionSwitchChoice{sessionSwitchKeep, sessionSwitchDiscard} {
		name := "keep"
		if choice == sessionSwitchDiscard {
			name = "discard"
		}
		t.Run(name, func(t *testing.T) {
			m, _ := newImageTestModel(t)
			firstPath := filepath.Join(t.TempDir(), "first.png")
			secondPath := filepath.Join(t.TempDir(), "second.png")
			writeImageAttachmentFixtureVariant(t, firstPath, 1)
			writeImageAttachmentFixtureVariant(t, secondPath, 2)
			attachImageFixture(t, m, firstPath, "")
			attachImageFixture(t, m, secondPath, "")
			wantImages := clonePendingImages(m.pendingImages)

			m.localOnly = true
			m.ollamaModels = append(m.ollamaModels, OllamaModelDescriptor{
				Name: "qwen:cloud", Source: OllamaModelCloud, Selectable: true, Fit: true, RequiresConsent: true,
			})
			m.sessionID = 7
			m.sessionPublicID = "aaaaaa7"
			m.activeSessionTitle = "Source session"
			m.setComposerDraftAtRune("preserve exact draft", 8)
			m.beginSessionSwitch(42, "aaaaa2a", "Expected target")
			m.pendingSessionSwitch.Choice = choice
			m.pendingSessionSwitch.LoadToken = 7
			m.sessionLoadToken = 7
			m.sessionLoading = true

			updated, _ := m.Update(SessionLoadedMsg{
				LoadToken: 7,
				SessionID: 43,
				State: persistedSessionState{
					Version: currentPersistedSessionVersion,
					Mode:    ModeNormal,
					Model:   "qwen:cloud",
				},
				StateRecord: db.SessionStateRecord{
					SessionID: 43,
					Revision:  1,
				},
			})
			m = updated.(*Model)
			if m.sessionID != 7 || m.activeSessionTitle != "Source session" || m.model != "vision-model" ||
				m.pendingSessionSwitch != nil || m.input.Value() != "preserve exact draft" ||
				textareaCursorRuneOffset(m.input.Value(), m.input.Line(), m.input.Column()) != 8 {
				t.Fatalf("mismatched receipt changed boundary: session=%d title=%q model=%q pending=%#v draft=%q cursor=%d",
					m.sessionID, m.activeSessionTitle, m.model, m.pendingSessionSwitch, m.input.Value(),
					textareaCursorRuneOffset(m.input.Value(), m.input.Line(), m.input.Column()))
			}
			assertPendingImagesEqual(t, m.pendingImages, wantImages)
			if len(m.entries) == 0 || !strings.Contains(m.entries[len(m.entries)-1].Content, "does not match") {
				t.Fatalf("mismatched receipt has no exact failure notice: %#v", m.entries)
			}
		})
	}
}

func TestSessionSwitchKeepAndDiscardCommitAtomicallyAfterSuccess(t *testing.T) {
	for _, choice := range []sessionSwitchChoice{sessionSwitchKeep, sessionSwitchDiscard} {
		name := "keep"
		if choice == sessionSwitchDiscard {
			name = "discard"
		}
		t.Run(name, func(t *testing.T) {
			store, err := db.OpenPath(filepath.Join(t.TempDir(), "switch.db"))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = store.Close() })
			workspace := canonicalSessionTestWorkspace(t)
			target := createRestorableSession(t, store, workspace, "target session", "restored transcript")
			m, _ := newImageTestModel(t)
			m.SetSessionStore(store)
			m.agent.SetWorkDir(workspace)
			path := filepath.Join(t.TempDir(), "keep.png")
			writeImageAttachmentFixture(t, path)
			attachImageFixture(t, m, path, "")
			wantImages := clonePendingImages(m.pendingImages)
			m.setComposerDraftAtRune("carry this draft", 6)
			m.beginSessionSwitch(target.ID, target.PublicID, target.Title)

			cmd := m.startPendingSessionSwitch(choice)
			if cmd == nil || !m.sessionLoading {
				t.Fatalf("%s did not start restore", name)
			}
			receipt := awaitCommandMessage[SessionLoadedMsg](t, commandMessages(cmd), 2*time.Second)
			updated, _ := m.Update(receipt)
			m = updated.(*Model)
			if m.sessionID != target.ID || m.pendingSessionSwitch != nil {
				t.Fatalf("%s restore state = session %d boundary %#v", name, m.sessionID, m.pendingSessionSwitch)
			}
			if choice == sessionSwitchKeep {
				if m.input.Value() != "carry this draft" || textareaCursorRuneOffset(m.input.Value(), m.input.Line(), m.input.Column()) != 6 {
					t.Fatalf("kept draft/cursor = %q/%d", m.input.Value(), textareaCursorRuneOffset(m.input.Value(), m.input.Line(), m.input.Column()))
				}
				assertPendingImagesEqual(t, m.pendingImages, wantImages)
			} else if m.input.Value() != "" || len(m.pendingImages) != 0 {
				t.Fatalf("discard left draft=%q images=%d", m.input.Value(), len(m.pendingImages))
			}
			if err := m.ReleaseExecutionSessionLease(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestSessionSwitchFailureAndLoadCancelPreservePayload(t *testing.T) {
	for _, test := range []struct {
		name   string
		settle func(*Model)
	}{
		{name: "failed receipt", settle: func(m *Model) {
			_, _ = m.Update(SessionLoadedMsg{LoadToken: m.sessionLoadToken, Err: errors.New("database unavailable")})
		}},
		{name: "escape", settle: func(m *Model) {
			_, _ = m.Update(escKey())
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			m, _ := newImageTestModel(t)
			path := filepath.Join(t.TempDir(), "preserve.png")
			writeImageAttachmentFixture(t, path)
			attachImageFixture(t, m, path, "")
			wantImages := clonePendingImages(m.pendingImages)
			m.setComposerDraftAtRune("preserve me", 4)
			m.beginSessionSwitch(7, "aaaaaa7", "missing")
			m.startPendingSessionSwitch(sessionSwitchDiscard)
			test.settle(m)

			if m.pendingSessionSwitch != nil || m.input.Value() != "preserve me" || textareaCursorRuneOffset(m.input.Value(), m.input.Line(), m.input.Column()) != 4 {
				t.Fatalf("settled failure lost payload: boundary=%#v draft=%q cursor=%d", m.pendingSessionSwitch, m.input.Value(), textareaCursorRuneOffset(m.input.Value(), m.input.Line(), m.input.Column()))
			}
			assertPendingImagesEqual(t, m.pendingImages, wantImages)
		})
	}
}

func TestDirectSuccessfulSessionRestoreClearsSyntheticComposer(t *testing.T) {
	m := newTestModel(t)
	m.input.SetValue("must not cross sessions")
	m.sessionLoading = true
	m.sessionLoadToken = 3
	updated, _ := m.Update(SessionLoadedMsg{
		LoadToken: 3, SessionID: 9,
		State:       persistedSessionState{Version: currentPersistedSessionVersion, Mode: ModeNormal},
		StateRecord: db.SessionStateRecord{SessionID: 9, Revision: 1},
	})
	m = updated.(*Model)
	if m.sessionID != 9 || m.input.Value() != "" {
		t.Fatalf("direct restore crossed synthetic draft: session=%d draft=%q", m.sessionID, m.input.Value())
	}
}

func TestPendingSessionSwitchIsNotPersisted(t *testing.T) {
	m := newTestModel(t)
	m.input.SetValue("SWITCH_DRAFT_SECRET")
	m.beginSessionSwitch(42, "aaaaa2a", "target")
	raw, err := encodeSessionState(m)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(raw, "SWITCH_DRAFT_SECRET") {
		t.Fatalf("pending session switch leaked into durable state: %s", raw)
	}
}

func TestHeldFollowUpBlocksSessionSwitchWithoutCrossingOwners(t *testing.T) {
	m, liveImages, queuedImages := heldQueueBoundaryFixture(t)
	m.sessionID = 7
	m.sessionPublicID = "aaaaaa7"
	m.activeSessionTitle = "old session"
	m.agent.AddUserMessage("old model context")
	m.entries = []ChatEntry{{Kind: "assistant", Content: "old visible context"}}

	if cmd := m.beginSessionSwitch(42, "aaaaa2a", "other session"); cmd != nil {
		t.Fatal("held follow-up started a session load")
	}
	if m.pendingSessionSwitch != nil || m.sessionLoading || m.sessionID != 7 {
		t.Fatalf("blocked switch changed boundary: pending=%#v loading=%v session=%d", m.pendingSessionSwitch, m.sessionLoading, m.sessionID)
	}
	if m.input.Value() != "active retry" || !m.queuedFollowUpHeld() || m.queuedFollowUp.Prompt != "held follow-up" {
		t.Fatalf("blocked switch changed text owners: live=%q queue=%#v", m.input.Value(), m.queuedFollowUp)
	}
	assertPendingImagesEqual(t, m.pendingImages, liveImages)
	assertPendingImagesEqual(t, m.queuedFollowUp.Images, queuedImages)
	if len(m.agent.Messages()) != 1 || len(m.entries) < 2 {
		t.Fatalf("blocked switch cleared old session: messages=%d entries=%#v", len(m.agent.Messages()), m.entries)
	}
	notice := m.entries[len(m.entries)-1].Content
	for _, want := range []string{"opening a saved session", "↑ swap", "Esc clear"} {
		if !strings.Contains(notice, want) {
			t.Fatalf("blocked switch notice omitted %q: %q", want, notice)
		}
	}
}

func TestHeldFollowUpBlocksNewConversationReset(t *testing.T) {
	for _, test := range []struct {
		name      string
		wantDraft string
		invoke    func(*Model)
	}{
		{name: "ctrl+n", wantDraft: "active retry", invoke: func(m *Model) { _, _ = m.Update(ctrlKey('n')) }},
		{name: "slash clear", wantDraft: "/clear", invoke: func(m *Model) {
			m.input.SetValue("/clear")
			_ = m.submitInput()
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			m, liveImages, queuedImages := heldQueueBoundaryFixture(t)
			m.sessionID = 7
			m.sessionPublicID = "aaaaaa7"
			m.activeSessionTitle = "old session"
			m.agent.AddUserMessage("old model context")
			m.entries = []ChatEntry{{Kind: "assistant", Content: "old visible context"}}

			test.invoke(m)
			if m.sessionID != 7 || m.activeSessionTitle != "old session" || len(m.agent.Messages()) != 1 || len(m.entries) < 2 {
				t.Fatalf("blocked reset changed old session: session=%d title=%q messages=%d entries=%#v", m.sessionID, m.activeSessionTitle, len(m.agent.Messages()), m.entries)
			}
			if m.input.Value() != test.wantDraft || !m.queuedFollowUpHeld() || m.queuedFollowUp.Prompt != "held follow-up" {
				t.Fatalf("blocked reset changed text owners: live=%q queue=%#v", m.input.Value(), m.queuedFollowUp)
			}
			assertPendingImagesEqual(t, m.pendingImages, liveImages)
			assertPendingImagesEqual(t, m.queuedFollowUp.Images, queuedImages)
			notice := m.entries[len(m.entries)-1].Content
			for _, want := range []string{"starting a new conversation", "↑ swap", "Esc clear"} {
				if !strings.Contains(notice, want) {
					t.Fatalf("blocked reset notice omitted %q: %q", want, notice)
				}
			}
		})
	}
}

func TestAuthorizedConversationReplacementClearsHeldQueue(t *testing.T) {
	m, _, _ := heldQueueBoundaryFixture(t)
	held := m.queuedFollowUp
	m.resetConversationSession()
	if m.queuedFollowUp != nil {
		t.Fatalf("authorized replacement carried held queue: %#v", m.queuedFollowUp)
	}
	for index, attachment := range held.Images {
		if attachment.Ref.Digest != "" || attachment.Ref.MIMEType != "" || attachment.Ref.Name != "" ||
			attachment.Ref.SizeBytes != 0 || attachment.Ref.Width != 0 || attachment.Ref.Height != 0 ||
			attachment.Image.SHA256 != "" || attachment.Image.Name != "" || attachment.Image.MediaType != "" ||
			attachment.Image.Size != 0 || attachment.Image.Width != 0 || attachment.Image.Height != 0 || len(attachment.Image.Data) != 0 {
			t.Fatalf("authorized replacement retained queued image %d: %#v", index, attachment)
		}
	}
}

func heldQueueBoundaryFixture(t *testing.T) (*Model, []pendingImageAttachment, []pendingImageAttachment) {
	t.Helper()
	m, _ := newImageTestModel(t)
	dir := t.TempDir()
	activePath := filepath.Join(dir, "active.png")
	queuedPath := filepath.Join(dir, "queued.png")
	writeImageAttachmentFixtureVariant(t, activePath, 41)
	writeImageAttachmentFixtureVariant(t, queuedPath, 42)
	attachImageFixture(t, m, activePath, "")
	liveImages := clonePendingImages(m.pendingImages)
	m.pendingImages = nil
	attachImageFixture(t, m, queuedPath, "")
	queuedImages := clonePendingImages(m.pendingImages)
	m.pendingImages = clonePendingImages(liveImages)
	m.input.SetValue("active retry")
	m.queuedFollowUp = &queuedFollowUp{Prompt: "held follow-up", Images: clonePendingImages(queuedImages), RecoveryHeld: true}
	return m, liveImages, queuedImages
}

func assertPendingImagesEqual(t *testing.T, got, want []pendingImageAttachment) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("pending image count = %d, want %d", len(got), len(want))
	}
	for index := range want {
		if got[index].Ref != want[index].Ref || got[index].Image.SHA256 != want[index].Image.SHA256 ||
			got[index].Image.Name != want[index].Image.Name || len(got[index].Image.Data) != len(want[index].Image.Data) {
			t.Fatalf("pending image %d = %#v, want %#v", index, got[index], want[index])
		}
	}
}
