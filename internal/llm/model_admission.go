package llm

import (
	"context"
	"errors"
	"fmt"
	"sync"
)

// modelAdmission keeps ordinary inference from changing local model residency
// between an expert resource snapshot and its cleanup. Expert inference is
// allowed inside the reservation, but cleanup closes that lane before waiting
// for in-flight expert requests to drain.
//
// A broadcast channel makes every wait context-aware without spawning lock
// acquisition goroutines that could outlive their caller.
type modelAdmission struct {
	mu sync.Mutex

	changed chan struct{}

	ordinary           int
	experts            int
	reservationWaiters int
	reserved           bool
	closing            bool
	autoRelease        bool
	selected           map[string]struct{}
}

func newModelAdmission() *modelAdmission {
	return &modelAdmission{changed: make(chan struct{})}
}

func (a *modelAdmission) acquireOrdinary(ctx context.Context) error {
	if a == nil {
		return errors.New("model admission is unavailable")
	}
	if ctx == nil {
		return errors.New("ordinary inference context is required")
	}
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		a.mu.Lock()
		if !a.reserved && a.experts == 0 && a.reservationWaiters == 0 {
			a.ordinary++
			a.mu.Unlock()
			return nil
		}
		changed := a.changed
		a.mu.Unlock()

		select {
		case <-changed:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

func (a *modelAdmission) releaseOrdinary() {
	if a == nil {
		return
	}
	a.mu.Lock()
	if a.ordinary > 0 {
		a.ordinary--
		a.broadcastLocked()
	}
	a.mu.Unlock()
}

func (a *modelAdmission) acquireExpert(ctx context.Context, model string) error {
	if a == nil {
		return errors.New("model admission is unavailable")
	}
	if ctx == nil {
		return errors.New("expert inference context is required")
	}
	key := modelResourceKey(model)
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		a.mu.Lock()
		if a.reserved {
			if a.closing {
				a.mu.Unlock()
				return errors.New("expert model reservation is closing")
			}
			if _, selected := a.selected[key]; !selected {
				a.mu.Unlock()
				return fmt.Errorf("expert model %q is not part of the active reservation", model)
			}
			a.experts++
			a.mu.Unlock()
			return nil
		}
		if a.ordinary == 0 && a.reservationWaiters == 0 {
			a.experts++
			a.mu.Unlock()
			return nil
		}
		changed := a.changed
		a.mu.Unlock()

		select {
		case <-changed:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

func (a *modelAdmission) releaseExpert() {
	if a == nil {
		return
	}
	a.mu.Lock()
	if a.experts > 0 {
		a.experts--
	}
	if a.experts == 0 && a.reserved && a.closing && a.autoRelease {
		a.finishReservationLocked()
	} else {
		a.broadcastLocked()
	}
	a.mu.Unlock()
}

func (a *modelAdmission) reserve(ctx context.Context, selected map[string]string) error {
	if a == nil {
		return errors.New("model admission is unavailable")
	}
	if ctx == nil {
		return errors.New("expert reservation context is required")
	}

	a.mu.Lock()
	a.reservationWaiters++
	a.broadcastLocked()
	a.mu.Unlock()

	for {
		if err := ctx.Err(); err != nil {
			a.mu.Lock()
			a.reservationWaiters--
			a.broadcastLocked()
			a.mu.Unlock()
			return err
		}
		a.mu.Lock()
		if !a.reserved && a.ordinary == 0 && a.experts == 0 {
			a.reservationWaiters--
			a.reserved = true
			a.closing = false
			a.autoRelease = false
			a.selected = make(map[string]struct{}, len(selected))
			for key := range selected {
				a.selected[key] = struct{}{}
			}
			a.broadcastLocked()
			a.mu.Unlock()
			return nil
		}
		changed := a.changed
		a.mu.Unlock()

		select {
		case <-changed:
		case <-ctx.Done():
			a.mu.Lock()
			a.reservationWaiters--
			a.broadcastLocked()
			a.mu.Unlock()
			return ctx.Err()
		}
	}
}

// waitForExpertDrain prevents new expert dispatches and waits for those
// already in flight. On cancellation, the reservation remains in place until
// the final expert exits, so ordinary inference cannot invalidate resources
// that are still in use after a cleanup deadline.
func (a *modelAdmission) waitForExpertDrain(ctx context.Context) error {
	if a == nil {
		return errors.New("model admission is unavailable")
	}
	if ctx == nil {
		return errors.New("expert cleanup context is required")
	}
	for {
		a.mu.Lock()
		if !a.reserved {
			a.mu.Unlock()
			return errors.New("expert reservation is not active")
		}
		a.closing = true
		if err := ctx.Err(); err != nil {
			a.autoRelease = true
			a.broadcastLocked()
			a.mu.Unlock()
			return err
		}
		if a.experts == 0 {
			a.mu.Unlock()
			return nil
		}
		changed := a.changed
		a.mu.Unlock()

		select {
		case <-changed:
		case <-ctx.Done():
			a.mu.Lock()
			a.closing = true
			a.autoRelease = true
			a.broadcastLocked()
			a.mu.Unlock()
			return ctx.Err()
		}
	}
}

func (a *modelAdmission) finishReservation() {
	if a == nil {
		return
	}
	a.mu.Lock()
	if a.reserved {
		if a.experts == 0 {
			a.finishReservationLocked()
		} else {
			a.closing = true
			a.autoRelease = true
			a.broadcastLocked()
		}
	}
	a.mu.Unlock()
}

func (a *modelAdmission) finishReservationLocked() {
	a.reserved = false
	a.closing = false
	a.autoRelease = false
	a.selected = nil
	a.broadcastLocked()
}

func (a *modelAdmission) broadcastLocked() {
	close(a.changed)
	a.changed = make(chan struct{})
}
