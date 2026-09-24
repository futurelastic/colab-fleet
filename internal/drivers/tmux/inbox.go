package tmux

// This file wires colab-fleet #119: delivering Send's final hop over a
// target session's own inbox, in front of the terminal-surface path this
// driver already had, when — and only when — a caller has capability-
// detected an inbox for that target. The terminal path (tmux.go's Send)
// stays the fallback verbatim; nothing in this file changes it, and a
// Driver that never calls WithInboxResolver behaves exactly as it did
// before #119.
//
// # Since #184
//
// The inbox is now one of two paths a send chooses between by route (auto,
// terminal, inbox — Send in tmux.go), and a delivery over it is CONFIRMED from
// the receiver's own transcript (inbox_confirm.go) instead of being reported
// delivered on a clean write. A decline is only ever a decline while nothing has
// been written; once any byte has gone out the message is the inbox's, an
// unconfirmed one is recorded in the cross-path ledger (route.go), and the same
// text is never sent down the other path. docs/adr/184-route-by-sender.md is the
// whole argument.
//
// Everything genuinely protocol-specific — the auth line, the message line,
// the receipt line — lives in internal/inboxclient, which knows the bytes
// and nothing about where either endpoint of a connection is found. This
// file is the other half: identity (#116), capability detection, and
// mapping a receipt onto fleet.Outcome honestly (#117's ruling). Where a
// real socket lives on disk, and where a real per-session token comes from,
// is machine-local knowledge #117 authorises this service to hold and
// CLAUDE.local.md forbids this PUBLIC repo from ever naming — see InboxAddress
// and InboxResolver below for why that knowledge is injected, not baked in.

import (
	"context"
	"errors"
	"fmt"
	"net"
	"time"

	fleet "github.com/godx-jp/colab-fleet"
	"github.com/godx-jp/colab-fleet/internal/driver"
	"github.com/godx-jp/colab-fleet/internal/inboxclient"
)

// InboxAddress is where and how to reach one session's own inbox, for one
// already-resolved ProcessIdentity. Both fields are supplied per call by an
// InboxResolver — never derived here from a path convention this repository
// does not commit; see internal/probe's own #118 spike test doc comment for
// why not ("the general mechanism ... not the paths").
type InboxAddress struct {
	// Network and Socket name a net.Dial target (in practice "unix" and a
	// filesystem path, but this file never assumes that — see inboxDialFunc).
	Network string
	Socket  string
	// Token is this session's own per-session inbox credential — read by
	// whatever #117's grant authorises on the resolver's side. Never cached
	// by this driver: the same "never cached across a call" discipline
	// ProcessIdentity's own doc comment states for a different kind of
	// identity, applied here to a credential instead of a pid.
	Token string
	// ModeClass is the permission-mode class the TARGET session runs in, as
	// known to whatever supplies this address — never a fact this driver
	// derives, and never a default. Empty means the resolver could not say,
	// which makes the inbox path unavailable for the call (sendViaInbox
	// below); it is not an error and not a refusal.
	//
	// colab-fleet #148: the receiving runtime holds a message whose sender
	// asserts no class whenever the receiver runs with permission prompts
	// bypassed, and holds one asserting the WRONG class in every case. Both
	// are silent — held messages never reach the model and are dropped when
	// the receiver's hold deadline passes, while this driver reported
	// delivered. So the class must travel per target, beside the socket and
	// the token, and be mirrored rather than assumed. See
	// inboxclient.ModeClass for why a single compiled-in class is not a fix.
	ModeClass inboxclient.ModeClass
}

// InboxResolver answers, for one authoritatively-resolved process identity,
// whether that session has an inbox this driver can reach — capability
// detection, not a requirement every target must satisfy (#119: "keep the
// existing pane path as a fallback, capability-detected per target").
//
//   - ok=false, err=nil: no inbox for this target. Not a refusal, and not
//     logged as one — the same not-an-error shape
//     ProcessIdentityCoverage's own "unresolved" already uses for absence.
//     sendViaInbox falls through to the pane path.
//   - err!=nil: the resolver itself could not answer (its own credential
//     store was unreadable, say). Treated identically to ok=false: a
//     caller cannot usefully act on half a capability, and guessing at it
//     is exactly what #116 was filed to stop. The error is not surfaced to
//     Send's caller as a refusal — it would misattribute a resolver-side
//     problem to the session being sent to, which is a different failure
//     with a different owner.
//   - ok=true, err=nil: addr is used for this delivery.
type InboxResolver func(ctx context.Context, identity ProcessIdentity) (addr InboxAddress, ok bool, err error)

// WithInboxResolver enables colab-fleet #119's delivery path. Off by
// default — the same off-by-default contract WithCredentialPath and
// WithTrustSeed already state: a driver built for a test or a sandbox must
// never attempt an inbox delivery merely because it was constructed.
func WithInboxResolver(r InboxResolver) Option {
	return func(d *Driver) { d.inboxResolver = r }
}

// inboxDialFunc is this file's own exec-style seam — deliberately separate
// from d.dial (tmux control-mode connections, subscribe.go) and d.run/d.psRun
// (the multiplexer and the OS process table, tmux.go/processidentity.go's own
// doc comments explain why those two stay apart from each other): a test
// double built for any of those must never silently answer for a socket
// dial too.
type inboxDialFunc func(ctx context.Context, network, address string) (net.Conn, error)

func dialInboxReal(ctx context.Context, network, address string) (net.Conn, error) {
	var dialer net.Dialer
	return dialer.DialContext(ctx, network, address)
}

// withInboxDial injects a fake dialer. Unexported: tests only, the same
// shape as withExec/withPSExec.
func withInboxDial(f inboxDialFunc) Option { return func(d *Driver) { d.inboxDial = f } }

// inboxRoundTripTimeout bounds one inbox exchange — auth line, message
// line, response line — independent of d.deadline (§4.4's declared per-call
// deadline governs enumerate/ps, not this network hop #119 adds). Pinned to
// inboxclient.FirstLineDeadline: #115 measured the runtime's own first-line
// deadline at "a few seconds", and waiting longer than the far end will
// wait buys this driver nothing but a slower failure.
const inboxRoundTripTimeout = inboxclient.FirstLineDeadline

// inboxResult is what one pass through the inbox path amounted to (#184).
//
// Final says whether the inbox path ANSWERED. When it did, Receipt is the
// answer and the caller returns it: the message is now on the inbox path's
// account and no other path may carry it. When it did not, nothing was written —
// Why says in words what stopped it — and the caller either falls back to the
// terminal (route auto) or turns Why into a refusal (route inbox). The two are
// deliberately one type: a decline is exactly "no byte written", the only state
// from which either continuation is safe.
type inboxResult struct {
	Receipt fleet.DeliveryReceipt
	Final   bool
	// Tried is true once the path had a resolver to try, whatever came of it.
	// It separates "this machine has no inbox" (nothing to count) from "the
	// inbox declined this send" (the fallback rate an operator watches).
	Tried bool
	Why   string
}

func inboxDeclined(why string) inboxResult { return inboxResult{Tried: true, Why: why} }

func inboxAnswered(r fleet.DeliveryReceipt) inboxResult {
	return inboxResult{Receipt: r.WithRoute(fleet.RouteInbox), Final: true, Tried: true}
}

// sendViaInbox is Send's capability-detected inbox path (#119, #184). A result
// with Final=false (and a nil error) means the inbox could not carry this call
// and NOTHING was written; every Final=true result is this path's own, final
// answer for the call — never partial, never a guess.
//
// #144: Final=false covers two different things, deliberately conflated the
// same way capability-absence and a resolver error already were before this
// change — "this driver could not make the inbox path work for this call" and
// "there is no inbox for this call" are both facts about THIS attempt, not an
// answer from the target:
//
//   - no resolver configured, no identity, or the resolver itself declined
//   - no class to attest, or a body that cannot be attested
//   - no transcript to confirm a delivery against (#184)
//   - the dial to a resolved address failed
//   - a write that put NOT ONE BYTE on the connection
//
// # Where the line is (#184)
//
// It used to include "the write failed" without asking how far the write got. A
// write can fail after the auth line and half the message have gone out, and a
// receiver given part of a line and then a closed connection is not something a
// sender can prove discards it. Now the fallback is taken only when
// inboxclient.NothingWritten proves the socket got nothing. Any byte at all
// makes the outcome `unknown`, on the inbox's account, and the same text is
// never sent down the other path. A complete write is confirmed from the
// receiver's transcript (confirmInbox) — delivered if it shows, `unknown` if it
// does not, and never a terminal resend either way.
//
// The one other case that stays final with no fallback is identity verification
// failing immediately before the write: a deliberate refusal (#116), not a
// failed attempt, and ADR 119 explains why falling back there would defeat the
// check. It is verified twice — once where ADR 148 needs it, before attestation,
// and once immediately before the dial, after the work that takes time — and
// either failing is that same final refusal.
func (d *Driver) sendViaInbox(ctx context.Context, ref fleet.SessionRef, text string, opts driver.SendOptions) (inboxResult, error) {
	if d.inboxResolver == nil {
		return inboxResult{Why: "this machine has no inbox delivery configured"}, nil
	}
	from := opts.From
	// #150: from here on, every return below increments exactly one exit
	// counter — see counters.go for why each reason is counted apart.
	d.counters.incr(counterInboxAttempted)

	// #116's requirement #1: resolve fresh, never from this driver's own
	// memory of an earlier call.
	identity, row, err := d.resolveProcessIdentityRow(ctx, ref)
	if err != nil {
		if errors.Is(err, ErrProcessIdentityUnresolved) {
			// No authoritative identity to hand the resolver — a fact
			// about the TARGET (dead pane, no such session), which the
			// pane path below already re-derives and reports on its own
			// terms. Falling through here avoids reporting it twice, in
			// two different vocabularies, for the same call.
			d.counters.incr(counterInboxFallbackIdentityUnresolved)
			return inboxDeclined("the session's process could not be identified: " + err.Error()), nil
		}
		d.counters.incr(counterInboxErrorIdentityResolve)
		return inboxResult{}, err
	}

	addr, ok, rerr := d.inboxResolver(ctx, identity)
	if rerr != nil || !ok {
		// See InboxResolver's own doc comment: both cases mean "no usable
		// capability for this call", never a refusal. #150 counts them apart
		// all the same: one is this machine failing to read its own index,
		// the other a target that has no inbox.
		if rerr != nil {
			d.counters.incr(counterInboxFallbackResolverError)
			return inboxDeclined("this machine could not read its inbox index"), nil
		}
		d.counters.incr(counterInboxFallbackResolverDeclined)
		return inboxDeclined("the session has no inbox entry"), nil
	}

	// #116's requirement #2: verify immediately before the write, not
	// merely at resolution time above — the gap between the two calls,
	// however small, is exactly where a recycled pid would surface. This
	// is the check #117's ruling calls "more load-bearing, not less": a
	// valid socket and valid auth would otherwise deliver cleanly to
	// whatever now holds this pid, reporting success at every layer.
	if verr := d.VerifyProcessIdentity(ctx, identity); verr != nil {
		return d.inboxIdentityRefused(verr), nil
	}

	// #148: attest the target's permission-mode class, or do not use this path
	// at all. Attest refuses when the resolver named no class, named an
	// unusable one, or when the text cannot be wrapped in a form guaranteed to
	// survive the receiver's own byte-for-byte rebuild check.
	//
	// This is ADR 119's rule applied to a piece of the capability we did not
	// previously know was part of it: the honest response to half a capability
	// is the same as to none of it. Sending anyway is what produced #148 —
	// this driver reported delivered while the receiver held the message for a
	// human who was never coming, then dropped it at its own hold deadline.
	//
	// Placed AFTER the first identity verification above and BEFORE the dial
	// below, deliberately. Verification failing is a final refusal with no
	// fallback (ADR 119: falling back there would defeat the check), so
	// checking attestability first would silently convert that refusal into a
	// fallback for any send that merely happened to be unattestable — turning
	// a security gate off through an unrelated door. Nothing is dialled for a
	// send that cannot be attested.
	//
	// #158: the sender label rides in the envelope's sender-name attribute, and
	// a relayOfHuman declaration as the body's first line. Attest drops a name
	// it cannot guarantee rather than refusing the send, so the label never
	// changes whether this path is taken.
	//
	// #150: the body is checked and counted BEFORE Attest, independently of the
	// class, because Attest refuses a missing class first and would otherwise
	// hide every body refusal behind it. The body counted is exactly the body
	// Attest is given — declaration line included.
	body := driver.WithDeclaration(from, text)
	d.counters.incr(counterInboxAttestChecked)
	if !inboxclient.BodyAttestable(body) {
		d.counters.incr(counterInboxAttestBodyLookalike)
	}
	attested, ok := inboxclient.Attest(body, addr.ModeClass, driver.SenderLabel(from))
	if !ok {
		if !addr.ModeClass.Valid() {
			d.counters.incr(counterInboxFallbackNoModeClass)
			return inboxDeclined("the inbox index names no permission-mode class for this session, so a message cannot be attested"), nil
		}
		d.counters.incr(counterInboxFallbackBodyUnattestable)
		return inboxDeclined("the text cannot be carried in a peer-message envelope that is guaranteed to arrive intact"), nil
	}

	// #184: a delivery that cannot be confirmed cannot be stood behind, and the
	// confirmation is the receiver's own transcript. Located BEFORE the write —
	// the offset it carries is the transcript's size now, which is what makes an
	// identical earlier message unable to confirm this one.
	src, srcOK := d.resolveTranscriptSource(ctx, ref, &row)
	if !srcOK {
		d.counters.incr(counterInboxFallbackNoTranscript)
		return inboxDeclined("the session's transcript could not be located, so a delivery could not be confirmed"), nil
	}

	// The second verification, after everything above that takes time and
	// immediately before the dial (#116).
	if verr := d.VerifyProcessIdentity(ctx, identity); verr != nil {
		return d.inboxIdentityRefused(verr), nil
	}

	dctx, cancel := context.WithTimeout(ctx, inboxRoundTripTimeout)
	defer cancel()
	network := addr.Network
	if network == "" {
		network = "unix"
	}
	conn, derr := d.inboxDial(dctx, network, addr.Socket)
	if derr != nil {
		// #144: a dial failure is a fact about this attempt's transport, not
		// an answer from the target — the pane path independently re-derives
		// whether the session itself is reachable, so this falls through to
		// it rather than reporting a permanent refusal here.
		d.counters.incr(counterInboxFallbackDialFailed)
		return inboxDeclined("the session's inbox could not be reached"), nil
	}
	defer conn.Close()

	probe := newInboxProbe(attested, body)
	key := deliveryKey(text, from)
	entry := unconfirmedEntry{
		Key: key, Cwd: row.cwd, TranscriptPath: src.path, TranscriptOffset: src.offset, Probe: probe,
	}

	_, werr := inboxclient.Deliver(conn, addr.Token, attested, inboxRoundTripTimeout)
	if werr != nil {
		if inboxclient.NothingWritten(werr) {
			// #144, narrowed by #184: not one byte reached the connection, so
			// the receiver cannot have been given anything and the pane path
			// may carry the message. This is the only write failure that may.
			d.counters.incr(counterInboxFallbackWriteFailed)
			return inboxDeclined("the write to the session's inbox failed before any byte was sent"), nil
		}
		// Some bytes went out. The message may be in the receiver's hands, so
		// this is `unknown` on the inbox's account and the terminal is not
		// tried — see the doc comment above.
		d.counters.incr(counterInboxUnknownPartialWrite)
		entry.Partial = true
		d.noteUnconfirmed(ref.ID, entry)
		return inboxAnswered(fleet.DeliveryReceipt{
			Outcome: fleet.OutcomeUnknown,
			Reason: "the write to the session's inbox broke part-way, so the message may or may not have " +
				"reached the receiver. It was NOT sent again on any path: sending it again could deliver it " +
				"twice. Read the session's transcript before deciding; a resend of the same text is held for " +
				strandedRetention.String() + " or until the message is recorded",
		}), nil
	}
	d.counters.incr(counterInboxWritten)

	// A clean write proves only that bytes reached a socket (#144). What the
	// receiver did with them is in its transcript.
	if how := d.confirmInbox(ctx, src, probe); how != inboxScanSilent {
		d.counters.incr(counterInboxConfirmed)
		if how == inboxScanByEnvelope {
			d.counters.incr(counterInboxConfirmedByEnvelope)
		} else {
			d.counters.incr(counterInboxConfirmedByOriginBody)
		}
		return inboxAnswered(fleet.DeliveryReceipt{
			Outcome: fleet.OutcomeDelivered,
			Reason:  "the receiver's own transcript recorded this message as a peer message (" + src.evidence + ")",
		}), nil
	}
	d.counters.incr(counterInboxUnconfirmed)
	d.noteUnconfirmed(ref.ID, entry)
	return inboxAnswered(fleet.DeliveryReceipt{
		Outcome: fleet.OutcomeUnknown,
		Reason: "the message was written to the session's inbox but the receiver's transcript did not record " +
			"it within " + inboxWindowOf(d).String() + " (" + src.evidence + "). It may still arrive, or the " +
			"receiver may be holding it. It was NOT sent again on any path: sending it again could deliver " +
			"it twice. Read the session's transcript before deciding; a resend of the same text is held for " +
			strandedRetention.String() + " or until the message is recorded",
	}), nil
}

// inboxIdentityRefused is the final refusal for an identity that could not be
// verified (#116): nothing was written, and no path is tried — the terminal
// would defeat the check (ADR 119).
func (d *Driver) inboxIdentityRefused(verr error) inboxResult {
	d.counters.incr(counterInboxRefusedIdentityUnverified)
	return inboxAnswered(fleet.DeliveryReceipt{
		Outcome: fleet.OutcomeRefused,
		Reason: fmt.Sprintf("identity could not be verified immediately before the "+
			"inbox write, so nothing was sent: %v", verr),
	})
}

// inboxWindowOf is the confirmation window in force, for a receipt's reason.
func inboxWindowOf(d *Driver) time.Duration {
	if d.inboxWindow > 0 {
		return d.inboxWindow
	}
	return inboxConfirmWindow
}

// mapInboxOutcome is the one place inboxclient.Outcome becomes fleet.Outcome
// — a single switch, one for one, so "was anything flattened here" is
// answerable by reading one function rather than auditing every call site
// (#117's "surface the receipt vocabulary honestly"). #144: inboxclient.Deliver
// currently only ever produces OutcomeDelivered (see its own doc comment —
// there is no reply address to observe the other five over yet, #120); the
// rest of this switch stays exhaustive and ready rather than trimmed to the
// one reachable case, so wiring a future receipt path costs nothing here.
func mapInboxOutcome(o inboxclient.Outcome) fleet.Outcome {
	switch o {
	case inboxclient.OutcomeDelivered:
		return fleet.OutcomeDelivered
	case inboxclient.OutcomeHeld:
		return fleet.OutcomeHeld
	case inboxclient.OutcomeDenied:
		return fleet.OutcomeDenied
	case inboxclient.OutcomeExpired:
		return fleet.OutcomeExpired
	case inboxclient.OutcomeRefused:
		return fleet.OutcomeRefused
	case inboxclient.OutcomeDropped:
		return fleet.OutcomeDropped
	default:
		// Deliver only ever constructs a Receipt with OutcomeDelivered
		// today (see its own doc comment, #144), so this default is
		// unreachable from that path in practice — kept as the honest
		// fallback rather than a panic, the same "fail toward unknown"
		// discipline session-abstraction.md §2 states for every other
		// field, in case a future #120 fix ever hands this an outcome
		// outside the set above.
		return fleet.OutcomeUnknown
	}
}

// inboxEligible reports whether opts describes a call the inbox path can
// even attempt. Three flags all name a pane-composer shape the inbox path
// has no analogue for — Submit=false asks to land text without submitting,
// which presumes a composer to land it in; ResumeIfStranded and
// ReplaceIfStranded both ask to finish or discard a PANE delivery that
// stranded earlier, which the inbox path cannot have done (it has no
// composer to strand in — see the #119 issue body's own "the failure mode
// ... stops existing"). A caller asking for any of these is explicitly
// asking for the pane, so Send skips the inbox attempt entirely rather than
// let sendViaInbox reinterpret a pane-shaped request. A forced terminal route
// (#184) is the fourth: the caller has asked for the terminal by name.
func inboxEligible(opts driver.SendOptions) bool {
	return opts.Submit && !opts.ResumeIfStranded && !opts.ReplaceIfStranded && opts.Route != fleet.RouteTerminal
}

// panePrefix opens the first line the terminal path adds for a labelled send.
const panePrefix = "[from: "

// paneLabelled is the terminal path's form of the sender label (colab-fleet
// #158). That path has no envelope, so the label goes on as one short first
// line of the text, followed by the relayOfHuman declaration when asked for.
//
// It uses the same normalised name the envelope would carry, which also keeps
// it one line: SenderName drops every line and paragraph separator. A label
// that normalises to nothing adds no line at all.
//
// Send applies this AFTER the #53 runtime-syntax guard has judged the caller's
// own text. Prefixing first would move a leading slash command off the first
// line and past the guard, which is the opposite of what a label is for.
func paneLabelled(text string, from *fleet.MessageFrom) string {
	text = driver.WithDeclaration(from, text)
	if name := inboxclient.SenderName(driver.SenderLabel(from)); name != "" {
		return panePrefix + name + "]\n" + text
	}
	return text
}
