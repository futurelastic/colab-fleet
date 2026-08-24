package tmux

import (
	"errors"
	"fmt"
	"os"
	"strings"

	fleet "github.com/godx-jp/colab-fleet"
)

// colab-fleet issue #94: an operator declaring what THIS machine's sessions
// always carry, instead of every caller having to pass the same variable on
// every create forever — and the one caller that forgets not failing, but
// starting a healthy-looking session that falls back to whatever ambient
// identity the consuming tool finds on disk.
//
// # Why this lives in the driver, not the HTTP handler
//
// A create aimed at this machine can arrive two ways: served locally, or
// relayed here by a peer that merely forwards the request body — env
// included — verbatim (internal/drivers/remote). Provisioning in the HTTP
// handler would run on every request THIS SERVICE HANDLES, including one it
// is only relaying onward to a THIRD machine, which would read this
// machine's files and ship the value over the network to a session that
// will run somewhere else entirely. "Per-machine identity" has to mean the
// machine that runs the session, not the machine that took the call, so
// this only runs where a session is actually started — Driver.Create,
// after the relay decision has already been made by whoever resolved which
// driver instance to call.
//
// # Why the declaration is loaded once and the value is not
//
// The list of entries — which variables, from which files, required or not,
// scoped to what — is machine configuration, read at daemon start
// (cmd/colab-fleetd/config.go's DisallowUnknownFields means it must exist in
// the code before it can exist in a config file at all, so enabling this
// needs a restart same as DefaultRuntime and TrustRoots).
//
// The VALUE behind each entry is not cached alongside it. It is read fresh
// from FromFile on every Create. Caching it at startup was the obvious
// implementation and the wrong one: it would silently defeat credential
// rotation — rotate the file, and every session created afterward would
// keep receiving the old value until somebody restarted the service, with
// nothing to say why. Reading per create means a rotation takes effect on
// the very next session, no restart, no coordination.

// SessionEnvScope narrows which sessions one SessionEnvEntry applies to.
//
// The zero value matches every session on this machine — appliesTo is the
// escape hatch for the session that must deliberately act as something
// other than the configured identity, and it exists so that exception costs
// an operator a configuration edit rather than a code change and a deploy.
// Without it, the first such exception would be the most expensive kind of
// change to make on exactly the path where a bug means every new session on
// the machine refuses.
type SessionEnvScope struct {
	// Agents matches SessionSpec.Agent.
	Agents []string
	// Markers matches SessionSpec.Marker.
	Markers []string
}

// matches reports whether spec falls inside the scope. A scope naming
// neither axis matches everything — the common case, an operator declaring
// an identity for the whole machine rather than carving out an agent.
func (s SessionEnvScope) matches(spec fleet.SessionSpec) bool {
	if len(s.Agents) == 0 && len(s.Markers) == 0 {
		return true
	}
	for _, a := range s.Agents {
		if string(spec.Agent) == a {
			return true
		}
	}
	for _, m := range s.Markers {
		if spec.Marker == m {
			return true
		}
	}
	return false
}

// SessionEnvEntry is one operator-declared variable this machine's sessions
// should carry, read from FromFile at create time rather than at daemon
// start (see the package-level note above for why that split is the whole
// point).
type SessionEnvEntry struct {
	// Name is the variable name a session process will see. Validated at
	// startup by ValidateSessionEnv, not on every create, for the same
	// reason colab-fleet issue #60's DefaultRuntime is: a typo here should
	// be a message an operator reads once, not a refusal every later
	// caller meets.
	Name string
	// FromFile is an absolute path this driver reads fresh on every
	// Create. Never staged, logged, or placed in an argv — it flows
	// straight into the same staging file (stageEnv) a caller's own Env
	// values already go through, and out through the same login-shell
	// wrap.
	FromFile string
	// Required makes a missing, unreadable or empty FromFile refuse the
	// create rather than silently omit the variable. Per entry rather
	// than a machine-wide switch, because both answers are legitimate for
	// different variables on the same machine — an identity credential
	// probably wants Required; a nice-to-have probably does not.
	Required bool
	// AppliesTo scopes this entry. See SessionEnvScope.
	AppliesTo SessionEnvScope
}

// ValidateSessionEnv checks every entry's shape once, at startup, so a
// misconfiguration is a message an operator reads at daemon start rather
// than a create-time refusal every later caller meets — the same reasoning
// colab-fleet issue #60 states for DefaultRuntime.
func ValidateSessionEnv(entries []SessionEnvEntry) error {
	seen := make(map[string]bool, len(entries))
	for _, e := range entries {
		if !validEnvName(e.Name) {
			return fmt.Errorf("sessionEnv: %q is not a usable variable name "+
				"(letters, digits and underscore, not starting with a digit)", e.Name)
		}
		if e.FromFile == "" {
			return fmt.Errorf("sessionEnv: %q has no fromFile", e.Name)
		}
		if !strings.HasPrefix(e.FromFile, "/") {
			return fmt.Errorf("sessionEnv: %q's fromFile %q must be an absolute path", e.Name, e.FromFile)
		}
		// Two entries for the same name would leave "which one wins"
		// unanswered — refused here rather than resolved by iteration
		// order, which would make the answer depend on how the config
		// file happened to list them.
		if seen[e.Name] {
			return fmt.Errorf("sessionEnv: %q is declared more than once", e.Name)
		}
		seen[e.Name] = true
	}
	return nil
}

// provisionSessionEnv merges this machine's declared identity into spec.Env,
// and is where colab-fleet issue #94's precedence table (recorded in full on
// the issue) is implemented:
//
//   - required entry, caller silent           → configuration provides it
//   - required entry, caller supplies ANY value → refuse, uncompared
//   - non-required entry, caller supplies a value → the caller wins
//   - non-required entry, caller silent          → configuration provides it
//
// A session outside an entry's AppliesTo scope never meets that entry at
// all: not provisioned, not required, not compared against. That is the
// escape hatch appliesTo exists to be.
//
// # A required entry never compares a caller's value against the configured one — this was an equality oracle
//
// An earlier revision let a caller supply the identical value and proceed,
// on the reasoning that agreement is not a conflict. It was wrong: a caller
// holding only the `create` grant could supply a CANDIDATE value for a
// variable it has no read access to and learn, from whether the create
// succeeded or was refused, whether the candidate matched the configured
// secret. Brute-forcing a long credential this way is infeasible and was
// never the threat — confirming a candidate is. "Is this stale copy still
// the live credential" is exactly the question someone holding a leaked or
// outdated value wants answered, and the old rule answered it through a
// documented, authorized path with no failed-auth trace anywhere. See the
// issue thread for the full write-up; the general shape is worth carrying
// elsewhere too — branching OBSERVABLY on a comparison against a
// caller-unreadable secret is an oracle whatever the surrounding intent was,
// and the intent here was only ever to be helpful to a caller who already
// happened to hold the right value.
//
// So a required entry now refuses ANY caller-supplied value for that name,
// without reading the configured file and without comparing — the read and
// the comparison are what made the outcome depend on the secret, so both are
// skipped entirely rather than performed and then discarded, which would
// reopen the same oracle as a timing difference between the match and
// no-match paths. The refusal message says only that the name is required on
// this machine and must be omitted; it never says whether the supplied value
// would have matched.
//
// # Which refusal is which kind, and why that is a decision rather than a default
//
// Two different things can go wrong here, and they must not answer alike:
//
//   - The CALLER supplied ANY value for a name a required entry owns.
//     Correctable by the caller — omit the field — so this returns a bare
//     error, which falls through writeDriverError's default arm to
//     "invalid" / 400 exactly like every other malformed-input refusal
//     already in Create.
//   - This MACHINE cannot back a required entry: the file is missing,
//     unreadable, or empty. No correction to the request body fixes a
//     machine whose credential file is absent — the caller would retry
//     forever against a condition only an operator can clear. Reported
//     instead as a typed *fleet.Error with Kind: fleet.ErrorUnsupported
//     (501), matching the wire answer the OTHER driver in this repository
//     already gives for its own env refusal (internal/drivers/opencode's
//     blanket "no honest way to honour env"). Before this, the two answered
//     the same category of refusal two different ways — this closes that
//     asymmetry rather than adding a third answer. driver.ErrUnsupported's
//     own doc comment states the exact semantics wanted here: "nothing will
//     change by asking again", which is precisely true of a create that
//     will keep failing until an operator repairs the file — so the message
//     addresses the operator, not the caller, and says so.
func (d *Driver) provisionSessionEnv(spec fleet.SessionSpec) (map[string]string, error) {
	if len(d.sessionEnv) == 0 {
		return spec.Env, nil
	}

	var merged map[string]string
	for _, entry := range d.sessionEnv {
		if !entry.AppliesTo.matches(spec) {
			continue
		}

		_, callerSet := spec.Env[entry.Name]

		// A required entry refuses ANY caller-supplied value for this name,
		// before the configured file is read and without ever comparing —
		// see the doc comment above. This must be the first thing checked
		// once an entry is in scope: reading the file first and comparing
		// after would still make the refusal depend on the caller's value,
		// which is the oracle this guard exists to close.
		if entry.Required && callerSet {
			return nil, fmt.Errorf(
				"create: env %s is required on this machine and must be omitted from the "+
					"request; this machine provides its own value for it",
				entry.Name)
		}

		value, err := readSessionEnvFile(entry.FromFile)
		if err != nil {
			if !entry.Required {
				continue // legitimate: not required, silently absent (§94)
			}
			return nil, &fleet.Error{
				Kind: fleet.ErrorUnsupported,
				Message: fmt.Sprintf(
					"create: sessionEnv %q is required on this machine but could not be "+
						"provisioned from %s: %v — an operator must repair that file; "+
						"retrying this create will not help until they do",
					entry.Name, entry.FromFile, err),
				Machine: d.machine,
			}
		}

		if callerSet {
			// Only reachable here when the entry is NOT required — a
			// required entry with callerSet already returned above. "The
			// caller wins" (§94's precedence table): the caller's own value
			// is already in spec.Env/merged, so there is nothing to compare
			// and nothing to do. There is no protected value to leak on this
			// path, because a non-required entry never refuses on the
			// caller's account.
			continue
		}

		if merged == nil {
			merged = make(map[string]string, len(spec.Env)+len(d.sessionEnv))
			for k, v := range spec.Env {
				merged[k] = v
			}
		}
		merged[entry.Name] = value
	}

	if merged == nil {
		return spec.Env, nil
	}
	return merged, nil
}

// readSessionEnvFile reads and bounds-checks one configured value.
//
// The bound is the same one validateEnv already enforces on a caller's own
// values, for the identical reason: the staging file this feeds into
// (stageEnv) is line-oriented, so a value containing a newline would arrive
// as a second, fabricated variable. Checked HERE rather than left to
// validateEnv's later pass over the merged map, because a bound violation in
// a configured value is this machine's problem, not the caller's — folding
// it into validateEnv would report an operator's bad file to the caller as
// a 400, exactly the misattribution colab-fleet issue #94 warns against for
// the missing-file case. A value that passes this check is guaranteed to
// pass validateEnv afterward, so that later pass never fires for a reason
// that traces back to configuration.
func readSessionEnvFile(path string) (string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	value := strings.TrimRight(string(raw), "\r\n")
	if value == "" {
		return "", errors.New("file is empty")
	}
	if strings.ContainsAny(value, "\n\x00") {
		return "", errors.New("value contains a newline or NUL, which the staging format cannot carry")
	}
	return value, nil
}
