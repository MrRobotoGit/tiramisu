// Package streampolicy holds the pure rules that tell a player's reads from probes on a virtual
// .mkv: header and tail probe zones, seeks, streaming proof. No state: callers pass what they saw.
package streampolicy

import (
	"time"

	"tiramisu/internal/warmup"
)

// HeaderProbeZone is the start of the file that players and Samba re-read for container headers.
const HeaderProbeZone = 2 * 1024 * 1024

// HeaderChainLimit bounds how far a header read may continue sequentially and still count as one:
// the SSD warmup head (64MB by default). Plex's head reads ran past 16MB on every resume; inside
// the head the SSD serves the reads and the pump skips that zone anyway, so a player that really
// starts over loses nothing waiting until the head ends to reposition the pump.
func HeaderChainLimit() int64 {
	if warmup.FileSize > 16*1024*1024 {
		return warmup.FileSize
	}
	return 16 * 1024 * 1024
}

// IsHeaderRead reports a header read by a handle already positioned past it, or a read continuing
// one sequentially up to HeaderChainLimit. It is not a seek (82% of seek interrupts in production
// landed here) and must not move the handle's offset, which the pump follows and Release persists
// as the resume position.
func IsHeaderRead(prevOff, off, chainEnd, tolerance int64) bool {
	if prevOff < HeaderProbeZone {
		return false
	}
	if off < HeaderProbeZone {
		return true
	}
	limit := HeaderChainLimit()
	if off < limit && prevOff >= limit {
		// Plex parses the head out of order (2MB, then 28MB): any read inside the SSD head by a
		// handle positioned past it is still header parsing, sequential or not.
		return true
	}
	return chainEnd > 0 && off < limit && abs(off-chainEnd) <= tolerance
}

// ShouldInterruptForSeek returns true for genuine seeks beyond budget.
// Ignores: new handles (prevOff<=0), Samba header probes (off==0), sequential reads.
func ShouldInterruptForSeek(prevOff, off, budget int64) bool {
	if prevOff <= 0 || off == 0 || (off < HeaderProbeZone && prevOff >= HeaderProbeZone) {
		return false
	}
	return off > prevOff+budget || prevOff > off+budget
}

// TailProbeZoneSize returns the adaptive tail-probe zone size for a file of the given size:
// 5%, clamped to [64MB, 2GB]. Shared by the LastSeekOff
// write-site guard, and the ResumeAnchor seed-site guard - a single scanner probe reading MKV
// Cues near the end of a file must never look like a real playback position to any of them.
// promoteIfStreaming marks a handle primary once it has proven it is the player: several
// consecutive sequential reads of streaming size. The pump syncs to primary handles only, and
// during steady playback the real reader is usually an attached (secondary) handle - the
// nominal primary can be an idle probe holding the first reference, so the pump follows the
// wrong offset and the player is served by FetchBlock (Soulm8te, 2026-08-19: 29 attaches, all
// primary=false, only a seek recovered it by forcing a refCount 0->1 reconnect).
//
// Promotion is monotonic and never recomputed per tick, unlike the freshness heuristic reverted
// in 6ae20c9: the pump takes the MAX of the primaries' offsets, so a promotion can only raise
// that value, never drag it backwards - which is what produced the jump/re-anchor thrashing.
// Tail-zone reads never count: an EOF probe must not be able to pull the pump to the end.
// AdvanceProvesStreaming reports whether one read looks like the player moving forward through
// the file: streaming-sized, sequential from the previous read, and outside the tail probe zone.
// The size floor is a quarter of StreamingThreshold so Synology's 64KB CIFS reads still count.
func AdvanceProvesStreaming(prevOff, off, readLen, size, tolerance, streamingThreshold int64) bool {
	if prevOff < 0 || off <= prevOff || readLen <= 0 {
		return false
	}
	if off-prevOff > readLen+tolerance {
		return false
	}
	if readLen < streamingThreshold/4 {
		return false
	}
	return off < size-TailProbeZoneSize(size)
}

const PrimaryProofReads = 3 // consecutive sequential reads before a handle drives the pump

func TailProbeZoneSize(size int64) int64 {
	zone := size / 20 // 5%
	if zone < 64*1024*1024 {
		zone = 64 * 1024 * 1024
	}
	if zone > 2*1024*1024*1024 {
		zone = 2 * 1024 * 1024 * 1024
	}
	return zone
}

// ShouldServeTailFromSSD reports whether a read at offset off (file size size) can be
// served from the tail warmup file (the last tailWarmupSize bytes). Pure gate for the
// Read() path: ReadTail itself is range-safe (returns 0 outside coverage), so this
// only decides whether the SSD tail is even a candidate. It deliberately does not
// depend on handle state - any first read with off >= warmup.FileSize transitions
// WARMUP→STREAMING (seek/resume detection) before a state-gated tail check could run.
// A resume in the last 16MB was therefore served by network FetchBlock (~16s TTFF on
// a 36.6GB remux, 2026-08-06) although the tail file was complete on SSD.
func ShouldServeTailFromSSD(off, size, tailWarmupSize int64) bool {
	if size <= 0 || tailWarmupSize <= 0 {
		return false
	}
	tailStart := size - tailWarmupSize
	if tailStart < 0 {
		tailStart = 0
	}
	return off >= tailStart
}

// ShouldRecordResumeOff reports whether a read may update the persisted resume position.
// PlaybackState is per-path, so before the single-primary invariant any handle touching the file
// could write here, and the monotonic guard it used to carry made the value a high-water mark
// that never came back down: a probe reading at 6431MB became the "resume position" for a player
// sitting at 1300MB, and the next session anchored the pump 5GB off (Lucky S01E04, 2026-08-22).
// Gating on the primary handle and dropping the monotonic guard records where the player
// actually is. The 2MB floor and the tail-probe ceiling still exclude header and Cues reads.
func ShouldRecordResumeOff(healthy, isPrimary bool, off, size int64) bool {
	if !healthy || !isPrimary || size <= 0 {
		return false
	}
	return off > 2*1024*1024 && off < size-TailProbeZoneSize(size)
}

// ResumeAnchorFromPersisted applies the tail-probe guard to a persisted resume
// position. Returns (anchor, zeroed): anchor is a valid pump start (0 when unusable),
// zeroed=true when the value lies in the tail-probe zone (or beyond EOF) and must be
// cleared - a single scanner probe reading MKV Cues near the end of a file must never
// look like a real playback position. Shared by the registry seeding and the DB
// fallback so both paths apply identical semantics.
func ResumeAnchorFromPersisted(lastSeekOff, size int64) (anchor int64, zeroed bool) {
	if lastSeekOff > 2*1024*1024 && lastSeekOff < size-TailProbeZoneSize(size) {
		return lastSeekOff, false
	}
	if lastSeekOff >= size-TailProbeZoneSize(size) {
		return 0, true
	}
	return 0, false
}

// ShouldInterruptPump decides whether a read should interrupt the pump. Reads served
// from the SSD tail warmup (ShouldServeTailFromSSD) never touch the pump - Interrupt()
// + ResetShield() would only stall a pump the read does not need. The old isTailProbe
// gate was unreachable dead state (any read in the tail zone first transitions
// WARMUP→STREAMING), so a live scrub into the last 16MB interrupted the pump even
// though the SSD served the read instantly. Leaving the SSD tail is no seek either: Plex reads
// the Cues there and then the head, which interrupted the pump and reset the global shield.
func ShouldInterruptPump(prevOff, off, budget int64, tailServed, prevTailServed bool) bool {
	if tailServed || prevTailServed {
		return false
	}
	return ShouldInterruptForSeek(prevOff, off, budget)
}

// PrimaryProofFor returns how many consecutive sequential reads a challenger must show before it
// may take primary. One landing within the read-ahead budget of the incumbent is the player moving
// normally (its own reopen lands right where it left off) and needs the base proof. One landing
// further away would drag the pump across the file, so it must sustain the streak far longer than
// a short-lived probe can: Lucky S01E04, 2026-08-22 saw handles attach at 5578/6069/6430MB, earn
// primary within 2-3s and be released immediately, each yanking the pump away from a player
// reading 2GB behind. A genuine seek by an open handle promotes through Read()'s seek path
// instead, so it never pays this. The extended proof is also the escape hatch that stops a wrong
// incumbent from wedging primary forever.
func PrimaryProofFor(challengerOff, incumbentOff, budget int64) int32 {
	if incumbentOff <= 0 || budget <= 0 {
		return PrimaryProofReads // no incumbent to displace
	}
	d := challengerOff - incumbentOff
	if d < 0 {
		d = -d
	}
	if d <= budget {
		return PrimaryProofReads
	}
	return PrimaryProofReads * 4
}

// PumpStrandedAhead reports whether the pump sits so far past the player that it can no longer
// serve it. nativePumpChunk's hard limit already parks a healthy pump at ~budget, so only a lead
// beyond 2x budget means a bad anchor rather than throttling. playerOff == 0 means no primary
// handle is registered right now (handle handoff), and must never drag the pump back to 0.
func PumpStrandedAhead(offset, playerOff, budget int64) bool {
	if playerOff <= 0 || budget <= 0 {
		return false
	}
	return offset > playerOff+2*budget
}

// ShouldFireStarvationRevival decides whether the pump loop must self-revive: the
// pump is confirmed playback, its lead over the player is thin (< 16MB), and it is
// advancing below 75% of the player's consumption rate for the observation window.
// This replicates what a manual player seek does via V286b (Interrupt + ResetShield +
// fresh peer selection) without moving the player. Rates are bytes/second averaged
// over the 10s window by the caller. The lead gate protects the healthy hard-limit
// idle (lead = ReadAheadBudget): a pump with a thick lead is deliberately stopped,
// not starved (coincidentally the same 16MB as warmup.TailWarmupSize, but a
// different purpose: the watchdog lead, not the SSD tail coverage). Cooldown
// prevents revival storms.
func ShouldFireStarvationRevival(confirmed bool, leadBytes int64, pumpRate, playerRate float64, cooldownAge time.Duration) bool {
	if !confirmed || cooldownAge < 60*time.Second {
		return false
	}
	if playerRate <= 0 || leadBytes >= 16*1024*1024 {
		return false
	}
	return pumpRate < playerRate*0.75
}

func abs(n int64) int64 {
	if n < 0 {
		return -n
	}
	return n
}
