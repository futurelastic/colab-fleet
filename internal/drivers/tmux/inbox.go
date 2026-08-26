package tmux

// This file wires colab-fleet #119: delivering Send's final hop over a
// target session's own inbox, in front of the terminal-surface path this
// driver already had, when — and only when — a caller has capability-
// detected an inbox for that target. The terminal path (tmux.go's Send)
// stays the fallback verbatim; nothing in this file changes it, and a
// Driver that never calls WithInboxResolver behaves exactly as it did
// before #119.
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

// sendViaInbox is Send's capability-detected fast path (#119). ok=false
// (with a zero DeliveryReceipt and nil error) means no inbox capability
// applies to this call, so the caller falls through to the pane path
// unchanged; every ok=true return is this path's own, final answer for the
// call — never partial, never a guess.
func (d *Driver) sendViaInbox(ctx context.Context, ref fleet.SessionRef, text string) (fleet.DeliveryReceipt, bool, error) {
	if d.inboxResolver == nil {
		return fleet.DeliveryReceipt{}, false, nil
	}

	// #116's requirement #1: resolve fresh, never from this driver's own
	// memory of an earlier call.
	identity, err := d.ResolveProcessIdentity(ctx, ref)
	if err != nil {
		if errors.Is(err, ErrProcessIdentityUnresolved) {
			// No authoritative identity to hand the resolver — a fact
			// about the TARGET (dead pane, no such session), which the
			// pane path below already re-derives and reports on its own
			// terms. Falling through here avoids reporting it twice, in
			// two different vocabularies, for the same call.
			return fleet.DeliveryReceipt{}, false, nil
		}
		return fleet.DeliveryReceipt{}, false, err
	}

	addr, ok, rerr := d.inboxResolver(ctx, identity)
	if rerr != nil || !ok {
		// See InboxResolver's own doc comment: both cases mean "no usable
		// capability for this call", never a refusal.
		return fleet.DeliveryReceipt{}, false, nil
	}

	// #116's requirement #2: verify immediately before the write, not
	// merely at resolution time above — the gap between the two calls,
	// however small, is exactly where a recycled pid would surface. This
	// is the check #117's ruling calls "more load-bearing, not less": a
	// valid socket and valid auth would otherwise deliver cleanly to
	// whatever now holds this pid, reporting success at every layer.
	if verr := d.VerifyProcessIdentity(ctx, identity); verr != nil {
		return fleet.DeliveryReceipt{
			Outcome: fleet.OutcomeRefused,
			Reason: fmt.Sprintf("identity could not be verified immediately before the "+
				"inbox write, so nothing was sent: %v", verr),
		}, true, nil
	}

	dctx, cancel := context.WithTimeout(ctx, inboxRoundTripTimeout)
	defer cancel()
	network := addr.Network
	if network == "" {
		network = "unix"
	}
	conn, derr := d.inboxDial(dctx, network, addr.Socket)
	if derr != nil {
		return fleet.DeliveryReceipt{
			Outcome: fleet.OutcomeRefused,
			Reason:  fmt.Sprintf("could not reach this session's inbox: %v", derr),
		}, true, nil
	}
	defer conn.Close()

	receipt, derr := inboxclient.Deliver(conn, addr.Token, text, inboxRoundTripTimeout)
	if derr != nil {
		return fleet.DeliveryReceipt{
			Outcome: fleet.OutcomeUnknown,
			Reason:  fmt.Sprintf("inbox delivery did not resolve to a receipt: %v", derr),
		}, true, nil
	}
	return fleet.DeliveryReceipt{
		Outcome: mapInboxOutcome(receipt.Outcome),
		Reason:  receipt.Reason,
	}, true, nil
}

// mapInboxOutcome is the one place inboxclient.Outcome becomes fleet.Outcome
// — a single switch, one for one, so "was anything flattened here" is
// answerable by reading one function rather than auditing every call site
// (#117's "surface the receipt vocabulary honestly").
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
		// inboxclient.Deliver already refuses to return an outcome outside
		// its own closed set (Outcome.valid()), so this default is
		// unreachable from that path — kept as the honest fallback rather
		// than a panic, the same "fail toward unknown" discipline
		// session-abstraction.md §2 states for every other field.
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
// let sendViaInbox reinterpret a pane-shaped request.
func inboxEligible(opts driver.SendOptions) bool {
	return opts.Submit && !opts.ResumeIfStranded && !opts.ReplaceIfStranded
}
