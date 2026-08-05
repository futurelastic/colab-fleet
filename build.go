package fleet

import (
	"runtime"
	"runtime/debug"
	"strings"
)

// Build identifies the code a service is running.
//
// # Why a wire type, and why it took an incident to add one
//
// Two machines in one fleet silently ran different builds. The older one still
// had a bug the newer had fixed, and the symptom — a session stranded at a
// question the newer code answers — made no sense against the source anyone
// was reading. The whole diagnosis was spent looking for a defect that had
// already been fixed.
//
// Nothing anywhere could have said so. Every surface reported the service was
// healthy, because by its own standard it was: it was running, answering, and
// correct for the code it happened to be. **A distributed system where a
// participant cannot state which code it is running has no way to distinguish
// "we disagree" from "we are different vintages"** — and those need opposite
// responses. The first is a bug; the second is a deploy.
//
// This is §5.7 in a new place. Not knowing a peer's build must be
// distinguishable from a peer whose build matches, so `Known` is explicit
// rather than inferred from an empty revision.
//
// # Why the VCS stamp rather than a version constant
//
// A hand-maintained version constant records what someone remembered to bump;
// the toolchain's VCS stamp records what was actually compiled. `Modified`
// matters more than either: a binary built from uncommitted changes has no
// meaningful identity at all, and saying so is more useful than a revision
// that names a commit the binary does not contain.
type Build struct {
	// Known is false when the toolchain supplied no VCS information — a
	// binary built from a source tree with no repository, or with
	// -buildvcs=false. Absence of a stamp is not evidence of a match.
	Known bool `json:"known"`
	// Revision is the commit the binary was built from.
	Revision string `json:"revision,omitempty"`
	// Modified reports uncommitted changes in the tree at build time. A
	// modified build never compares equal to anything, including itself.
	Modified bool `json:"modified,omitempty"`
	// Time is the commit timestamp, RFC3339, useful for "which is older"
	// when two revisions are both unfamiliar.
	Time string `json:"time,omitempty"`
	// Go is the toolchain version. Skew here is rarer and less dangerous
	// than source skew, but it costs one field to rule out.
	Go string `json:"go,omitempty"`
}

// SameAs reports whether two builds are demonstrably the same code.
//
// Unknown or modified builds are never the same as anything — including an
// identical-looking counterpart. That asymmetry is deliberate: this predicate
// exists to raise a warning, and a false "same" suppresses exactly the warning
// worth having. Answering "not demonstrably the same" for a pair that happens
// to match costs a log line; answering "same" for a pair that does not costs
// the incident this type was added after.
func (b Build) SameAs(other Build) bool {
	if !b.Known || !other.Known || b.Modified || other.Modified {
		return false
	}
	return b.Revision == other.Revision
}

// DifferenceFrom explains why two builds do not compare equal, or returns ""
// when they do.
//
// SameAs answers a yes/no that has three causes underneath it, and an operator
// needs to know which: two different revisions is a deploy that has not
// finished, while an unknown or dirty build is a comparison that could not be
// made at all. Reporting the second as "differs" sends someone looking for a
// version mismatch that may not exist — the same class of wasted diagnosis
// this type was added to prevent.
func (b Build) DifferenceFrom(other Build) string {
	switch {
	case b.SameAs(other):
		return ""
	case !b.Known && !other.Known:
		return "neither build is stamped, so skew cannot be ruled out"
	case !b.Known:
		return "ours is not stamped, so skew cannot be ruled out"
	case !other.Known:
		return "theirs is not stamped, so skew cannot be ruled out"
	case b.Modified && other.Modified:
		return "both were built from uncommitted changes and have no comparable identity"
	case b.Modified:
		return "ours was built from uncommitted changes and has no comparable identity"
	case other.Modified:
		return "theirs was built from uncommitted changes and has no comparable identity"
	default:
		return "different revisions"
	}
}

// Short renders a build for a log line or a status display.
func (b Build) Short() string {
	if !b.Known {
		return "unknown"
	}
	rev := b.Revision
	if len(rev) > 12 {
		rev = rev[:12]
	}
	if b.Modified {
		return rev + "+dirty"
	}
	return rev
}

// SelfBuild reports the build identity of the running binary.
//
// Read once at startup by the service and served from GET /v1/health; a caller
// asking a peer gets the peer's answer, which is the entire point.
func SelfBuild() Build {
	b := Build{Go: runtime.Version()}
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return b
	}
	for _, s := range info.Settings {
		switch s.Key {
		case "vcs.revision":
			b.Revision, b.Known = s.Value, true
		case "vcs.modified":
			b.Modified = s.Value == "true"
		case "vcs.time":
			b.Time = s.Value
		}
	}
	// A revision that is present but empty is not a stamp. Guard rather than
	// trust, because the failure this type exists to catch is precisely a
	// confident report built on an absent measurement.
	if strings.TrimSpace(b.Revision) == "" {
		b.Known, b.Revision = false, ""
	}
	return b
}
