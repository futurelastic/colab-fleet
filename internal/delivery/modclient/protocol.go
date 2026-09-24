package modclient

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"
)

// ProtocolVersion is the only protocol number this build speaks. A module whose
// hello names another number is refused outright: there is no negotiation,
// because a guess about what an unknown protocol means is exactly the kind of
// guess that delivers a message twice.
const ProtocolVersion = 1

// RequiredOps are the six operations every module must advertise in its hello.
// A module missing any of them is refused: a lane that can send but not
// confirm, or attach but not close, would strand state on one side of the pipe.
var RequiredOps = []string{"prepare-launch", "attach", "send", "confirm", "close", "health"}

// The operation names, for callers and tests that need the wire spelling.
const (
	OpPrepareLaunch = "prepare-launch"
	OpAttach        = "attach"
	OpSend          = "send"
	OpConfirm       = "confirm"
	OpClose         = "close"
	OpHealth        = "health"
)

// Limits on what a module may make us hold. Every response field is untrusted
// input, so each string and collection that is retained or logged is capped
// here rather than trusted to be small.
const (
	maxHelloPrefixes = 64
	maxHelloOps      = 64
	maxIdentLen      = 64   // module name, version strings, op names
	maxLaneKeyLen    = 128  // laneKey, sendId, session ids
	maxTextLen       = 256  // reason, kind, state, error message...
	maxPathLen       = 4096 // transcript.path — carried, never opened
	maxEnvEntries    = 64
	maxEnvNameLen    = 128
	maxEnvValueLen   = 32 << 10
	maxEnvTotalLen   = 256 << 10
	maxRawFieldLen   = 64 << 10 // retained JSON blobs of a health result
)

// --- wire types -------------------------------------------------------------

// Hello is the first line a module writes. Unknown fields are ignored.
type Hello struct {
	Event               string   `json:"event"`
	Module              string   `json:"module"`
	Protocol            int      `json:"protocol"`
	Version             string   `json:"version"`
	Ops                 []string `json:"ops"`
	ReservedEnvPrefixes []string `json:"reservedEnvPrefixes"`
}

// PrepareLaunchArgs asks for the environment a session's agent process needs.
type PrepareLaunchArgs struct {
	ClaudeVersion string `json:"claudeVersion,omitempty"`
}

// PrepareLaunchResult carries the module's environment for one launch. The
// values are paths the launch already handles; the client validates their
// syntax and size and never logs them.
type PrepareLaunchResult struct {
	LaneKey       string            `json:"laneKey"`
	ClaudeVersion string            `json:"claudeVersion"`
	Env           map[string]string `json:"env"`
}

// AttachArgs opens (or re-opens) the module's channel for a lane.
type AttachArgs struct {
	LaneKey   string `json:"laneKey"`
	PID       int    `json:"pid,omitempty"`
	Cwd       string `json:"cwd,omitempty"`
	TimeoutMs int    `json:"timeoutMs,omitempty"`
}

// AttachResult is the answer to attach. A negative probe is a RESULT
// (Live=false with a Reason), never an error.
type AttachResult struct {
	LaneKey       string `json:"laneKey"`
	Live          bool   `json:"live"`
	State         string `json:"state"`
	Reason        string `json:"reason,omitempty"`
	SessionID     string `json:"sessionId,omitempty"`
	ClaudeVersion string `json:"claudeVersion,omitempty"`
	PeerVerified  *bool  `json:"peerVerified,omitempty"`
	SinceMs       int64  `json:"sinceMs"`
}

// SendArgs delivers one message on a lane.
type SendArgs struct {
	LaneKey           string `json:"laneKey"`
	Text              string `json:"text"`
	AllowLeadingSlash bool   `json:"allowLeadingSlash,omitempty"`
	ConfirmWaitMs     int    `json:"confirmWaitMs,omitempty"`
}

// SendResult reports a delivery attempt. Written is NOT delivery: only a
// confirm verdict says whether the message landed.
type SendResult struct {
	SendID     string         `json:"sendId"`
	Written    bool           `json:"written"`
	Transcript Transcript     `json:"transcript"`
	Confirm    *ConfirmResult `json:"confirm,omitempty"`
}

// Transcript is the handle a module returns so the service could confirm from
// the runtime's own transcript. Path is CARRIED ONLY: the client never opens it,
// because a response must never choose a file the service then reads.
type Transcript struct {
	SessionID string `json:"sessionId"`
	Path      string `json:"path,omitempty"`
	Offset    int64  `json:"offset"`
}

// ConfirmArgs asks whether an earlier send landed.
type ConfirmArgs struct {
	LaneKey string `json:"laneKey"`
	SendID  string `json:"sendId"`
	WaitMs  int    `json:"waitMs,omitempty"`
}

// ConfirmResult is the module's verdict. Verdict is one of confirmed, queued,
// silent, rejected; Final is true for confirmed and rejected.
type ConfirmResult struct {
	Verdict   string `json:"verdict"`
	Kind      string `json:"kind,omitempty"`
	Enqueued  bool   `json:"enqueued"`
	ElapsedMs int64  `json:"elapsedMs"`
	Final     bool   `json:"final"`
}

// CloseArgs tears a lane down; the operation is idempotent.
type CloseArgs struct {
	LaneKey string `json:"laneKey"`
}

// CloseResult reports whether the lane existed.
type CloseResult struct {
	Removed bool `json:"removed"`
}

// HealthArgs asks for liveness, optionally with one lane's detail.
type HealthArgs struct {
	LaneKey string `json:"laneKey,omitempty"`
}

// HealthResult is the module's own account of itself. SupportedClaude, Lanes
// and Counters are opaque here: they are retained (size-capped) for an operator
// to read and are never interpreted.
type HealthResult struct {
	OK                  bool            `json:"ok"`
	Module              string          `json:"module"`
	Protocol            int             `json:"protocol"`
	Version             string          `json:"version"`
	Platform            string          `json:"platform"`
	SupportedClaude     json.RawMessage `json:"supportedClaude,omitempty"`
	PeerCheck           bool            `json:"peerCheck"`
	ReservedEnvPrefixes []string        `json:"reservedEnvPrefixes"`
	Lanes               json.RawMessage `json:"lanes,omitempty"`
	Counters            json.RawMessage `json:"counters,omitempty"`
}

// --- errors -----------------------------------------------------------------

// Error is a module's own refusal: an ok:false response. It is a complete,
// well-formed answer — the request reached the module and was decided — so it
// is neither ErrNotSent nor ErrLost. Message is UNTRUSTED prose: it is capped
// and stripped of control characters, and it is quoted by Error().
type Error struct {
	Code      string
	Message   string
	Retryable bool
}

func (e *Error) Error() string {
	return fmt.Sprintf("modclient: module answered %s: %q", e.Code, e.Message)
}

// IsCode reports whether err is a module *Error carrying code.
func IsCode(err error, code string) bool {
	var e *Error
	return errors.As(err, &e) && e.Code == code
}

var (
	// ErrNotSent means the request never reached the module: it was refused
	// locally (the module is unavailable, the caller's context was already
	// done), or it was still queued when the caller gave up, or the pipe was
	// already broken before a single byte of it was written. Nothing happened
	// on the module side, so a caller may safely fall back to another path.
	ErrNotSent = errors.New("request not sent")

	// ErrLost means at least one byte of the request was written but no usable
	// answer came back (deadline, the module died, an unparseable response).
	// The module MAY have acted on it. The outcome is unknown and the request
	// must never be resent or replayed on another path.
	ErrLost = errors.New("request written but no usable answer")

	// ErrDeadline marks an error caused by the module missing its own
	// per-request deadline (as opposed to the caller's context ending). It is
	// always wrapped together with ErrNotSent or ErrLost.
	ErrDeadline = errors.New("module missed its deadline")

	// ErrBadResponse marks an answer that violates the protocol (unparseable
	// result, a lane key that cannot be echoed safely, an oversized field). It
	// satisfies errors.Is for ErrLost as well: the request reached the module,
	// so its effect is unknown.
	ErrBadResponse = errors.New("module response violates the protocol")
)

// badResponseError is an unusable answer; it counts as both ErrBadResponse and
// ErrLost.
type badResponseError struct{ op, why string }

func (e *badResponseError) Error() string {
	return "modclient: " + e.op + ": unusable response: " + e.why
}

func (e *badResponseError) Is(target error) bool {
	return target == ErrBadResponse || target == ErrLost
}

func badResponse(op, why string) error { return &badResponseError{op: op, why: clean(why, 200)} }

// notSent wraps ErrNotSent (and cause, when given) for op.
func notSent(op string, cause error, format string, args ...any) error {
	detail := fmt.Sprintf(format, args...)
	if cause != nil {
		return fmt.Errorf("modclient: %s: %w (%w): %s", op, ErrNotSent, cause, detail)
	}
	return fmt.Errorf("modclient: %s: %w: %s", op, ErrNotSent, detail)
}

// --- hello ------------------------------------------------------------------

// HelloRefusal is returned by ParseHello when a module's first line is not one
// this build may talk to. The supervisor disables the module and logs Reason
// once; a refusal is never retried, because the same binary will say the same
// thing again.
type HelloRefusal struct{ Reason string }

func (e *HelloRefusal) Error() string { return "modclient: hello refused: " + e.Reason }

func refuse(format string, args ...any) error {
	return &HelloRefusal{Reason: clean(fmt.Sprintf(format, args...), maxTextLen)}
}

// ParseHello validates a module's first line: valid JSON; event "hello"; a
// non-empty module name; protocol equal to ProtocolVersion; ops a superset of
// RequiredOps; every reserved prefix a ValidReservedPrefix. Unknown fields are
// ignored. Every error is a *HelloRefusal.
func ParseHello(line []byte) (Hello, error) {
	var h Hello
	if err := json.Unmarshal(line, &h); err != nil {
		return Hello{}, refuse("not a valid hello object: %v", err)
	}
	if h.Event != "hello" {
		return Hello{}, refuse("first line has event %q, want \"hello\"", clean(h.Event, 32))
	}
	if strings.TrimSpace(h.Module) == "" {
		return Hello{}, refuse("hello names no module")
	}
	if h.Protocol != ProtocolVersion {
		return Hello{}, refuse("protocol %d is not supported (this build speaks %d)", h.Protocol, ProtocolVersion)
	}
	have := make(map[string]bool, len(h.Ops))
	for _, op := range h.Ops {
		have[op] = true
	}
	var missing []string
	for _, op := range RequiredOps {
		if !have[op] {
			missing = append(missing, op)
		}
	}
	if len(missing) > 0 {
		return Hello{}, refuse("hello lacks required operations: %s", strings.Join(missing, ", "))
	}
	if len(h.ReservedEnvPrefixes) > maxHelloPrefixes {
		return Hello{}, refuse("hello reserves %d environment prefixes (at most %d)", len(h.ReservedEnvPrefixes), maxHelloPrefixes)
	}
	seen := map[string]bool{}
	var prefixes []string
	for _, p := range h.ReservedEnvPrefixes {
		if !ValidReservedPrefix(p) {
			return Hello{}, refuse("hello carries a malformed reserved environment prefix %q", clean(p, 32))
		}
		if !seen[p] {
			seen[p] = true
			prefixes = append(prefixes, p)
		}
	}
	ops := make([]string, 0, len(h.Ops))
	for _, op := range h.Ops {
		if len(ops) == maxHelloOps {
			break
		}
		ops = append(ops, clean(op, maxIdentLen))
	}
	return Hello{
		Event:               "hello",
		Module:              clean(strings.TrimSpace(h.Module), maxIdentLen),
		Protocol:            h.Protocol,
		Version:             clean(h.Version, maxIdentLen),
		Ops:                 ops,
		ReservedEnvPrefixes: prefixes,
	}, nil
}

// ValidReservedPrefix reports whether p may be a reserved environment prefix:
// non-empty, at most 64 bytes, and made only of [A-Za-z_][A-Za-z0-9_]*. The
// grammar is the environment-variable grammar on purpose — a prefix that could
// not begin a variable name reserves nothing and is a module bug.
func ValidReservedPrefix(p string) bool {
	return len(p) <= 64 && validEnvName(p)
}

// validEnvName reports whether s matches [A-Za-z_][A-Za-z0-9_]*.
func validEnvName(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '_', c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z':
		case i > 0 && c >= '0' && c <= '9':
		default:
			return false
		}
	}
	return true
}

// --- untrusted-text hygiene ---------------------------------------------------

// clean makes module-supplied text safe to keep and log: invalid UTF-8 is
// replaced, control characters become '?', and the result is cut to at most max
// bytes on a rune boundary.
func clean(s string, max int) string {
	if !utf8.ValidString(s) {
		s = strings.ToValidUTF8(s, "�")
	}
	var b strings.Builder
	for _, r := range s {
		if unicode.IsControl(r) {
			r = '?'
		}
		if b.Len()+utf8.RuneLen(r) > max {
			break
		}
		b.WriteRune(r)
	}
	return b.String()
}

// validKey reports whether s can be kept as an opaque handle (laneKey, sendId)
// and echoed back byte-for-byte: 1..maxLaneKeyLen bytes of valid UTF-8 with no
// control characters. A key containing path separators IS accepted — it is
// opaque, and it is the caller's rule never to build a path from it.
func validKey(s string) bool {
	if s == "" || len(s) > maxLaneKeyLen || !utf8.ValidString(s) {
		return false
	}
	for _, r := range s {
		if unicode.IsControl(r) {
			return false
		}
	}
	return true
}

func capRaw(raw json.RawMessage) json.RawMessage {
	if len(raw) > maxRawFieldLen {
		return nil
	}
	return raw
}

// --- result checks ------------------------------------------------------------
//
// Each check runs on a result that already decoded into its Go type (so field
// types are right and unknown fields are gone) and enforces size and content
// limits. A failure makes the whole response unusable; it is never partially
// trusted.

func (r *PrepareLaunchResult) check() error {
	if !validKey(r.LaneKey) {
		return errors.New("laneKey is missing or malformed")
	}
	r.ClaudeVersion = clean(r.ClaudeVersion, maxIdentLen)
	if len(r.Env) > maxEnvEntries {
		return fmt.Errorf("env has %d entries (at most %d)", len(r.Env), maxEnvEntries)
	}
	total := 0
	for k, v := range r.Env {
		switch {
		case len(k) > maxEnvNameLen || !validEnvName(k):
			return errors.New("env carries a malformed variable name")
		case len(v) > maxEnvValueLen:
			return errors.New("env carries an oversized value")
		case strings.IndexByte(v, 0) >= 0:
			return errors.New("env carries a value containing NUL")
		}
		total += len(k) + len(v)
	}
	if total > maxEnvTotalLen {
		return errors.New("env is oversized")
	}
	if r.Env == nil {
		r.Env = map[string]string{}
	}
	return nil
}

func (r *AttachResult) check() error {
	if r.LaneKey != "" && !validKey(r.LaneKey) {
		return errors.New("laneKey is malformed")
	}
	r.State = clean(r.State, 32)
	r.Reason = clean(r.Reason, maxTextLen)
	r.SessionID = clean(r.SessionID, maxLaneKeyLen)
	r.ClaudeVersion = clean(r.ClaudeVersion, maxIdentLen)
	return nil
}

func (r *SendResult) check() error {
	if r.SendID != "" && !validKey(r.SendID) {
		return errors.New("sendId is malformed")
	}
	r.Transcript.SessionID = clean(r.Transcript.SessionID, maxLaneKeyLen)
	r.Transcript.Path = clean(r.Transcript.Path, maxPathLen)
	if r.Confirm != nil {
		r.Confirm.check()
	}
	return nil
}

func (r *ConfirmResult) check() error {
	r.Verdict = clean(r.Verdict, 32)
	r.Kind = clean(r.Kind, 64)
	return nil
}

func (r *CloseResult) check() error { return nil }

func (r *HealthResult) check() error {
	r.Module = clean(r.Module, maxIdentLen)
	r.Version = clean(r.Version, maxIdentLen)
	r.Platform = clean(r.Platform, maxIdentLen)
	r.SupportedClaude = capRaw(r.SupportedClaude)
	r.Lanes = capRaw(r.Lanes)
	r.Counters = capRaw(r.Counters)
	if r.ReservedEnvPrefixes != nil {
		valid := make([]string, 0, len(r.ReservedEnvPrefixes))
		seen := map[string]bool{}
		for _, p := range r.ReservedEnvPrefixes {
			if len(valid) == maxHelloPrefixes {
				break
			}
			if ValidReservedPrefix(p) && !seen[p] {
				seen[p] = true
				valid = append(valid, p)
			}
		}
		r.ReservedEnvPrefixes = valid
	}
	return nil
}
