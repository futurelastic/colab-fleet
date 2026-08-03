package fleet

import "fmt"

// ErrorKind is the closed set of wire error kinds (api-http.md §2).
type ErrorKind string

const (
	ErrorInvalid      ErrorKind = "invalid"
	ErrorUnauthorized ErrorKind = "unauthorized"
	ErrorNotFound     ErrorKind = "not_found"
	ErrorConflict     ErrorKind = "conflict"
	ErrorUnsupported  ErrorKind = "unsupported"
	// ErrorUnreachable means the machine did not answer; nothing is known.
	// Must never be conflated with ErrorNotFound (api-http.md §2): one
	// means the fleet knows the session does not exist, the other means
	// the fleet knows nothing at all. A client that treats a 504 as a 404
	// will confidently report work as gone while it is running fine on an
	// unreachable host.
	ErrorUnreachable ErrorKind = "unreachable"
)

// DefaultHTTPStatus is the api-http.md §2 table, kept next to the kind it
// describes so a Go client never has to hardcode it separately from the
// server that emits it.
func (k ErrorKind) DefaultHTTPStatus() int {
	switch k {
	case ErrorInvalid:
		return 400
	case ErrorUnauthorized:
		return 401
	case ErrorNotFound:
		return 404
	case ErrorConflict:
		return 409
	case ErrorUnsupported:
		return 501
	case ErrorUnreachable:
		return 504
	default:
		return 500
	}
}

// Error is the wire error body (api-http.md §2), wrapped in {"error": ...}
// by ErrorEnvelope. It implements the standard error interface so it can
// travel through ordinary Go error-handling paths inside the service.
type Error struct {
	Kind      ErrorKind `json:"kind"`
	Message   string    `json:"message"`
	Machine   MachineId `json:"machine,omitempty"`
	Retryable bool      `json:"retryable"`
}

func (e *Error) Error() string {
	if e.Machine != "" {
		return fmt.Sprintf("fleet: %s (%s, machine=%s)", e.Message, e.Kind, e.Machine)
	}
	return fmt.Sprintf("fleet: %s (%s)", e.Message, e.Kind)
}

// ErrorEnvelope is the top-level JSON body of every non-2xx response
// (api-http.md §2).
type ErrorEnvelope struct {
	Error Error `json:"error"`
}
