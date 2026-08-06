package fleet

// SessionEnvironment records what a session's process actually received.
//
// # Why this is a wire type and not a log line
//
// A session created through the API and a session created by an on-machine
// launcher do not necessarily get the same environment, and until this type
// existed the only way to find out was to read two process-manager unit files
// on two different hosts and compare them by eye. That is inference, and it had
// already gone wrong: the two units in one fleet built PATHs of different
// lengths. Nobody chose that. It is what hand-maintained environment blocks on
// two machines do.
//
// Wrapping a created session in a login shell fixes the parity but does not
// make it CHECKABLE — the environment then depends on a shell configuration
// file this service does not own, cannot validate, and cannot report. This type
// is the part that keeps the dependency visible: the service still does not own
// the file, but it now records what came out of it, so drift is something you
// read rather than something you deduce.
//
// # Why there are no values here
//
// The whole point of the login shell is that it exports credentials. So this
// type carries variable NAMES and never values — a name says an MCP credential
// is present, which is the question being asked, while a value would put the
// credential itself into an API response, a log, and any client that caches
// one.
//
// Path is the single exception, and it is not an exception to the rule: a
// search path is not a secret, and it is the exact drift that motivated the
// type. Recording its entries is the difference between "the two machines
// differ somehow" and "this one has an extra entry, here it is".
type SessionEnvironment struct {
	// Known is false when no record was captured. §5.7: a driver that
	// cannot report this must say so rather than return an empty record
	// that reads as "the session had no environment".
	Known bool `json:"known"`

	// Reason explains a false Known — the capture timed out, the substrate
	// does not support it, the session was not created by this service.
	Reason string `json:"reason,omitempty"`

	// Shell, Login and Interactive describe the MECHANISM, which is half the
	// answer to "did this session get what a launcher-created one gets".
	//
	// Interactive is load-bearing and is the field most likely to be assumed
	// rather than read. A login shell that is NOT interactive does not read
	// the interactive startup file, which on a normal developer machine is
	// exactly where credentials are exported — so a session can be started
	// through a login shell, report a login shell, and still have none of
	// them. Measured, not assumed: see the driver's loginWrap.
	Shell       string `json:"shell,omitempty"`
	Login       bool   `json:"login"`
	Interactive bool   `json:"interactive"`

	// Names is every environment variable name the session process held at
	// the moment it exec'd the agent, sorted. Never values.
	Names []string `json:"names,omitempty"`

	// Path is PATH split into entries, in order.
	Path []string `json:"path,omitempty"`

	// ServiceNames is the same enumeration for the SERVICE's own process.
	//
	// It is here so a reader can see what the shell ADDED rather than only
	// what the session ended up with. Without it, "this session has 40
	// variables" is a number with nothing to compare against; with it, the
	// difference is the answer to whether the wrapping did anything at all.
	ServiceNames []string `json:"serviceNames,omitempty"`

	// ServicePath is the service process's own PATH entries — the value the
	// process-manager unit sets by hand, and therefore the one that drifted.
	ServicePath []string `json:"servicePath,omitempty"`

	CapturedAt *Timestamp `json:"capturedAt,omitempty"`
}

// AddedByShell returns the variable names the session has that the service
// process did not — what the shell configuration contributed.
//
// This is the field a reader actually wants: an empty result means the login
// shell added nothing, which means the credentials are not there, which means
// the session will start fine and fail at its first tool call. That failure is
// invisible at creation time and this makes it visible.
func (e SessionEnvironment) AddedByShell() []string {
	if !e.Known {
		return nil
	}
	have := make(map[string]bool, len(e.ServiceNames))
	for _, n := range e.ServiceNames {
		have[n] = true
	}
	var added []string
	for _, n := range e.Names {
		if !have[n] {
			added = append(added, n)
		}
	}
	return added
}
