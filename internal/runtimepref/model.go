// Package runtimepref owns small, user-scoped runtime preferences that must
// survive process restarts without becoming part of repository or session
// state.
package runtimepref

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/abdul-hamid-achik/sonar/internal/safeio"
)

const (
	preferenceVersion         = 1
	maxPreferenceFileBytes    = 1024
	maxPreferredModelBytes    = 256
	maxPreferredProviderBytes = 128
	maxPreferredThemeBytes    = 64
	preferenceReadTimeout     = 2 * time.Second
	preferenceLockTimeout     = 2 * time.Second
	defaultPreferencesFile    = "runtime-preferences.json"
)

type document struct {
	Version        int    `json:"version"`
	ManualModel    string `json:"manual_model,omitempty"`
	ManualProvider string `json:"manual_provider,omitempty"`
	Theme          string `json:"theme,omitempty"`
}

// Store persists one bounded manual model selection in an owner-private file.
// It deliberately contains no workspace or session identity.
type Store struct {
	mu     sync.RWMutex
	path   string
	reader *safeio.Reader
}

// DefaultPath returns the user-scoped preference path used by the CLI.
func DefaultPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("home dir: %w", err)
	}
	return filepath.Join(home, ".config", "sonar", defaultPreferencesFile), nil
}

// NewStore creates a preference store at path. The file is opened lazily.
func NewStore(path string) *Store {
	return &Store{path: filepath.Clean(path), reader: safeio.NewReader()}
}

// LoadManualModel reads the saved manual selection. The boolean is false when
// no selection is saved. Unknown versions and malformed documents fail closed.
func (s *Store) LoadManualModel() (string, bool, error) {
	if s == nil || strings.TrimSpace(s.path) == "" || s.reader == nil {
		return "", false, fmt.Errorf("model preference store is not initialized")
	}
	s.mu.RLock()
	defer s.mu.RUnlock()

	doc, err := s.loadLocked()
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", false, nil
		}
		return "", false, err
	}
	if doc.ManualModel == "" {
		return "", false, nil
	}
	return doc.ManualModel, true, nil
}

// SetManualModel atomically replaces the saved manual model selection while
// preserving any saved provider preference.
func (s *Store) SetManualModel(model string) error {
	model, err := validateModel(model)
	if err != nil {
		return err
	}
	return s.update(func(doc *document) error {
		doc.ManualModel = model
		return nil
	})
}

// ClearManualModel durably removes any saved manual model selection without
// clearing an unrelated provider preference.
func (s *Store) ClearManualModel() error {
	return s.update(func(doc *document) error {
		doc.ManualModel = ""
		return nil
	})
}

// LoadManualProvider reads the saved inference provider profile name.
func (s *Store) LoadManualProvider() (string, bool, error) {
	if s == nil || strings.TrimSpace(s.path) == "" || s.reader == nil {
		return "", false, fmt.Errorf("provider preference store is not initialized")
	}
	s.mu.RLock()
	defer s.mu.RUnlock()

	doc, err := s.loadLocked()
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", false, nil
		}
		return "", false, err
	}
	if doc.ManualProvider == "" {
		return "", false, nil
	}
	return doc.ManualProvider, true, nil
}

// SetManualProvider atomically stores the selected provider profile name.
func (s *Store) SetManualProvider(name string) error {
	name, err := validateProvider(name)
	if err != nil {
		return err
	}
	return s.update(func(doc *document) error {
		doc.ManualProvider = name
		return nil
	})
}

// LoadTheme reads the saved theme selection. The boolean is false when the
// user has never chosen one, which is distinct from having chosen the default.
func (s *Store) LoadTheme() (string, bool, error) {
	if s == nil || strings.TrimSpace(s.path) == "" || s.reader == nil {
		return "", false, fmt.Errorf("theme preference store is not initialized")
	}
	s.mu.RLock()
	defer s.mu.RUnlock()

	doc, err := s.loadLocked()
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", false, nil
		}
		return "", false, err
	}
	if doc.Theme == "" {
		return "", false, nil
	}
	return doc.Theme, true, nil
}

// SetTheme atomically stores the selected theme identifier. The value is a
// registry key, not free text, so the same bounded charset the provider name
// uses is more than sufficient.
func (s *Store) SetTheme(id string) error {
	id, err := validateTheme(id)
	if err != nil {
		return err
	}
	return s.update(func(doc *document) error {
		doc.Theme = id
		return nil
	})
}

// ClearTheme removes the saved theme preference, returning the UI to whatever
// the configuration file selects.
func (s *Store) ClearTheme() error {
	return s.update(func(doc *document) error {
		doc.Theme = ""
		return nil
	})
}

// ClearManualProvider removes the saved provider preference.
func (s *Store) ClearManualProvider() error {
	return s.update(func(doc *document) error {
		doc.ManualProvider = ""
		return nil
	})
}

func (s *Store) update(mutate func(*document) error) error {
	if s == nil || strings.TrimSpace(s.path) == "" {
		return fmt.Errorf("preference store is not initialized")
	}
	if mutate == nil {
		return fmt.Errorf("preference update is nil")
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	dir := filepath.Dir(s.path)
	if err := safeio.ValidatePublishPath(s.path); err != nil {
		return fmt.Errorf("validate preference path: %w", err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create preference dir: %w", err)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return fmt.Errorf("secure preference dir: %w", err)
	}
	if err := safeio.ValidatePublishPath(s.path); err != nil {
		return fmt.Errorf("revalidate preference path: %w", err)
	}

	return safeio.WithExclusiveFileLock(s.path+".lock", preferenceLockTimeout, func() error {
		doc, err := s.loadLocked()
		if err != nil {
			if !errors.Is(err, os.ErrNotExist) {
				return err
			}
			doc = document{Version: preferenceVersion}
		}
		doc.Version = preferenceVersion
		if err := mutate(&doc); err != nil {
			return err
		}
		return s.persistLocked(doc)
	})
}

func (s *Store) loadLocked() (document, error) {
	var doc document
	data, err := s.reader.ReadPrivateRegularFileNoFollow(s.path, maxPreferenceFileBytes, preferenceReadTimeout)
	if err != nil {
		return doc, fmt.Errorf("read model preference: %w", err)
	}
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&doc); err != nil {
		return document{}, fmt.Errorf("parse model preference: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			err = fmt.Errorf("trailing JSON value")
		}
		return document{}, fmt.Errorf("parse model preference: %w", err)
	}
	if doc.Version != preferenceVersion {
		return document{}, fmt.Errorf("unsupported model preference version %d", doc.Version)
	}
	if doc.ManualModel != "" {
		model, err := validateModel(doc.ManualModel)
		if err != nil {
			return document{}, fmt.Errorf("invalid saved model preference: %w", err)
		}
		doc.ManualModel = model
	}
	if doc.ManualProvider != "" {
		provider, err := validateProvider(doc.ManualProvider)
		if err != nil {
			return document{}, fmt.Errorf("invalid saved provider preference: %w", err)
		}
		doc.ManualProvider = provider
	}
	return doc, nil
}

func (s *Store) persistLocked(doc document) error {
	data, err := json.Marshal(doc)
	if err != nil {
		return fmt.Errorf("marshal model preference: %w", err)
	}
	data = append(data, '\n')
	if len(data) > maxPreferenceFileBytes {
		return fmt.Errorf("serialized model preference exceeds %d bytes", maxPreferenceFileBytes)
	}

	dir := filepath.Dir(s.path)
	tmp, err := os.CreateTemp(dir, ".runtime-preferences-*.tmp")
	if err != nil {
		return fmt.Errorf("create preference temp file: %w", err)
	}
	tmpName := tmp.Name()
	defer func() {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
	}()
	if err := tmp.Chmod(0o600); err != nil {
		return fmt.Errorf("secure preference temp file: %w", err)
	}
	if _, err := tmp.Write(data); err != nil {
		return fmt.Errorf("write model preference: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		return fmt.Errorf("sync model preference: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close model preference: %w", err)
	}
	if err := safeio.ValidatePublishPath(s.path); err != nil {
		return fmt.Errorf("revalidate preference publish path: %w", err)
	}
	if err := os.Rename(tmpName, s.path); err != nil {
		return fmt.Errorf("commit model preference: %w", err)
	}
	if directory, err := os.Open(dir); err == nil {
		_ = directory.Sync()
		_ = directory.Close()
	}
	return nil
}

func validateModel(model string) (string, error) {
	model = strings.TrimSpace(model)
	if model == "" {
		return "", fmt.Errorf("model preference is empty")
	}
	if !utf8.ValidString(model) {
		return "", fmt.Errorf("model preference is not valid UTF-8")
	}
	if len(model) > maxPreferredModelBytes {
		return "", fmt.Errorf("model preference exceeds %d bytes", maxPreferredModelBytes)
	}
	for _, r := range model {
		if unicode.IsControl(r) || unicode.In(r, unicode.Bidi_Control) {
			return "", fmt.Errorf("model preference contains control characters")
		}
	}
	return model, nil
}

func validateProvider(name string) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return "", fmt.Errorf("provider preference is empty")
	}
	if !utf8.ValidString(name) {
		return "", fmt.Errorf("provider preference is not valid UTF-8")
	}
	if len(name) > maxPreferredProviderBytes {
		return "", fmt.Errorf("provider preference exceeds %d bytes", maxPreferredProviderBytes)
	}
	for i, r := range name {
		switch {
		case r >= 'A' && r <= 'Z', r >= 'a' && r <= 'z', r == '_', r == '-':
			continue
		case r >= '0' && r <= '9':
			if i == 0 {
				return "", fmt.Errorf("provider preference must not start with a digit")
			}
		default:
			if unicode.IsControl(r) || unicode.In(r, unicode.Bidi_Control) {
				return "", fmt.Errorf("provider preference contains control characters")
			}
			return "", fmt.Errorf("provider preference contains invalid characters")
		}
	}
	return name, nil
}

func validateTheme(id string) (string, error) {
	id = strings.ToLower(strings.TrimSpace(id))
	if id == "" {
		return "", fmt.Errorf("theme preference is empty")
	}
	if !utf8.ValidString(id) {
		return "", fmt.Errorf("theme preference is not valid UTF-8")
	}
	if len(id) > maxPreferredThemeBytes {
		return "", fmt.Errorf("theme preference exceeds %d bytes", maxPreferredThemeBytes)
	}
	for _, r := range id {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-':
			continue
		default:
			return "", fmt.Errorf("theme preference contains invalid characters")
		}
	}
	return id, nil
}
