package selfupdate

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"strings"
	"time"
)

// smokeEnv makes the child process launched by the smoke test skip Bootstrap directly.
//
// Strictly speaking omitting it would do no harm: the child process's os.Executable() is artex.new, so every derived
// path carries the .new prefix and never touches the real update files. But relying on such a coincidence is too fragile;
// an explicit short-circuit is obvious at a glance and also saves the child process one pointless disk probe.
const smokeEnv = "ARTEX_SELFUPDATE_SMOKE"

// Action is the instruction Bootstrap gives to main.
type Action int

const (
	// Continue: start the server as usual.
	Continue Action = iota
	// Restart: exit immediately with ExitRestart so the watchdog script relaunches it.
	Restart
)

// State describes the update status at this startup, so /api/update/check can report it truthfully to the frontend.
// "Whether the last update succeeded or was rolled back".
type State struct {
	Pending     bool   // swapped but not yet confirmed stable
	RolledBack  bool   // this startup just performed an automatic rollback
	FailedStage bool   // staged files failed verification/smoke test and have been discarded
	Detail      string // one-line explanation for the user
}

// Bootstrap runs at the very start of main and must be called before listening on any port or opening the database.
//
// Three scenarios:
//
// ① staged file artex.new exists -> verify + smoke test; on success swap and request a restart; on failure discard and keep running the old version
// ② only the marker file remains -> a swap just completed, count one attempt; roll back after enough consecutive failures
// ③ nothing exists -> start normally
func Bootstrap() (Action, State) {
	if os.Getenv(smokeEnv) != "" {
		return Continue, State{}
	}
	p, err := ResolvePaths()
	if err != nil {
		log.Printf("[update] Skipping bootstrap: %v", err)
		return Continue, State{}
	}

	if _, err := os.Stat(p.New); err == nil {
		return applyStaged(p)
	}

	m, ok := readMarker(p.Marker)
	if !ok {
		return Continue, State{}
	}
	return confirmOrRollback(p, m)
}

// applyStaged handles the "staged file exists" situation: swap in if verification passes, discard if it fails.
//
// This is the only place in the whole update chain that overwrites the executable, and the last gate -- the smoke test blocks ...
// Problems like corrupt downloads, wrong architecture, missing dynamic links. Once an unrunnable binary is let through, ...
// The watchdog script would tirelessly relaunch it over and over, while the Go code never gets a chance to run, so automatic rollback is out of the question.
func applyStaged(p Paths) (Action, State) {
	m, _ := readMarker(p.Marker)

	if err := verifyStaged(p); err != nil {
		log.Printf("[update] staged new version failed verification and was discarded, continuing with the current version: %v", err)
		cleanStaged(p)
		_ = os.Remove(p.Marker)
		return Continue, State{FailedStage: true, Detail: "new version verification failed, discarded: " + err.Error()}
	}

	if err := swap(p); err != nil {
		log.Printf("[update] Swap failed, continuing with the current version: %v", err)
		cleanStaged(p)
		_ = os.Remove(p.Marker)
		return Continue, State{FailedStage: true, Detail: "Swap failed:" + err.Error()}
	}

	// Swap succeeded. Keep the marker; the next startup (which runs the new version) confirms whether it is stable.
	m.Attempts = 0
	if m.StagedAt == 0 {
		m.StagedAt = time.Now().Unix()
	}
	if err := writeMarker(p.Marker, m); err != nil {
		log.Printf("[update] Failed to write update marker (automatic rollback capability lost): %v", err)
	}
	log.Printf("[update] Swapped to %s, exiting to restart (exit %d)", orUnknown(m.To), ExitRestart)
	return Restart, State{Pending: true}
}

// confirmOrRollback handles "post-swap-in startup": accumulates attempt counts, and swaps the old version back if over the limit.
//
// The counter only increments after the Go code is running, so it covers "executable but crashes during initialization" ...
// Failures such as (incompatible config, port already taken, DB migration blown up); "unable to exec at all" is covered by the pre-swap ...
// The smoke test blocks it; together the two make it complete.
func confirmOrRollback(p Paths, m marker) (Action, State) {
	m.Attempts++
	if m.Attempts > maxAttempts {
		if err := rollback(p); err != nil {
			// If even the rollback failed, do not restart again, otherwise it will fall into an infinite restart loop. Clear the marker,
			// Let the process start in its current state -- if it fails to start, the user can at least see the reason in the logs.
			log.Printf("[update] New version failed to start %d times in a row, and rollback failed: %v", maxAttempts, err)
			_ = os.Remove(p.Marker)
			return Continue, State{Detail: "New version failed to start and rollback failed:" + err.Error()}
		}
		log.Printf("[update] new version failed to start %d times in a row, rolled back to %s, exiting to restart (exit %d)",
			maxAttempts, orUnknown(m.From), ExitRestart)
		_ = os.Remove(p.Marker)
		return Restart, State{RolledBack: true, Detail: fmt.Sprintf("New version failed to start, rolled back to %s", orUnknown(m.From))}
	}
	if err := writeMarker(p.Marker, m); err != nil {
		log.Printf("[update] Failed to update the update marker: %v", err)
	}
	log.Printf("[update] new version starting (attempt %d/%d); the update will be confirmed once it runs stably",
		m.Attempts, maxAttempts)
	return Continue, State{Pending: true}
}

// Settle confirms the new version is running stably and clears the update marker.
//
// Called by main with a delay after the HTTP listener is up: only counts if it stays alive through this period, otherwise the marker stays in place,
// Keep accumulating attempt counts on subsequent startups until rollback is triggered.
func Settle() {
	p, err := ResolvePaths()
	if err != nil {
		return
	}
	settle(p)
}

func settle(p Paths) {
	if _, ok := readMarker(p.Marker); !ok {
		return // Not a post-update startup, nothing to do
	}
	if err := os.Remove(p.Marker); err != nil && !errors.Is(err, os.ErrNotExist) {
		log.Printf("[update] Failed to clear the update marker: %v", err)
		return
	}
	log.Printf("[update] New version is running stably, update complete (previous version retained as %s)", p.Old)
}

// SettleDelay is the runtime duration required to decide that "the new version has survived".
const SettleDelay = 30 * time.Second

// verifyStaged verifies the staged file: first compare SHA256, then actually launch it once.
func verifyStaged(p Paths) error {
	want, err := os.ReadFile(p.Sum)
	if err != nil {
		return fmt.Errorf("read checksum: %w", err)
	}
	got, err := fileSHA256(p.New)
	if err != nil {
		return fmt.Errorf("compute checksum: %w", err)
	}
	if !strings.EqualFold(strings.TrimSpace(string(want)), got) {
		return errors.New("SHA256 mismatch (download corrupted or tampered with)")
	}
	return smokeTest(p.New)
}

// smokeTest launches the new binary with -h to confirm it can actually execute on the current system.
// This blocks a whole class of problems: truncated downloads, wrong architecture (exec format error), missing dependencies, etc.
func smokeTest(bin string) error {
	if err := os.Chmod(bin, 0o755); err != nil {
		return fmt.Errorf("grant execute permission: %w", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, bin, "-h")
	cmd.Env = append(os.Environ(), smokeEnv+"=1")
	out, err := cmd.CombinedOutput()
	if ctx.Err() != nil {
		return errors.New("Smoke test timed out (new binary not responding)")
	}
	if err != nil {
		snippet := strings.TrimSpace(string(out))
		if len(snippet) > 300 {
			snippet = snippet[:300] + "..."
		}
		return fmt.Errorf("Smoke test failed: %v: %s", err, snippet)
	}
	return nil
}

// swap replaces the current binary with the staged new version.
//
// Both Unix and Windows allow renaming a running executable (what Windows forbids is deleting and
// ... overwrite, rename is not among them), so no per-platform handling is needed here, nor stopping itself first.
func swap(p Paths) error {
	// Windows rename does not overwrite an existing target, so the .old left by the previous update round must be cleared first.
	if err := os.Remove(p.Old); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("clean up old backup %s: %w", p.Old, err)
	}
	if err := os.Rename(p.Current, p.Old); err != nil {
		return fmt.Errorf("back up current version: %w", err)
	}
	if err := os.Rename(p.New, p.Current); err != nil {
		// If swap-in fails but the current version has already been moved away, it must be put back as-is, otherwise there will be no executable at next start.
		if rerr := os.Rename(p.Old, p.Current); rerr != nil {
			return fmt.Errorf("Failed to install the new version (%v), and failed to restore the current version: %w", err, rerr)
		}
		return fmt.Errorf("install the new version: %w", err)
	}
	_ = os.Remove(p.Sum)
	return nil
}

// rollback swaps back the old version saved by swap.
func rollback(p Paths) error {
	if _, err := os.Stat(p.Old); err != nil {
		return fmt.Errorf("No backup available for rollback %s: %w", p.Old, err)
	}
	// Move the unstartable new version to .failed for investigation instead of deleting it directly.
	failed := p.Current + ".failed"
	_ = os.Remove(failed)
	if err := os.Rename(p.Current, failed); err != nil {
		return fmt.Errorf("move away the failed version: %w", err)
	}
	if err := os.Rename(p.Old, p.Current); err != nil {
		return fmt.Errorf("restore old version: %w", err)
	}
	return nil
}

// Rollback is the implementation of /api/update/rollback: actively reverting to the previous version.
// Only performs the swap-in; restart is likewise left to the watchdog script (the caller then exits with ExitRestart).
func Rollback() error {
	p, err := ResolvePaths()
	if err != nil {
		return err
	}
	if _, err := os.Stat(p.Old); err != nil {
		return errors.New("no previous version to roll back to (" + p.Old + " does not exist)")
	}
	cleanStaged(p)
	if err := smokeTest(p.Old); err != nil {
		return fmt.Errorf("Previous version cannot execute, refusing rollback: %w", err)
	}
	// Swap current and backup: after rollback it can be rolled back again.
	tmp := p.Current + ".swap"
	_ = os.Remove(tmp)
	if err := os.Rename(p.Current, tmp); err != nil {
		return fmt.Errorf("move away the current version: %w", err)
	}
	if err := os.Rename(p.Old, p.Current); err != nil {
		_ = os.Rename(tmp, p.Current)
		return fmt.Errorf("install the previous version: %w", err)
	}
	if err := os.Rename(tmp, p.Old); err != nil {
		log.Printf("[update] Failed to tidy up the backup after rollback (does not affect operation): %v", err)
	}
	_ = os.Remove(p.Marker)
	return nil
}

// HasBackup reports whether a rollback-capable previous version exists, for the frontend to decide whether to show the rollback button.
func HasBackup() bool {
	p, err := ResolvePaths()
	if err != nil {
		return false
	}
	_, err = os.Stat(p.Old)
	return err == nil
}

func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func orUnknown(s string) string {
	if strings.TrimSpace(s) == "" {
		return "Unknown version"
	}
	return s
}
