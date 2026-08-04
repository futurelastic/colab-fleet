package fleet

// Response answers a prompt a session is blocked on (§3).
//
// # Why this is not just send()
//
// send() delivers a message, and delivers it through a paste buffer
// specifically so the bytes are never interpreted as control input — a message
// containing "C-c" must not interrupt the session receiving it. That property
// is correct for messages and is exactly what makes a prompt unanswerable:
// a menu wants a keypress, and send() is built to guarantee it never sends one.
//
// So answering is a different operation, not a flag on delivery. The
// separation also means the two can be authorised differently and refused for
// different reasons, which they should be: delivering text to a working
// session is harmless, while answering a prompt commits to whatever the prompt
// was asking.
//
// # Why a choice and not a key
//
// §5.1 says the interface expresses questions, never mechanisms — state()
// rather than readScreen(), send() rather than typeKeys(). A "press Enter"
// operation would bind every future driver to this substrate's idea of
// confirmation. A choice is what the caller actually means; how a driver
// produces it is the driver's business.
type Response struct {
	// Choice selects a numbered option, 1-based. Zero means "accept
	// whatever is highlighted", which is what a caller usually wants and
	// what a human pressing Enter would get.
	Choice int

	// Cancel dismisses the prompt instead of answering it. A caller that
	// does not like any of the options needs a way to say so that is not
	// "pick one anyway".
	Cancel bool

	// Nonce is the SessionPrompt.Nonce the caller was answering.
	//
	// A caller reads a prompt, shows it to a human, and answers seconds or
	// minutes later. In between the session may have moved on and be showing
	// a DIFFERENT question in the same place — and an answer submitted by
	// index would be applied to it, silently. Supplying the nonce turns that
	// into a refusal.
	//
	// Optional, and its absence is not free: a driver must say, in the
	// receipt, that it answered without checking. An automated caller should
	// always send it; a human at a terminal answering immediately reasonably
	// may not.
	Nonce string
}
