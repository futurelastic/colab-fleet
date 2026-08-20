package main

import (
	"encoding/json"
	"fmt"
	"os"

	fleet "github.com/godx-jp/colab-fleet"
	"github.com/godx-jp/colab-fleet/internal/service"
)

// fileConfig is the on-disk form of what a fleet member needs to know about
// everyone else (§6, §7.2).
//
// It is a file rather than more environment variables because a principal
// table is a table: names, secrets and per-verb grants do not survive being
// flattened into a delimiter-separated string without becoming unreadable and
// unreviewable. Peers are statically configured (§7.2), so a file that an
// operator edits and can diff is the right shape.
//
// Nothing here has a default. An absent file means single-token mode, which is
// the older behaviour and still reasonable for one machine; a present file is
// authoritative.
type fileConfig struct {
	// Principals may call THIS machine, and with what permission.
	Principals []struct {
		Name   string   `json:"name"`
		Token  string   `json:"token"`
		Grants []string `json:"grants"`
	} `json:"principals"`

	// Peers are machines this one may call, each with the credential THIS
	// machine presents there. Note the asymmetry: a peer's entry here is
	// our identity on them, not theirs on us. Those are different secrets
	// and conflating them is how a fleet ends up with one shared token
	// again by accident.
	Peers []struct {
		Machine string `json:"machine"`
		URL     string `json:"url"`
		Token   string `json:"token"`
	} `json:"peers"`

	// TrustRoots are the directories under which #47's trust seeding runs:
	// a session under one of these never meets the runtime's folder-trust
	// question, whoever started it. A list alongside Principals for the
	// same reason Peers is one — an operator edits and diffs a file, not a
	// delimiter-separated string — and, like the rest of this file, it
	// names real paths and so lives only in a config an operator points
	// FLEET_CONFIG at, never in this repository. See
	// internal/trustseed.Seeder for the mechanism and its scope guards.
	TrustRoots []string `json:"trustRoots,omitempty"`
}

func loadConfig(path string) (*fileConfig, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var c fileConfig
	dec := json.NewDecoder(newTrimReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&c); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if len(c.Principals) == 0 {
		return nil, fmt.Errorf("%s: no principals; a config with nobody in it "+
			"would deny every request, which is more likely a mistake than an intent", path)
	}
	for _, p := range c.Principals {
		if p.Name == "" || p.Token == "" {
			return nil, fmt.Errorf("%s: a principal needs both a name and a token", path)
		}
	}
	return &c, nil
}

func (c *fileConfig) principals() ([]service.Principal, error) {
	out := make([]service.Principal, 0, len(c.Principals))
	for _, p := range c.Principals {
		var grants []service.Grant
		for _, g := range p.Grants {
			gr := service.Grant(g)
			if !service.ValidGrant(gr) {
				return nil, fmt.Errorf("principal %q: unknown grant %q", p.Name, g)
			}
			grants = append(grants, gr)
		}
		out = append(out, service.Principal{Name: p.Name, Token: p.Token, Grants: grants})
	}
	return out, nil
}

func (c *fileConfig) peerFor(machine fleet.MachineId) (url, token string, ok bool) {
	for _, p := range c.Peers {
		if fleet.MachineId(p.Machine) == machine {
			return p.URL, p.Token, true
		}
	}
	return "", "", false
}

// newTrimReader exists so a config file may carry a leading byte-order mark or
// stray whitespace without failing to parse, which is the kind of thing that
// costs an operator twenty minutes at an unhelpful hour.
func newTrimReader(b []byte) *trimReader {
	i := 0
	for i < len(b) && (b[i] == ' ' || b[i] == '\n' || b[i] == '\r' || b[i] == '\t') {
		i++
	}
	if len(b) >= i+3 && b[i] == 0xEF && b[i+1] == 0xBB && b[i+2] == 0xBF {
		i += 3
	}
	return &trimReader{b: b[i:]}
}

type trimReader struct {
	b []byte
	i int
}

func (r *trimReader) Read(p []byte) (int, error) {
	if r.i >= len(r.b) {
		return 0, fmt.Errorf("EOF")
	}
	n := copy(p, r.b[r.i:])
	r.i += n
	return n, nil
}
