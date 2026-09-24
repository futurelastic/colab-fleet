package modclient_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/godx-jp/colab-fleet/internal/delivery/modclient"
)

const goodHello = `{"event":"hello","module":"m","protocol":1,"version":"1.2.3",` +
	`"ops":["prepare-launch","attach","send","confirm","close","health"],"reservedEnvPrefixes":["ACME_"]}`

func TestParseHello_Accepts(t *testing.T) {
	h, err := modclient.ParseHello([]byte(goodHello))
	if err != nil {
		t.Fatalf("a good hello was refused: %v", err)
	}
	if h.Module != "m" || h.Version != "1.2.3" || h.Protocol != 1 {
		t.Errorf("hello = %+v", h)
	}
	if len(h.ReservedEnvPrefixes) != 1 || h.ReservedEnvPrefixes[0] != "ACME_" {
		t.Errorf("prefixes = %v", h.ReservedEnvPrefixes)
	}
}

func TestParseHello_Refusals(t *testing.T) {
	cases := []struct {
		name, line, want string
	}{
		{"protocol", strings.Replace(goodHello, `"protocol":1`, `"protocol":2`, 1), "protocol 2"},
		{"no protocol", strings.Replace(goodHello, `"protocol":1,`, "", 1), "protocol 0"},
		{"missing op", strings.Replace(goodHello, `"health"`, `"other"`, 1), "health"},
		{"missing every op", `{"event":"hello","module":"m","protocol":1,"ops":[]}`, "prepare-launch"},
		{"invalid JSON", `this is not json{`, "not a valid hello"},
		{"empty line", ``, "not a valid hello"},
		{"a JSON string", `"hello"`, "not a valid hello"},
		{"wrong types", `{"event":"hello","module":"m","protocol":"1"}`, "not a valid hello"},
		{"wrong event", strings.Replace(goodHello, `"event":"hello"`, `"event":"response"`, 1), "event"},
		{"no event", strings.Replace(goodHello, `"event":"hello",`, "", 1), "event"},
		{"no module", strings.Replace(goodHello, `"module":"m",`, "", 1), "no module"},
		{"malformed prefix: space", strings.Replace(goodHello, `"ACME_"`, `"AC ME_"`, 1), "reserved environment prefix"},
		{"malformed prefix: empty", strings.Replace(goodHello, `"ACME_"`, `""`, 1), "reserved environment prefix"},
		{"malformed prefix: leading digit", strings.Replace(goodHello, `"ACME_"`, `"1ACME_"`, 1), "reserved environment prefix"},
		{"malformed prefix: equals", strings.Replace(goodHello, `"ACME_"`, `"ACME="`, 1), "reserved environment prefix"},
		{"prefix type", strings.Replace(goodHello, `["ACME_"]`, `[1]`, 1), "not a valid hello"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := modclient.ParseHello([]byte(tc.line))
			if err == nil {
				t.Fatal("a hello that must be refused was accepted")
			}
			var ref *modclient.HelloRefusal
			if !errors.As(err, &ref) {
				t.Fatalf("error %T is not a *HelloRefusal", err)
			}
			if !strings.Contains(ref.Reason, tc.want) {
				t.Errorf("reason %q does not mention %q", ref.Reason, tc.want)
			}
		})
	}
}

func TestParseHello_TooManyPrefixesRefused(t *testing.T) {
	var b strings.Builder
	b.WriteString(`{"event":"hello","module":"m","protocol":1,"ops":["prepare-launch","attach","send","confirm","close","health"],"reservedEnvPrefixes":[`)
	for i := 0; i < 65; i++ {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(`"P` + strings.Repeat("A", i%5) + string(rune('a'+i%26)) + `_"`)
	}
	b.WriteString(`]}`)
	if _, err := modclient.ParseHello([]byte(b.String())); err == nil {
		t.Fatal("a hello reserving 65 prefixes was accepted")
	}
}

func TestParseHello_IgnoresUnknownFields(t *testing.T) {
	line := `{"event":"hello","module":"m","protocol":1,"version":"1",` +
		`"ops":["prepare-launch","attach","send","confirm","close","health","extra-op"],` +
		`"reservedEnvPrefixes":[],"futureField":{"a":[1,2,3]},"anotherOne":true}`
	h, err := modclient.ParseHello([]byte(line))
	if err != nil {
		t.Fatalf("unknown fields must be ignored, got %v", err)
	}
	if h.Module != "m" {
		t.Errorf("hello = %+v", h)
	}
}

func TestParseHello_DedupesPrefixesAndCleansText(t *testing.T) {
	line := `{"event":"hello","module":"  m\u0007  ","protocol":1,"version":"v\u001b[31m",` +
		`"ops":["prepare-launch","attach","send","confirm","close","health"],"reservedEnvPrefixes":["A_","A_","B_"]}`
	h, err := modclient.ParseHello([]byte(line))
	if err != nil {
		t.Fatal(err)
	}
	if len(h.ReservedEnvPrefixes) != 2 {
		t.Errorf("prefixes = %v, want the duplicate dropped", h.ReservedEnvPrefixes)
	}
	if strings.ContainsAny(h.Module+h.Version, "\x07\x1b") {
		t.Errorf("control characters survived: %q %q", h.Module, h.Version)
	}
}

func TestValidReservedPrefix(t *testing.T) {
	ok := []string{"A", "_", "ACME_", "acme", "A1", "_x9", strings.Repeat("A", 64)}
	bad := []string{"", "1A", "A-B", "A B", "A=", "A.B", "é", strings.Repeat("A", 65)}
	for _, p := range ok {
		if !modclient.ValidReservedPrefix(p) {
			t.Errorf("%q should be a valid prefix", p)
		}
	}
	for _, p := range bad {
		if modclient.ValidReservedPrefix(p) {
			t.Errorf("%q should not be a valid prefix", p)
		}
	}
}

func TestError_IsCodeAndMessageIsQuoted(t *testing.T) {
	err := error(&modclient.Error{Code: "not-live", Message: "line one\nline two", Retryable: true})
	wrapped := errors.Join(errors.New("context"), err)
	if !modclient.IsCode(wrapped, "not-live") || modclient.IsCode(wrapped, "refused") {
		t.Error("IsCode must find the code through wrapping and only that code")
	}
	if modclient.IsCode(errors.New("x"), "not-live") {
		t.Error("IsCode matched a foreign error")
	}
	if strings.Contains(err.Error(), "\n") {
		t.Errorf("an untrusted message reached Error() unquoted: %q", err.Error())
	}
	if errors.Is(err, modclient.ErrNotSent) || errors.Is(err, modclient.ErrLost) {
		t.Error("a module's own refusal is a complete answer: neither not-sent nor lost")
	}
}
