// Package selfupdate implements ARTEX's in-page one-click update: fetching the new version from the GitHub Release
// Binary, verification, staging, and atomic swap on next startup.
//
// Overall division of labor (see start.sh / start.bat):
//
//	start script = dumb watchdog loop, only responsible for "deciding whether to relaunch based on exit code after the process exits"
//	this package = all error-prone logic (download / SHA256 verification / smoke test / swap / rollback on failure)
//
// The swap lives in Go rather than in scripts because sha256 verification and smoke tests written in sh and bat
// Two variants must be written (sha256sum / shasum / certutil), and this is precisely the link where no mistake is allowed -- swapping in a
// For an unrunnable binary, the watchdog faithfully relaunches it over and over, and the user has to log onto the machine to fix it manually.
//
// One complete update goes through three process startups:
//
//	(1) old server receives /api/update/apply -> download and verify -> stage artex.new -> exit 75
//	(2) script relaunches the old binary -> Bootstrap finds artex.new -> verify + smoke test -> swap -> exit 75
//	③ The script relaunches, now on the new version -> Bootstrap records one attempt -> clears the marker after successful startup
//
// Any failed step falls back to the old version: ② if verification fails, delete the staged file and keep running the old version; ③ if it fails to survive 3 times in a row ...
// Clearing the marker (crash if it fails to start) automatically swaps artex.old back.
package selfupdate

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// ExitRestart is the "please have the daemon relaunch me" exit code (EX_TEMPFAIL). When the startup script sees it
// it reruns immediately, not counted toward crash backoff. 0 means the user stopped normally (the script exits the loop); anything else is treated as a crash.
const ExitRestart = 75

// maxAttempts is the number of startup attempts allowed after a swap. Each startup of the new version increments the count by 1; surviving
// settleDelay clears the marker; crashing maxAttempts times in a row means the new version cannot start at all, so it automatically rolls back.
const maxAttempts = 3

// Paths are all files involved in one upgrade, all placed under the **directory containing the executable**.
// CWD is deliberately not used: when running as a service the working directory may be / or any path, and using CWD would leave staged files in
// the wrong place, directly breaking the swap logic.
type Paths struct {
	Dir     string // directory containing the executable
	Current string // currently running binary        artex      / artex.exe
	New     string // staged new version            artex.new  / artex.new.exe
	Sum     string // new version's sha256 (hex)  artex.new.sha256 / artex.new.exe.sha256
	Old     string // old version backed up before the swap      artex.old  / artex.old.exe
	Marker  string // upgrade status marker            artex.upgrade.json
}

// ResolvePaths derives all upgrade paths from the current executable.
//
// On Windows, .new/.old must also carry the .exe suffix, otherwise both the smoke test and execution after the swap will fail,
// so the suffix is stripped first and then re-appended, keeping naming symmetric on both platforms.
func ResolvePaths() (Paths, error) {
	exe, err := os.Executable()
	if err != nil {
		return Paths{}, fmt.Errorf("Locating executable: %w", err)
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}
	dir := filepath.Dir(exe)
	name := filepath.Base(exe)
	ext := filepath.Ext(name) // on Windows it is ".exe", usually empty on Unix
	stem := strings.TrimSuffix(name, ext)

	join := func(suffix string) string { return filepath.Join(dir, stem+suffix+ext) }
	return Paths{
		Dir:     dir,
		Current: exe,
		New:     join(".new"),
		Sum:     join(".new") + ".sha256",
		Old:     join(".old"),
		Marker:  filepath.Join(dir, stem+".upgrade.json"),
	}, nil
}

// marker records the progress of one swap, used to trigger automatic rollback when the new version fails to come up.
type marker struct {
	From     string `json:"from"`     // version before update
	To       string `json:"to"`       // target version
	Attempts int    `json:"attempts"` // number of start attempts since swap-in
	StagedAt int64  `json:"staged_at"`
}

func readMarker(path string) (marker, bool) {
	b, err := os.ReadFile(path)
	if err != nil {
		return marker{}, false
	}
	var m marker
	if json.Unmarshal(b, &m) != nil {
		return marker{}, false
	}
	return m, true
}

func writeMarker(path string, m marker) error {
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, b, 0o644)
}

// cleanStaged clears staged files. Successful swap, verification failure, and user cancellation all go through it to avoid leftovers.
// artex.new is retried on the next startup.
func cleanStaged(p Paths) {
	_ = os.Remove(p.New)
	_ = os.Remove(p.Sum)
}

// CompareVersions compares two version numbers, returning -1/0/1 (a<b / a==b / a>b).
// ok=false means at least one side is not a comparable version number (e.g. a local dev build "dev" or
// "0.3.7-2-gabc1234-dirty" produced by git describe); the caller should then disable one-click update,
// otherwise the in-development build would be "updated" to the official release, overwriting uncommitted changes.
func CompareVersions(a, b string) (int, bool) {
	av, aok := parseVersion(a)
	bv, bok := parseVersion(b)
	if !aok || !bok {
		return 0, false
	}
	for i := range 3 {
		if av[i] != bv[i] {
			if av[i] < bv[i] {
				return -1, true
			}
			return 1, true
		}
	}
	return 0, true
}

// parseVersion parses a version number of the form "v0.3.7" / "0.3.7" into a [3]int.
//
// Only clean three-part versions are accepted: build.sh uses git describe on non-tag builds to produce
// versions with suffixes like "0.3.7-2-gabc1234"; they must be judged incomparable rather than treated as
// 0.3.7 -- otherwise dev builds would be misjudged as "already latest" or overwritten by the official release.
func parseVersion(s string) ([3]int, bool) {
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(s, "v")
	if s == "" {
		return [3]int{}, false
	}
	parts := strings.Split(s, ".")
	if len(parts) != 3 {
		return [3]int{}, false
	}
	var out [3]int
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 {
			return [3]int{}, false
		}
		out[i] = n
	}
	return out, true
}

// InDocker reports whether the process runs inside a container. Under Docker the swap writes to the container writable layer,
// `docker compose up -d` rebuilding the container falls back to the image-bundled version -- this is expected behavior
// (the user is pulling a new image at that point), but the frontend must be able to explain this clearly.
func InDocker() bool {
	if _, err := os.Stat("/.dockerenv"); err == nil {
		return true
	}
	b, err := os.ReadFile("/proc/1/cgroup")
	if err != nil {
		return false
	}
	s := string(b)
	return strings.Contains(s, "docker") || strings.Contains(s, "containerd")
}
