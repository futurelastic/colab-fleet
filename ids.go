package fleet

// MachineId identifies one host in the fleet. Addressing is (machine, id) —
// session-abstraction.md §7.1 — so this is never itself an identifier of a
// session, only of the machine that runs one.
type MachineId string

// RuntimeId identifies which driver a session runs under. Opaque to this
// package; drivers own the vocabulary (session-abstraction.md §4).
type RuntimeId string

// AgentId identifies a named persona/config. A hint, not a guarantee — see
// SessionSpec.Agent and §4.3's capability declaration.
type AgentId string

// AbsolutePath is a filesystem path. Everything in this API that names a
// path is absolute; nothing accepts a relative path, and context in
// particular is never inlined into a command line (§5.3) — it travels only
// as an AbsolutePath.
type AbsolutePath string
