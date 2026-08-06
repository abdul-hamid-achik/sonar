//go:build !darwin

package speech

import "os/exec"

// Linux and the rest reach a synthesizer through the same subprocess shape, but
// none is part of a base system the way macOS ships `say`. Reporting false is
// the honest answer until one is chosen and verified on a real host: a stub
// that claimed a driver would let a caller enable voice and hear nothing, which
// is worse than being told the host has none.
//
// espeak-ng and Piper are the candidates. Piper's upstream archived in October
// 2025 and its active fork is GPL-3, which is a licensing decision for a
// distributed binary rather than an implementation detail — so neither is
// wired here until that call is made.
func Available() bool { return false }

func synthesizerCommand(string, int) (string, []string, error) {
	return "", nil, ErrUnavailable
}

func signalSynthesizer(*exec.Cmd) {}
