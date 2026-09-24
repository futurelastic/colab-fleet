package modclient

import (
	"bufio"
	"bytes"
)

// MaxLineBytes is the longest line (excluding its newline) the client accepts
// from a module. A response is untrusted input: a module that writes a
// multi-gigabyte line without a newline must cost us a bounded amount of
// memory, not the daemon.
const MaxLineBytes = 4 << 20

// ReadLine reads one newline-terminated line from r.
//
// A line longer than MaxLineBytes is DRAINED to its newline WITHOUT being
// buffered and reported as oversize (line is nil): the caller drops it and the
// very next call reads the following line, so one huge response costs one lost
// answer, not the connection. The loop uses ReadSlice and stops accumulating the
// moment a line passes the limit, so memory stays within a small multiple of
// MaxLineBytes no matter how long the line is.
//
// A trailing '\r' is trimmed. A final line with no newline before EOF (or a
// read error) is a torn write and is discarded: the error is returned with a
// nil line. The returned slice is the caller's own; it is not aliased to r.
func ReadLine(r *bufio.Reader) (line []byte, oversize bool, err error) {
	var buf bytes.Buffer
	for {
		frag, rerr := r.ReadSlice('\n')
		complete := rerr == nil // frag ends with '\n'
		if !oversize {
			content := buf.Len() + len(frag)
			if complete {
				content--
			}
			if content > MaxLineBytes {
				oversize = true
				buf = bytes.Buffer{} // stop holding bytes; keep draining
			} else if complete && buf.Len() == 0 {
				// The common case: the whole line arrived in one fragment.
				return trimCR(append([]byte(nil), frag[:len(frag)-1]...)), false, nil
			} else {
				buf.Write(frag)
			}
		}
		switch {
		case complete:
			if oversize {
				return nil, true, nil
			}
			b := buf.Bytes()
			return trimCR(b[:len(b)-1]), false, nil
		case rerr == bufio.ErrBufferFull:
			continue
		default:
			return nil, oversize, rerr
		}
	}
}

func trimCR(b []byte) []byte {
	if n := len(b); n > 0 && b[n-1] == '\r' {
		return b[:n-1]
	}
	return b
}

// readTruncated reads one line but keeps only its first keep bytes, draining
// the rest without buffering it. It is for text we only ever LOG (stderr):
// unlike ReadLine it returns a final unterminated line together with the error,
// because the last words of a dying module are the ones worth having.
func readTruncated(r *bufio.Reader, keep int) (line []byte, truncated bool, err error) {
	for {
		frag, rerr := r.ReadSlice('\n')
		data := frag
		if rerr == nil {
			data = frag[:len(frag)-1]
		}
		if room := keep - len(line); room > 0 {
			if len(data) > room {
				line = append(line, data[:room]...)
				truncated = true
			} else {
				line = append(line, data...)
			}
		} else if len(data) > 0 {
			truncated = true
		}
		switch {
		case rerr == nil:
			return line, truncated, nil
		case rerr == bufio.ErrBufferFull:
			continue
		default:
			return line, truncated, rerr
		}
	}
}
