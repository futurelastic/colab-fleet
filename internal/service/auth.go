package service

import (
	"crypto/subtle"
	"net/http"
	"strings"

	fleet "github.com/godx-jp/colab-fleet"
)

// Per-principal authorization (§6).
//
// # What replaced one shared token, and why
//
// A single shared secret makes three of §6's four requirements unstatable.
// Requirement 3 wants mutation grants "opt-in per peer" — impossible when
// every caller presents the same string. Requirement 4 wants an audit trail of
// "actor, verb, target, outcome" — and the best actor a shared token can name
// is an address, which answers where from and never who. And a leaked secret
// forces every machine to rotate at once, which is why the README called auth
// lifecycle the largest unaddressed surface.
//
// A principal is a name, a credential, and a set of grants. Nothing more: this
// is a small statically-configured fleet (§7.2), not a directory service.
//
// # Grants are per verb, because §6 says so
//
// Reads are one grant; each mutating verb is its own. That granularity is not
// theoretical — "may watch my sessions" and "may kill my sessions" are exactly
// the distinction an operator wants when opening a machine to a peer at all,
// and a single mutate bit forces them together.
//
// GrantRelay is the odd one out and belongs here rather than in a global flag.
// It asks whether this principal may have mutations FORWARDED to peers on its
// behalf, which is a question about what this service will do as a client for
// them — D6's distinction, now expressible per caller instead of per service.
//
// Deliberately mutations only, not reads: a peer-targeted or fleet-scoped
// read stays gated by GrantRead alone (see reading() in http.go), and never
// gets mutating()'s upgrade to this grant. A relayed mutation changes state
// on a machine the caller is not talking to; a relayed read does not — so
// requiring GrantRelay for both would treat reaching and changing as one
// act. Ruled, not defaulted into: colab-fleet #81.

// Grant is a permitted verb (§6 requirement 3).
type Grant string

const (
	GrantRead      Grant = "read"      // list, state, events
	GrantCreate    Grant = "create"    // start sessions on this machine
	GrantSend      Grant = "send"      // deliver input to them
	GrantInterrupt Grant = "interrupt" // interrupt them
	GrantClose     Grant = "close"     // destroy them
	// GrantRename is its own verb rather than folded into close or send.
	//
	// It is not destructive, so lumping it with close would over-restrict; and
	// it is not delivery, so lumping it with send would under-restrict. What it
	// actually does is change the handle every other caller addresses a session
	// by — a distinct kind of power, and §6 says grants are per verb precisely
	// so a distinct power can be granted or withheld on its own.
	//
	// Absent means denied, like every other grant, so existing principals are
	// unaffected until an operator opts them in.
	GrantRename Grant = "rename"
	// GrantDiscard removes unsent composer text. Separate from send because
	// the interesting direction is the reverse of the usual one: an operator
	// may well want a janitor that can CLEAR stranded text without being able
	// to drive sessions at all.
	GrantDiscard Grant = "discard"
	// GrantKeys delivers a RAW KEY EVENT to a session's screen (POST …/keys).
	//
	// Not folded into send, though respond is. Respond shares send on a "same
	// blast radius" argument that does not survive here: respond is gated by a
	// prompt the driver RECOGNISED and answers it by index against a nonce,
	// while this one exists precisely for the screens nothing recognises. An
	// operator may quite reasonably permit a supervisor to answer known
	// questions and withhold the ability to press Enter on an unknown screen,
	// and §6 makes grants per verb so that a distinct power can be withheld on
	// its own.
	//
	// Absent means denied, like every other grant, so no existing principal
	// gains this by upgrading.
	GrantKeys  Grant = "keys"
	GrantRelay Grant = "relay" // have mutations proxied to peers
)

// Grants is every grant this service defines, in the order an operator would
// read them: reads, then the mutating verbs, then relay.
//
// It exists because there used to be a SECOND list — the config loader's
// validator — and adding a grant to one and not the other made the service
// refuse to start on a config an operator had every reason to think was valid.
// A grant that exists and cannot be granted is worse than one that does not
// exist. The comment left behind at the time said the real fix was for the
// grant set to have one definition, "worth doing the next time a grant is
// added"; GrantKeys is that time.
func Grants() []Grant {
	return []Grant{
		GrantRead, GrantCreate, GrantSend, GrantInterrupt,
		GrantClose, GrantRename, GrantDiscard, GrantKeys, GrantRelay,
	}
}

// ValidGrant reports whether a string names a grant. The one definition
// anything parsing configuration must go through.
func ValidGrant(g Grant) bool {
	for _, known := range Grants() {
		if g == known {
			return true
		}
	}
	return false
}

// Principal is one identity this service accepts (§6).
type Principal struct {
	// Name is what appears in the audit trail and in Request.Caller. It is
	// an identity, not an address — the whole point of the change.
	Name string
	// Token is the bearer credential this principal presents.
	Token string
	// Grants is what it may do. Absent means read-only is not implied:
	// absent means nothing is permitted, because §6's default is denied.
	Grants []Grant
}

// Allows reports whether this principal holds a grant.
func (p Principal) Allows(g Grant) bool {
	for _, have := range p.Grants {
		if have == g {
			return true
		}
	}
	return false
}

// principalFor resolves a presented bearer token to a principal.
//
// Comparison is constant-time. A token is a secret compared against attacker
// supplied input, and an early-exit compare leaks its prefix through timing —
// cheap to avoid, embarrassing to explain.
func (c Config) principalFor(r *http.Request) (Principal, bool) {
	presented := bearerOf(r)
	if presented == "" {
		return Principal{}, false
	}
	for _, p := range c.Principals {
		if p.Token == "" {
			continue
		}
		if subtle.ConstantTimeCompare([]byte(p.Token), []byte(presented)) == 1 {
			return p, true
		}
	}
	return Principal{}, false
}

// grantForVerb maps an HTTP request to the grant it requires.
//
// Reads are not enumerated: everything that is not a known mutating route is a
// read, which is the safe direction only because the mutating routes are a
// closed set this file lists explicitly. A new mutating endpoint that forgets
// to appear here would be treated as a read, so the list and the mux belong in
// the same review.
func grantForVerb(r *http.Request) Grant {
	path := r.URL.Path
	switch {
	case r.Method == http.MethodPost && strings.HasSuffix(path, "/sessions"):
		return GrantCreate
	case r.Method == http.MethodPost && strings.HasSuffix(path, "/input"):
		return GrantSend
	case r.Method == http.MethodPost && strings.HasSuffix(path, "/respond"):
		// Answering a prompt drives the session, same blast radius as
		// delivering input, so it shares that grant rather than inventing a
		// seventh one nobody would configure separately.
		return GrantSend
	case r.Method == http.MethodPost && strings.HasSuffix(path, "/interrupt"):
		return GrantInterrupt
	case r.Method == http.MethodPost && strings.HasSuffix(path, "/rename"):
		return GrantRename
	case r.Method == http.MethodPost && strings.HasSuffix(path, "/discard"):
		return GrantDiscard
	case r.Method == http.MethodPost && strings.HasSuffix(path, "/keys"):
		return GrantKeys
	case r.Method == http.MethodDelete:
		return GrantClose
	}
	return GrantRead
}

// onBehalfOfHeader carries the principal a relaying service is acting for.
//
// # Why an assertion and not a forwarded credential
//
// §13 requires a proxy to present the ORIGINAL caller's authority. With one
// shared token that was literal: forward the caller's credential and the peer
// accepts it, because it is the peer's credential too.
//
// Per-peer credentials remove that coincidence. A caller's token is meaningless
// on another machine, so there is nothing to forward. The authority a proxied
// request carries therefore splits in two:
//
//   - the relaying machine authenticates as ITSELF, with the credential that
//     machine holds on the peer — this is what the peer authorizes against;
//   - the original principal travels as an assertion, and the peer records it.
//
// The peer trusts the assertion exactly as far as it trusts the relay, which is
// the honest bound: a relay can never obtain more than it was granted, and
// cannot manufacture authority by naming a principal. What the assertion buys
// is the audit trail §6 requirement 4 asks for — "actor, verb, target,
// outcome" — which a bare machine-to-machine credential loses entirely.
//
// Recorded as a finding rather than assumed: D1's fix was right about WHERE
// authority must travel and its mechanism only worked because every machine
// shared a secret.
const onBehalfOfHeader = "Fleet-On-Behalf-Of"

// callerFor builds the Request a resolved principal makes.
func callerFor(p Principal, r *http.Request) fleet.Caller {
	name := p.Name
	// A relayed request names the original principal, with the relaying
	// machine recorded alongside it — an audit line that says only "the
	// peer did it" cannot answer who asked the peer.
	if behalf := r.Header.Get(onBehalfOfHeader); behalf != "" {
		name = behalf + " via " + p.Name
	}
	return fleet.Caller{Principal: name, Credential: p.Token}
}
