package tmux

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	fleet "github.com/godx-jp/colab-fleet"
)

// ProcessIdentity is one OS process, resolved live and never cached across a
// call (colab-fleet #116). It is the (pid, start-time) analogue of the
// (Pane, Created) pairing docs/adr/97-identity-in-record.md already uses for
// the SESSION-NAME axis, and for the identical reason: a bare PID is a
// number the kernel recycles, so PID alone cannot tell "the process this
// driver resolved a moment ago" apart from "an unrelated process that has
// since been handed the same number". StartedAt is what makes the pair
// specific to one process's one run rather than to a slot the kernel reuses.
//
// # Why this lives here, and why it stops short of a socket
//
// #116 is the prerequisite #119 (delivery over a session's own inbox) is
// blocked on, filed after a relay resolved a session name to a process by
// walking the process table and got a DIFFERENT session's inbox — a
// misroute that reported success. #116's own ask is narrower than #119's:
// an authoritative resolution, a pre-write verification, a refusal instead
// of a best guess, and a coverage check — not the inbox connection itself,
// which needs #117's still-open human ruling on credentials first. This
// file is exactly that narrower thing: it answers "which OS process is
// genuinely this session's runtime, right now, and is that still true a
// moment later" and stops there.
type ProcessIdentity struct {
	PID       int
	StartedAt time.Time
}

// ErrProcessIdentityUnresolved is returned by ResolveProcessIdentity and
// VerifyProcessIdentity whenever a process identity cannot be established
// with confidence — never in place of a best guess. Every wrapped instance
// gives a caller `errors.Is(err, ErrProcessIdentityUnresolved)` as a stable
// way to branch, the same shape ErrNotFound already gives this package's
// other callers.
var ErrProcessIdentityUnresolved = errors.New(
	"tmux: could not establish an authoritative process identity for this session")

// counterProcessIdentityUnresolved is #116's own coverage signal, the same
// idiom counterIdentityContested already established for a different axis
// of identity (tmux.go, colab-fleet #97): a rate, not a one-off log line,
// because a coverage gap that appears once and is never checked again is
// indistinguishable from one that never happened. Incremented from both
// ResolveProcessIdentity (a single session a caller asked about) and
// ProcessIdentityCoverage (a sweep over everything this driver enumerates)
// — two call sites, one counter, because both report the same fact: a
// session this driver tracks had no resolvable process identity.
const counterProcessIdentityUnresolved = "identity.process_unresolved"

// psStartTimeLayout matches `ps -o lstart=`'s fixed-width, unquoted output
// on this driver's own platform (e.g. "Mon Aug 26 10:15:23 2026") — the
// layout ps has used for this field since long before this driver existed,
// not something this code invents or needs to configure.
const psStartTimeLayout = "Mon Jan _2 15:04:05 2006"

// ResolveProcessIdentity answers colab-fleet #116's first requirement:
// resolution from an authoritative source at send time, never a cached or
// inferred map. Every call re-enumerates the multiplexer AND re-queries the
// OS process table — nothing here is read from this driver's own memory of
// an earlier call, because the PID that memory would hold is exactly the
// stale, since-recycled value #116's own report measured going wrong.
//
// Refuses (wrapping ErrProcessIdentityUnresolved) rather than guessing
// whenever the session cannot be found, its pane is dead, the multiplexer
// reports no usable PID, or the OS no longer has a process at that PID by
// the time this call gets there — never falls through to a zero value or a
// stale one, which is #116's third requirement, not just its first.
func (d *Driver) ResolveProcessIdentity(ctx context.Context, ref fleet.SessionRef) (ProcessIdentity, error) {
	ctx, cancel := d.bounded(ctx)
	defer cancel()

	rows, _, err := d.enumerate(ctx)
	if err != nil {
		return ProcessIdentity{}, fmt.Errorf("resolving process identity for %q: %w", ref.ID, err)
	}
	for _, r := range rows {
		if r.session != ref.ID {
			continue
		}
		if r.dead {
			return ProcessIdentity{}, fmt.Errorf("%w: %q: pane is dead", ErrProcessIdentityUnresolved, ref.ID)
		}
		if r.pid <= 0 {
			d.counters.incr(counterProcessIdentityUnresolved)
			return ProcessIdentity{}, fmt.Errorf("%w: %q: multiplexer reported no usable pid",
				ErrProcessIdentityUnresolved, ref.ID)
		}
		startedAt, err := d.processStartedAt(ctx, r.pid)
		if err != nil {
			d.counters.incr(counterProcessIdentityUnresolved)
			return ProcessIdentity{}, fmt.Errorf("%w: %q: %v", ErrProcessIdentityUnresolved, ref.ID, err)
		}
		return ProcessIdentity{PID: r.pid, StartedAt: startedAt}, nil
	}
	return ProcessIdentity{}, fmt.Errorf("%w: %q: no such session", ErrProcessIdentityUnresolved, ref.ID)
}

// VerifyProcessIdentity answers colab-fleet #116's second requirement: a
// verification step BEFORE the write, not after. want is a ProcessIdentity
// ResolveProcessIdentity already returned; this re-queries the OS process
// table for that exact PID right now and confirms the process still
// starting-timestamp-matches what was resolved earlier.
//
// This closes the gap ResolveProcessIdentity alone cannot: time passes
// between resolving an identity and using it, and in that gap the resolved
// process can exit and the kernel can hand its PID to something else
// entirely. A caller that resolved once and later acted on the same value
// without calling this again would be back to trusting a cached mapping —
// exactly what #116 was filed to stop.
//
// Returns nil only when the same process, not merely the same PID, is still
// there. Every other outcome — the PID no longer running, or running as a
// process with a different start time (recycled) — wraps
// ErrProcessIdentityUnresolved rather than reporting a partial match.
func (d *Driver) VerifyProcessIdentity(ctx context.Context, want ProcessIdentity) error {
	ctx, cancel := d.bounded(ctx)
	defer cancel()

	if want.PID <= 0 {
		return fmt.Errorf("%w: no pid to verify", ErrProcessIdentityUnresolved)
	}
	now, err := d.processStartedAt(ctx, want.PID)
	if err != nil {
		return fmt.Errorf("%w: pid %d: %v", ErrProcessIdentityUnresolved, want.PID, err)
	}
	if !now.Equal(want.StartedAt) {
		return fmt.Errorf("%w: pid %d now belongs to a process started %s, not the one resolved at %s (recycled)",
			ErrProcessIdentityUnresolved, want.PID, now, want.StartedAt)
	}
	return nil
}

// ProcessIdentityCoverage answers colab-fleet #116's fourth requirement: do
// sessions this driver tracks always appear wherever the authoritative
// process mapping lives? total is every live (non-dead) session this
// enumeration found; unresolved names the ones among them
// ResolveProcessIdentity-equivalent logic could not resolve a process
// identity for. A gap here — unresolved non-empty — is exactly the failure
// #116's own report names: "a relay silently misses exactly the sessions we
// started".
//
// This is a query, not a background job: nothing in this package schedules
// it. A caller wanting a continuous signal runs it on an interval, the same
// way cmd/colab-fleetd already schedules Driver.SeedTrustRoots; wiring that
// schedule is left to whoever picks that up, deliberately, so this change
// stays inside internal/drivers/tmux (see the ADR's "why no scheduling"
// note).
func (d *Driver) ProcessIdentityCoverage(ctx context.Context) (total int, unresolved []string, err error) {
	ctx, cancel := d.bounded(ctx)
	defer cancel()

	rows, _, err := d.enumerate(ctx)
	if err != nil {
		return 0, nil, err
	}
	for _, r := range rows {
		if r.dead {
			continue
		}
		total++
		if r.pid <= 0 {
			d.counters.incr(counterProcessIdentityUnresolved)
			unresolved = append(unresolved, r.session)
			continue
		}
		if _, err := d.processStartedAt(ctx, r.pid); err != nil {
			d.counters.incr(counterProcessIdentityUnresolved)
			unresolved = append(unresolved, r.session)
		}
	}
	return total, unresolved, nil
}

// processStartedAt is the one place this file asks the OS, rather than the
// multiplexer, a question. `ps -o lstart=` is the same class of external
// call this driver already makes to `tmux` (execFunc, d.run) — same
// contract, same test seam, deliberately a separate field (d.psRun) so a
// fake built for one can never silently stand in for the other. Never
// cached: every caller above calls this fresh, which is what makes
// ResolveProcessIdentity's "authoritative, not cached" claim true rather
// than aspirational.
//
// A PID the OS no longer has, or one `ps` cannot answer for any other
// reason, both come back as a plain error here — this function draws no
// distinction between them, because #116's callers only ever ask "can I
// trust this identity right now", not "why not".
func (d *Driver) processStartedAt(ctx context.Context, pid int) (time.Time, error) {
	out, err := d.psRun(ctx, d.psBin, "-o", "lstart=", "-p", strconv.Itoa(pid))
	if err != nil {
		return time.Time{}, fmt.Errorf("pid %d: %w", pid, err)
	}
	line := strings.TrimSpace(string(out))
	if line == "" {
		return time.Time{}, fmt.Errorf("pid %d: no such process", pid)
	}
	t, err := time.ParseInLocation(psStartTimeLayout, line, time.Local)
	if err != nil {
		return time.Time{}, fmt.Errorf("pid %d: unrecognised ps output %q: %w", pid, line, err)
	}
	return t, nil
}
