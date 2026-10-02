package tmux

import (
	"encoding/json"
	"strings"
	"testing"

	fleet "github.com/futurelastic/muster"
)

// plainTabbedScreen is a numbered question dialog with NO preview pane under a
// tab bar painted by tabs, the way the runtime draws one (muster#242).
func plainTabbedScreen(header string) string {
	return strings.Join([]string{
		"  the tail of the agent's own prose, written just above the dialog",
		rule,
		header,
		"",
		"Which layout do you prefer?",
		"",
		"❯ 1. Option A",
		"  2. Option B",
		"  3. Type something",
		rule,
		"  4. Chat about this",
		"",
		"Enter to select · Tab/Arrow keys to navigate · Esc to cancel",
	}, "\n")
}

type wantTabs struct {
	tabs []fleet.PromptTab
	tab  int // -1: absent
}

func tabsOf(cur int, glyphs ...string) []fleet.PromptTab {
	var out []fleet.PromptTab
	for i, g := range glyphs {
		state := fleet.PromptTabPending
		if strings.HasPrefix(g, "☒") {
			state = fleet.PromptTabAnswered
		}
		if i == cur {
			state = fleet.PromptTabCurrent
		}
		out = append(out, fleet.PromptTab{Header: strings.TrimSpace(strings.TrimLeft(g, "☒☐")), State: state})
	}
	return out
}

// The bar a consumer needs for a progress card: every question tab with its
// state, and where the current one is — read off the same screen as the
// question, on both layouts, and absent whenever it would be a guess.
func TestPromptReportsTheTabBar(t *testing.T) {
	cases := []struct {
		name   string
		screen string
		want   *wantTabs // nil: no tabs published
	}{
		{"two questions, first current", plainTabbedScreen(previewTabBar(0, "☐ Layout", "☐ Theme", "✔ Submit")),
			&wantTabs{tabsOf(0, "☐ Layout", "☐ Theme"), 0}},
		{"two questions, second current", plainTabbedScreen(previewTabBar(1, "☒ Layout", "☐ Theme", "✔ Submit")),
			&wantTabs{tabsOf(1, "☒ Layout", "☐ Theme"), 1}},
		{"four questions, third current, earlier ones answered",
			plainTabbedScreen(previewTabBar(2, "☒ Layout", "☒ Theme", "☐ Density", "☐ Font", "✔ Submit")),
			&wantTabs{tabsOf(2, "☒ Layout", "☒ Theme", "☐ Density", "☐ Font"), 2}},
		{"an answered tab walked back to is current, not answered",
			plainTabbedScreen(previewTabBar(0, "☒ Layout", "☒ Theme", "✔ Submit")),
			&wantTabs{tabsOf(0, "☒ Layout", "☒ Theme"), 0}},
		{"the Submit tab is highlighted: the tabs, none current",
			plainTabbedScreen(previewTabBar(2, "☒ Layout", "☒ Theme", "✔ Submit")),
			&wantTabs{tabsOf(-1, "☒ Layout", "☒ Theme"), -1}},
		{"two tabs with the same header are told apart by position",
			plainTabbedScreen(previewTabBar(1, "☐ Q", "☐ Q", "✔ Submit")),
			&wantTabs{tabsOf(1, "☐ Q", "☐ Q"), 1}},
		{"a preview pane beside the list", twoQuestionPreview(1, "☐ Layout", "☐ Theme", "✔ Submit").String(),
			&wantTabs{tabsOf(0, "☐ Layout", "☐ Theme"), 0}},
		// Fail to absent.
		{"a single-question dialog's lone chip", plainTabbedScreen(previewChip("☐ Pick")), nil},
		{"a single-question bar", plainTabbedScreen(previewTabBar(0, "☐ Pick", "✔ Submit")), nil},
		{"no highlight to read: the current tab would be a guess",
			plainTabbedScreen("←  ☐ Layout  ☐ Theme  ✔ Submit  →"), nil},
		{"a tab with a glyph nobody measured",
			plainTabbedScreen(previewTabBar(0, "☐ Layout", "▣ Theme", "✔ Submit")), nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p, _ := parsePromptShape(newScreen(c.screen))
			if p == nil {
				t.Fatalf("not recognised as a prompt:\n%s", c.screen)
			}
			if c.want == nil {
				if p.Tabs != nil || p.Tab != nil {
					t.Fatalf("tabs = %+v tab = %v, want neither", p.Tabs, p.Tab)
				}
				return
			}
			if len(p.Tabs) != len(c.want.tabs) {
				t.Fatalf("tabs = %+v, want %+v", p.Tabs, c.want.tabs)
			}
			for i := range p.Tabs {
				if p.Tabs[i] != c.want.tabs[i] {
					t.Errorf("tabs[%d] = %+v, want %+v", i, p.Tabs[i], c.want.tabs[i])
				}
			}
			switch {
			case c.want.tab < 0 && p.Tab != nil:
				t.Errorf("tab = %d, want absent", *p.Tab)
			case c.want.tab >= 0 && (p.Tab == nil || *p.Tab != c.want.tab):
				t.Errorf("tab = %v, want %d", p.Tab, c.want.tab)
			}
		})
	}
}

// Index 0 is a position, not an absence: omitempty on a plain int would drop
// it, and the first tab is the one every dialog opens on.
func TestPromptTabZeroSurvivesTheWire(t *testing.T) {
	p, _ := parsePromptShape(newScreen(plainTabbedScreen(previewTabBar(0, "☐ Layout", "☐ Theme", "✔ Submit"))))
	if p == nil {
		t.Fatal("not a prompt")
	}
	b, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	var wire map[string]json.RawMessage
	if err := json.Unmarshal(b, &wire); err != nil {
		t.Fatal(err)
	}
	if string(wire["tab"]) != "0" {
		t.Errorf(`"tab" on the wire = %s, want 0 (body %s)`, wire["tab"], b)
	}
	var tabs []map[string]string
	if err := json.Unmarshal(wire["tabs"], &tabs); err != nil || len(tabs) != 2 ||
		tabs[0]["header"] != "Layout" || tabs[0]["state"] != "current" || tabs[1]["state"] != "pending" {
		t.Errorf(`"tabs" on the wire = %s (%v)`, wire["tabs"], err)
	}

	single, _ := parsePromptShape(newScreen(plainTabbedScreen(previewChip("☐ Pick"))))
	b, _ = json.Marshal(single)
	if strings.Contains(string(b), `"tab`) {
		t.Errorf("a single-question prompt carries tab fields: %s", b)
	}
}

// Reading the bar must not move what already depended on the screen: the
// question, the options and the nonce are the ones the bar-less reading gave.
func TestTabBarDoesNotChangeTheQuestionOrTheOptions(t *testing.T) {
	p, _ := parsePromptShape(newScreen(plainTabbedScreen(previewTabBar(1, "☒ Layout", "☐ Theme", "✔ Submit"))))
	if p == nil || p.Question != "Which layout do you prefer?" || len(p.Options) != 4 {
		t.Fatalf("prompt = %+v", p)
	}
}
