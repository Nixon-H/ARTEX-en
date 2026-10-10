package agent

import (
	"log"
	"path/filepath"

	"github.com/Autumn-27/norma/agentcore"
	"github.com/Autumn-27/norma/noaadapter"
)

// noaWarn returns a diagnostics sink tagging non-fatal noa messages with the
// session, routed through the package logger (agents have no per-instance one).
func noaWarn(session string) func(string) {
	return func(msg string) { log.Printf("[noa] %s: %s", session, msg) }
}

// noa is the "model-driven context compaction" mechanism introduced in norma v0.4.0, offered as a platform experimental feature that users ... (enable in ...)
// Toggled in system settings. It is mutually exclusive with built-in compaction: noaadapter.Enable is the sole entry point; once attached... [fragment, truncated at tail]
// Context takeover handler (Compactor), the Compress tool, and the three resident prompt sections; not calling Enable means disabled
// (built-in compaction works as usual). The switch is resolved by the noaEnabledFn injected by each agent, read on every run
// ... once, so switching only affects runs started afterwards -- no agent rebuild needed.

// enableNoa wires noa into opts when the parser reports it enabled. archiveRoot is the persistence base directory for the compaction source text
// (takes the global workDir; all agents uniformly land under <workDir>/noa, not scattered across task/intent directories), sessionID
// ... names the archive subdirectory under it (globally unique, so no clash within one base dir).
//
// noa is experimental: integration failure must not interrupt real tasks. On error, report via onWarn and fall back to built-in compaction.
// On successful enable, clear opts.Compaction to avoid agentcore warning about "two context managers set at the same time".
func enableNoa(opts *agentcore.Options, enabled func() bool, archiveRoot, sessionID string, onWarn func(string)) {
	if enabled == nil || !enabled() {
		return
	}
	if opts.OnWarn == nil {
		opts.OnWarn = onWarn
	}
	if err := noaadapter.Enable(opts, noaadapter.Options{
		ArchiveBaseDir: filepath.Join(archiveRoot, "noa"),
		SessionID:      sessionID,
		OnWarn:         onWarn,
	}); err != nil {
		if onWarn != nil {
			onWarn("noa compaction failed to enable; falling back to built-in compaction:" + err.Error())
		}
		return
	}
	// Compactor overrides Compaction, but agentcore warns every time when both coexist; clear it explicitly.
	opts.Compaction = nil
}
