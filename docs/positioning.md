# Positioning — where this sits among comparable projects

Scanned 2026-08-21, roughly twenty-five projects, checked against repositories
and primary documentation rather than listicles. A star count with no recent
commit date was treated as meaningless and discarded.

This is not a feature comparison table. A reader arriving cold wants three
things: what category this actually is, which of its properties turned out to
be rare, and what that implies for where effort should go next. That is the
shape of this page.

## The category, and the ones it gets mistaken for

The space this sits in is a crowded one, and it is easy to conflate with four
adjacent categories that solve a different problem:

- **Multi-provider agent CLIs.** One runtime pointed at many model providers.
  Several are very large — one is approaching two hundred thousand stars. They
  have no fleet concept and do not drive anyone else's CLI. The most frequently
  cited "comparable" project, and a different product category.
- **Agent frameworks and SDK orchestrators.** Headless libraries. They do not
  attach to an interactive session that is already running. One borderline
  case runs its own agent server and can integrate third-party backends through
  an open protocol, but it manages them through its own server rather than by
  attaching to a terminal, and its multi-host story is manual switching between
  separate connections rather than one federated list.
- **Terminal control-plane primitives.** Mostly dumb pipes with no agent
  semantics. One is agent-aware and assembled into a real product.
- **Vendor tooling.** Every vendor ships tooling that drives only its own
  agent. No vendor ships a heterogeneous driver.

**Parallel-session managers and worktree orchestrators** are the genuinely
comparable pile, and it is crowded — several are actively committing. This is
where single-machine, multi-session management has already been solved a
dozen ways over. That is the peer group this project should be judged against,
not the multi-provider CLIs it gets name-checked alongside.

## What turned out to be rare

Individually, most of the properties this service has exist somewhere in the
survey:

- **Driving several vendors' interactive CLIs behind one interface** — several
  projects do this, one with a notably broad vendor list.
- **A three-state classification** separating busy, idle, and blocked-on-a-
  dialog — one project does this well, with per-vendor detection strategies,
  and even answers permission prompts by spawning a cheap verifier model.
  Worth reading if smarter auto-answering is ever wanted here.
- **A long-lived service with a documented HTTP API over agent sessions** —
  one project, the closest architectural match overall.

**The combination is what did not appear.** Specifically:

- **Peer federation of pre-existing sessions across independent machines,
  presented as one list: zero examples across the whole survey.** Projects
  that mention multiple machines mean something else — where an agent's tool
  calls execute, manual switching between backends, or elastic cloud sandboxes.
  Not one federates sessions that already exist on two hosts.
- **A delivery outcome that can report "sent but unconfirmed"**, rather than
  treating a send as fire-and-forget.
- **Distinguishing "the machine did not answer" from "the session is not
  there"** — a pair several designs collapse into one state.

The closest architectural match reports session status as exactly two values,
stable or running, so a session blocked on a permission prompt is
indistinguishable from one that is genuinely finished. That single distinction
is most of what this service's state taxonomy exists for.

## Trajectory — two signals, pointing opposite ways, both worth weighing

**Against the screen-reading approach.** The well-funded projects in this
space are moving away from driving interactive terminals, not toward doing it
better. Two notable ones changed course within the survey window: one shut
down, one pivoted from wrapping an agent CLI to a headless platform. Capital
in this space is betting on the opposite design from the one a terminal driver
represents.

**For the layer above.** No vendor and no open standard currently offers a
generic, multi-vendor way for a third party to ask an arbitrary running agent
process on another machine what state it is in, and answer its prompt. The one
real cross-vendor standard is an editor-driven protocol, about a year old,
whose adapters for the major agents were written by a third party rather than
by the vendors themselves. Adoption is real but concentrated in editor
integration, not fleet orchestration.

**Read together, this is the calibrating argument for where investment goes:**
the driver layer is a bridge across a gap that may close on its own, and the
durable asset is the federation, addressing and authorization layer above it —
not the driver beneath it. If the bottom half is ever rewritten, the seam to
build against is that open protocol rather than another vendor's terminal
quirks. Nothing in the survey shows that becoming necessary yet.

## Why the ground is thin — evidenced, not assumed

- Single-machine multi-session management is crowded because that is what
  most people need. Cross-host federation is a problem only people running an
  actual fleet ever hit.
- Robust per-vendor state detection is unglamorous ongoing maintenance. Most
  projects support two or three vendors well and wave at the rest.
- Federating across machines is a distributed-systems problem — partitions,
  conflicting claims, authority — and out of scope for a tool aimed at one
  developer on one laptop, which nearly every candidate in the survey is.

## Verdict

Not rebuilding something that exists. The gap this occupies is real, and it is
the unglamorous half: evidence-backed state with a nonce-guarded answer path,
and peer federation, sitting on top of a service with an actual API.

**Not independently verified:** one vendor's claimed support for the
cross-vendor protocol rests on secondary reporting only, and the
attach-versus-spawn behaviour of two of the session managers surveyed is
undocumented in their own READMEs.
