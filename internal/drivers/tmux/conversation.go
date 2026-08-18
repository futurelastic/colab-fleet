package tmux

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	fleet "github.com/godx-jp/colab-fleet"
)

// Locating the runtime's own record of a conversation.
//
// # Why this is the first source here that is not an echo
//
// Everything else this driver reports comes, in the end, from the screen the
// runtime chose to paint. When the runtime is the thing that is wrong, every
// one of those readings is wrong together and none of them disagrees — 51 of 52
// sessions read healthy on a machine whose account was refusing all work.
//
// The runtime writes a conversation record for its own purposes, unasked. That
// makes it an independent witness, and being able to say WHICH record belongs
// to a session is worth having before anything ever opens one.
//
// # What the store looks like, measured rather than assumed
//
// One directory per working directory, one file per conversation, the file
// named for the identifier the runtime filed it under. Measured across 545
// records in 58 directories: 518 carry a title record and 516 carry it as the
// very first line. The title's value is the session NAME — the string this
// driver itself passed on the command line at creation (see naming.go and
// claudeCodeCommand). So for anything this service created, matching is not
// inference about the world; it is reading our own input back out of a file
// somebody else wrote.
//
// # Why the name is not enough on its own
//
// Session names are REUSED here by convention: the same name is given to the
// same kind of work every time it starts. In the same measurement, 39
// (directory, name) pairs appeared more than once and one name appeared on 27
// records. A name match alone would confidently return the wrong conversation,
// increasingly often as the store grows.
//
// The tiebreak is not a heuristic. A conversation runs INSIDE the session, so a
// record created before the session existed cannot be that session's — those
// candidates are impossible, not merely unlikely. Measured over 13 live
// sessions: 11 names were unique, 2 were ambiguous with three candidates each,
// and after eliminating the impossible, 12 resolved and one refused. No wrong
// answers.
//
// What is deliberately NOT done is breaking a remaining tie by taking the most
// recently written record. It would have been right in both ambiguous cases
// above, and it is exactly the move this work exists to prevent: a guess shaped
// like a reading, right often enough that nobody checks it.
//
// # Dates come from the record's content, never from the filesystem
//
// The first timestamped line in a file was measured equal to that file's
// creation on every sample, INCLUDING resumed conversations — a resume opens a
// new record and does not copy older timestamps into it. Reading the date out
// of the content rather than out of file metadata also survives any copy,
// restore or file-sync that rewrites mtimes, which is the same reason
// SessionEnvironment records what it read instead of pointing at a file.
//
// # A create that opts out of remote control is unresolvable, permanently
//
// claudeCodeCommand passes the session name to the runtime only inside the
// remote-control branch. A session created with remote control explicitly
// disabled is given no name, writes no title record, and can never be found by
// a name-keyed lookup — not "not yet", ever. That coupling is load-bearing
// here. Changing it changes what a create does, which belongs with capturing
// the identifier at creation rather than with reading it afterwards.

const (
	// recordScanLines bounds how far into a record this driver reads. The
	// title is the first line and the first timestamp lands within a
	// handful; a bound keeps a huge conversation from being parsed to find
	// two fields near its top.
	recordScanLines = 64

	// recordLineLimit bounds one line. A single record can carry a whole
	// pasted file, and an unbounded reader here would pull it into memory on
	// a path that runs on every listing.
	recordLineLimit = 1 << 20

	// recordDateSlack absorbs the disagreement between the multiplexer's
	// second-granularity creation time and the runtime's own millisecond
	// stamp. Deliberately small: it only has to cover rounding, and every
	// millisecond of it is a millisecond in which an impossible candidate is
	// treated as possible.
	recordDateSlack = 2 * time.Second
)

// recordEntry is what one record file says about itself near its top.
type recordEntry struct {
	// id is the runtime's identifier, taken from the record's own content
	// rather than from the file name — the same content-over-metadata rule
	// the dates follow. A file that carries no identifier is not a
	// candidate for anything.
	id string
	// title is the session name the runtime recorded for this conversation.
	title string
	// began is the first timestamp in the file; zero when it carries none,
	// which means the record cannot be dated and therefore cannot be ruled
	// out.
	began time.Time
}

// conversationKey identifies a session for memoisation purposes.
//
// Keyed on the PANE and its creation time, never on the session name: a rename
// changes the name while the title already written into the record is immutable
// history, so a name-keyed memo would lose an identifier it had already
// established at exactly the moment the name changed. Creation time is in the
// key because pane ids reset when the multiplexer restarts, and §5.4's rule
// that an id alone is not identity applies to our own cache first.
type conversationKey struct {
	pane    string
	created time.Time
}

// recordDirFor encodes a working directory the way the runtime's store does:
// every character that is not a letter or a digit becomes a separator.
//
// The encoding is lossy and this driver never inverts it — it only ever asks
// "which directory would this working directory be recorded in". If the
// runtime's own encoding ever drifts from this one, the failure is a lookup
// that finds no directory and says so, not a lookup that finds the wrong one.
func recordDirFor(cwd string) string {
	var b strings.Builder
	for _, r := range cwd {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			b.WriteRune(r)
		default:
			b.WriteRune('-')
		}
	}
	return b.String()
}

// conversationStore reads and remembers what record files say about themselves.
//
// Its own mutex, not the driver's: this does file I/O, and holding the lock
// that guards every session's observed state while reading a directory would
// make a listing wait on a disk.
type conversationStore struct {
	root string

	mu sync.Mutex
	// entries caches per FILE. The lines being read are written once, at the
	// top of a record, and never rewritten — so a cached entry cannot go
	// stale, only be deleted with its file.
	entries map[string]recordEntry
	// resolved caches per SESSION, and only successes. A failure must be
	// retried: the record for a session created moments ago has not been
	// written yet, and caching "no" would make that permanent.
	resolved map[conversationKey]*fleet.ConversationRef
}

func newConversationStore(root string) *conversationStore {
	return &conversationStore{
		root:     root,
		entries:  map[string]recordEntry{},
		resolved: map[conversationKey]*fleet.ConversationRef{},
	}
}

// lookup answers for one session, and returns nil only when there is nothing to
// look in — which is "nobody looked", not "nothing was found".
func (s *conversationStore) lookup(key conversationKey, cwd, name string, started time.Time) *fleet.ConversationRef {
	if s == nil || s.root == "" {
		return nil
	}
	s.mu.Lock()
	if hit, ok := s.resolved[key]; ok {
		s.mu.Unlock()
		return hit
	}
	s.mu.Unlock()

	ref := s.derive(cwd, name, started)
	if ref != nil && ref.Known {
		s.mu.Lock()
		s.resolved[key] = ref
		s.mu.Unlock()
	}
	return ref
}

func (s *conversationStore) derive(cwd, name string, started time.Time) *fleet.ConversationRef {
	dir := filepath.Join(s.root, recordDirFor(cwd))
	files, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return fleet.UnresolvedConversation(
				"the runtime has recorded nothing for this session's working directory")
		}
		// A directory that exists and cannot be read is a different fact
		// from one that does not exist, and flattening the two would tell a
		// caller a session has no record when in truth nobody could look.
		return fleet.UnresolvedConversation(
			fmt.Sprintf("the record directory for this session's working directory could not be read: %v", err))
	}

	var candidates []recordEntry
	for _, f := range files {
		if f.IsDir() || !strings.HasSuffix(f.Name(), ".jsonl") {
			continue
		}
		entry, ok := s.entryFor(filepath.Join(dir, f.Name()))
		if !ok || entry.title != name {
			continue
		}
		candidates = append(candidates, entry)
	}
	if len(candidates) == 0 {
		return fleet.UnresolvedConversation(
			"no record in this session's working directory carries this session's name; " +
				"a record not written yet, and one written under another name, read the same from here")
	}

	// Eliminate the impossible: a conversation runs inside the session, so a
	// record that existed before the session did belongs to something else.
	// A record carrying no timestamp cannot be dated and so cannot be ruled
	// out — being unable to date it is not evidence against it.
	var possible []recordEntry
	for _, c := range candidates {
		if c.began.IsZero() || !c.began.Before(started.Add(-recordDateSlack)) {
			possible = append(possible, c)
		}
	}
	ruledOut := len(candidates) - len(possible)

	switch {
	case len(possible) == 0:
		return fleet.UnresolvedConversation(fmt.Sprintf(
			"all %d records carrying this session's name were created before this session existed, "+
				"so none of them can be its own", len(candidates)))
	case len(possible) > 1:
		return fleet.UnresolvedConversation(fmt.Sprintf(
			"%d records carrying this session's name could each be this conversation; "+
				"choosing one of them would be a guess, and a guess is what this field exists to avoid",
			len(possible)))
	case ruledOut > 0:
		return fleet.ResolvedConversation(possible[0].id, fleet.ConversationDerived, fmt.Sprintf(
			"the only record carrying this session's name that the session could have written; "+
				"%d others were ruled out as older than the session itself", ruledOut))
	default:
		return fleet.ResolvedConversation(possible[0].id, fleet.ConversationDerived,
			"the only record in this session's working directory carrying the name this service gave the session")
	}
}

func (s *conversationStore) entryFor(path string) (recordEntry, bool) {
	s.mu.Lock()
	if hit, ok := s.entries[path]; ok {
		s.mu.Unlock()
		return hit, hit.id != ""
	}
	s.mu.Unlock()

	entry, ok := readRecordEntry(path)
	if !ok {
		// Remember the miss too: an unreadable or untitled file is re-read
		// on every listing otherwise, and its top lines are as immutable as
		// anyone else's.
		entry = recordEntry{}
	}
	s.mu.Lock()
	s.entries[path] = entry
	s.mu.Unlock()
	return entry, entry.id != ""
}

// readRecordEntry reads the top of one record file.
//
// It reads what the record SAYS — identifier, title, first timestamp — and
// nothing else. No message content is parsed, and none is kept: this whole path
// answers "which record" without ever answering "what is in it".
func readRecordEntry(path string) (recordEntry, bool) {
	f, err := os.Open(path)
	if err != nil {
		return recordEntry{}, false
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), recordLineLimit)

	var entry recordEntry
	for i := 0; i < recordScanLines && sc.Scan(); i++ {
		var line struct {
			Type        string `json:"type"`
			SessionID   string `json:"sessionId"`
			CustomTitle string `json:"customTitle"`
			Timestamp   string `json:"timestamp"`
		}
		if err := json.Unmarshal(sc.Bytes(), &line); err != nil {
			// A torn or half-written line near the top is not a reason to
			// discard the file: the runtime appends, and the reader may
			// have arrived mid-write.
			continue
		}
		if entry.id == "" && line.SessionID != "" {
			entry.id = line.SessionID
		}
		if entry.title == "" && line.Type == "custom-title" {
			entry.title = line.CustomTitle
		}
		if entry.began.IsZero() && line.Timestamp != "" {
			if ts, err := time.Parse(time.RFC3339Nano, line.Timestamp); err == nil {
				entry.began = ts
			}
		}
		if entry.id != "" && entry.title != "" && !entry.began.IsZero() {
			break
		}
	}
	return entry, entry.id != "" && entry.title != ""
}
