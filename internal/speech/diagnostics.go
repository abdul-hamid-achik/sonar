package speech

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// Diagnostics: what a user needs to see when voice does not work.
//
// This file exists because of a specific afternoon. Dictation failed on a
// machine where every part looked installed — ffmpeg present, whisper-cli
// present, microphone working — and the missing piece was a model file, which
// nothing surfaced. Homebrew's whisper-cpp ships one 575 KB test fixture and no
// real model, so "brew install whisper-cpp" completes and dictation still
// cannot run. Diagnosing that took listing audio devices, recording a sample,
// and reading the source.
//
// Every line below is one thing that must be true, with the path that satisfies
// it or the command that would. A capability that can fail in four places has
// to be able to say which one.

// Diagnostic is one component of the voice pipeline.
type Diagnostic struct {
	// Component is what this line is about: "Recorder", "Model", …
	Component string
	// Detail is the resolved path or value when present.
	Detail string
	// Fix is the command that would satisfy it, empty when nothing is wrong.
	Fix string
	// OK reports whether this component is usable right now.
	OK bool
}

// Diagnose reports every part of the voice pipeline and what is missing.
//
// configuredModel and voices come from configuration so the report describes
// THIS session rather than a default one — a wrong model path in a config file
// is exactly the kind of thing this has to be able to show.
func Diagnose(configuredModel string, voices map[string]string) []Diagnostic {
	report := make([]Diagnostic, 0, 5)

	recorder, err := exec.LookPath("ffmpeg")
	report = append(report, Diagnostic{
		Component: "Recorder", Detail: recorder, OK: err == nil,
		Fix: fixWhen(err != nil, "brew install ffmpeg"),
	})

	transcriber, err := exec.LookPath(whisperBinary)
	report = append(report, Diagnostic{
		Component: "Transcriber", Detail: transcriber, OK: err == nil,
		Fix: fixWhen(err != nil, "brew install whisper-cpp"),
	})

	model := LocalTranscriber{Model: configuredModel}.resolveModel()
	modelDetail := model
	if model == "" && strings.TrimSpace(configuredModel) != "" {
		// Naming the path that was asked for and not found is the whole point:
		// "no model" beside a configured path reads as a bug in the harness.
		modelDetail = "configured but missing: " + configuredModel
	}
	report = append(report, Diagnostic{
		Component: "Model", Detail: modelDetail, OK: model != "",
		Fix: fixWhen(model == "", ModelDownloadHint()),
	})

	report = append(report, Diagnostic{
		Component: "Synthesizer", Detail: synthesizerDetail(), OK: Available(),
		Fix: fixWhen(!Available(), "no supported synthesizer on this platform"),
	})

	report = append(report, Diagnostic{
		Component: "Voices", Detail: voiceAssignments(voices), OK: true,
	})
	return report
}

func fixWhen(broken bool, fix string) string {
	if broken {
		return fix
	}
	return ""
}

func synthesizerDetail() string {
	if !Available() {
		return ""
	}
	return synthesizerName()
}

// voiceAssignments shows what each language would actually be read by, which is
// not the same as what the configuration says: an unconfigured language still
// resolves to something, and a configured voice that the host does not have is
// worth seeing next to one that it does.
func voiceAssignments(voices map[string]string) string {
	languages := []string{"es", "en"}
	for language := range voices {
		if language != "es" && language != "en" {
			languages = append(languages, language)
		}
	}
	assignments := make([]string, 0, len(languages))
	for _, language := range languages {
		voice := VoiceForLanguage(language, voices[language])
		if voice == "" {
			voice = "system default"
		} else if !voiceInstalled(voice) {
			voice += " (NOT INSTALLED)"
		}
		assignments = append(assignments, language+" → "+voice)
	}
	return strings.Join(assignments, ", ")
}

func voiceInstalled(name string) bool {
	for _, voice := range hostVoices() {
		if voice.name == name {
			return true
		}
	}
	return false
}

// InstalledModels lists what the host has, so a report can say "none" with
// authority rather than leaving the reader to guess where it looked.
func InstalledModels() []string {
	var models []string
	for _, root := range whisperModelRoots() {
		entries, err := os.ReadDir(root)
		if err != nil {
			continue
		}
		for _, entry := range entries {
			if !entry.IsDir() && usableWhisperModel(entry.Name()) {
				models = append(models, filepath.Join(root, entry.Name()))
			}
		}
	}
	return models
}
