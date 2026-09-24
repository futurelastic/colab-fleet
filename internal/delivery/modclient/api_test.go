package modclient_test

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"testing"
	"time"

	"github.com/godx-jp/colab-fleet/internal/delivery/modclient"
)

// The integration code is written against this surface. These declarations are
// compile-time checks: a rename or a changed signature breaks the build of this
// test, not the integration far from here.
var (
	_ int      = modclient.ProtocolVersion
	_ []string = modclient.RequiredOps
	_ int      = modclient.MaxLineBytes

	_ func(modclient.Config) *modclient.Client                                                                     = modclient.New
	_ func(*modclient.Client)                                                                                      = (*modclient.Client).Start
	_ func(*modclient.Client)                                                                                      = (*modclient.Client).Stop
	_ func(*modclient.Client) modclient.Status                                                                     = (*modclient.Client).Status
	_ func(*modclient.Client) bool                                                                                 = (*modclient.Client).Usable
	_ func(*modclient.Client) uint64                                                                               = (*modclient.Client).Generation
	_ func(*modclient.Client) (modclient.Hello, bool)                                                              = (*modclient.Client).Hello
	_ func(*modclient.Client) (modclient.HealthResult, bool)                                                       = (*modclient.Client).LastHealth
	_ func(*modclient.Client) []string                                                                             = (*modclient.Client).ReservedEnvPrefixes
	_ func(*modclient.Client, context.Context, modclient.PrepareLaunchArgs) (modclient.PrepareLaunchResult, error) = (*modclient.Client).PrepareLaunch
	_ func(*modclient.Client, context.Context, modclient.AttachArgs) (modclient.AttachResult, error)               = (*modclient.Client).Attach
	_ func(*modclient.Client, context.Context, modclient.SendArgs) (modclient.SendResult, error)                   = (*modclient.Client).Send
	_ func(*modclient.Client, context.Context, modclient.ConfirmArgs) (modclient.ConfirmResult, error)             = (*modclient.Client).Confirm
	_ func(*modclient.Client, context.Context, modclient.CloseArgs) (modclient.CloseResult, error)                 = (*modclient.Client).CloseLane
	_ func(*modclient.Client, context.Context, modclient.HealthArgs) (modclient.HealthResult, error)               = (*modclient.Client).Health

	_ modclient.Launcher                                               = modclient.ExecLauncher
	_ func(string) ([]string, []string)                                = modclient.ParseModuleList
	_ func(string) string                                              = modclient.DefaultModulesDir
	_ func(string, []string) ([]modclient.Found, []modclient.Problem)  = modclient.Discover
	_ func(func(string) string, string, []string) ([]string, []string) = modclient.ChildEnv
	_ func(string) bool                                                = modclient.ValidReservedPrefix
	_ func([]byte) (modclient.Hello, error)                            = modclient.ParseHello
	_ func(*bufio.Reader) ([]byte, bool, error)                        = modclient.ReadLine
	_ func(error, string) bool                                         = modclient.IsCode
	_ error                                                            = modclient.ErrNotSent
	_ error                                                            = modclient.ErrLost
	_ modclient.State                                                  = modclient.StateStarting
	_ modclient.State                                                  = modclient.StateAvailable
	_ modclient.State                                                  = modclient.StateUnavailable
	_ modclient.State                                                  = modclient.StateDisabled
	_ error                                                            = (*modclient.Error)(nil)
	_ modclient.Proc                                                   = procShape{}
)

type procShape struct{}

func (procShape) Stdin() io.WriteCloser { return nil }
func (procShape) Stdout() io.Reader     { return nil }
func (procShape) Stderr() io.Reader     { return nil }
func (procShape) Wait() error           { return nil }
func (procShape) Kill()                 {}
func (procShape) Pid() int              { return 0 }

// TestAPI_ShapeOfTheTypesTheIntegrationUses builds each wire and config type by
// field name, so a renamed or retyped field fails here.
func TestAPI_ShapeOfTheTypesTheIntegrationUses(t *testing.T) {
	yes := true
	_ = modclient.Config{
		Name: "n", Path: "p", Env: []string{"A=b"}, Launcher: modclient.ExecLauncher,
		HelloTimeout: time.Second, BackoffMin: time.Second, BackoffMax: time.Second, BackoffResetAfter: time.Second,
		HealthInterval: time.Second, HealthRetryAfter: time.Second, ShutdownGrace: time.Second,
		Deadlines: modclient.Deadlines{Default: time.Second, Prepare: time.Second, AttachExtra: time.Second},
		Logf:      func(string, ...any) {}, Count: func(string) {}, OnReady: func(uint64) {}, OnChange: func() {},
	}
	_ = modclient.Status{
		Name: "n", State: modclient.StateAvailable, Suspect: true, Reason: "r", Hello: &modclient.Hello{}, Health: &modclient.HealthResult{},
		Since: time.Now(), Restarts: 1, Generation: 1,
	}
	_ = modclient.Hello{Event: "hello", Module: "m", Protocol: 1, Version: "v", Ops: []string{}, ReservedEnvPrefixes: []string{}}
	_ = modclient.PrepareLaunchArgs{ClaudeVersion: "v"}
	_ = modclient.PrepareLaunchResult{LaneKey: "k", ClaudeVersion: "v", Env: map[string]string{}}
	_ = modclient.AttachArgs{LaneKey: "k", PID: 1, Cwd: "/", TimeoutMs: 1}
	_ = modclient.AttachResult{LaneKey: "k", Live: true, State: "live", Reason: "r", SessionID: "s", ClaudeVersion: "v", PeerVerified: &yes, SinceMs: 1}
	_ = modclient.SendArgs{LaneKey: "k", Text: "t", AllowLeadingSlash: true, ConfirmWaitMs: 1}
	_ = modclient.SendResult{SendID: "s", Written: true, Transcript: modclient.Transcript{SessionID: "s", Path: "p", Offset: 1}, Confirm: &modclient.ConfirmResult{}}
	_ = modclient.ConfirmArgs{LaneKey: "k", SendID: "s", WaitMs: 1}
	_ = modclient.ConfirmResult{Verdict: "confirmed", Kind: "k", Enqueued: true, ElapsedMs: 1, Final: true}
	_ = modclient.CloseArgs{LaneKey: "k"}
	_ = modclient.CloseResult{Removed: true}
	_ = modclient.HealthArgs{LaneKey: "k"}
	_ = modclient.HealthResult{
		OK: true, Module: "m", Protocol: 1, Version: "v", Platform: "p", SupportedClaude: json.RawMessage(`[]`), PeerCheck: true,
		ReservedEnvPrefixes: []string{}, Lanes: json.RawMessage(`[]`), Counters: json.RawMessage(`{}`),
	}
	_ = modclient.Error{Code: "c", Message: "m", Retryable: true}
	_ = modclient.Found{Name: "n", Path: "p"}
	_ = modclient.Problem{Name: "n", Reason: "r"}

	// The wire spelling of every field is part of the protocol.
	for _, tc := range []struct {
		v    any
		want string
	}{
		{modclient.PrepareLaunchArgs{}, `{}`},
		{modclient.PrepareLaunchArgs{ClaudeVersion: "1"}, `{"claudeVersion":"1"}`},
		{modclient.AttachArgs{LaneKey: "k"}, `{"laneKey":"k"}`},
		{modclient.AttachArgs{LaneKey: "k", PID: 2, Cwd: "/w", TimeoutMs: 3}, `{"laneKey":"k","pid":2,"cwd":"/w","timeoutMs":3}`},
		{modclient.SendArgs{LaneKey: "k", Text: "t"}, `{"laneKey":"k","text":"t"}`},
		{modclient.SendArgs{LaneKey: "k", Text: "t", AllowLeadingSlash: true, ConfirmWaitMs: 4}, `{"laneKey":"k","text":"t","allowLeadingSlash":true,"confirmWaitMs":4}`},
		{modclient.ConfirmArgs{LaneKey: "k", SendID: "s"}, `{"laneKey":"k","sendId":"s"}`},
		{modclient.ConfirmArgs{LaneKey: "k", SendID: "s", WaitMs: 5}, `{"laneKey":"k","sendId":"s","waitMs":5}`},
		{modclient.CloseArgs{LaneKey: "k"}, `{"laneKey":"k"}`},
		{modclient.HealthArgs{}, `{}`},
		{modclient.HealthArgs{LaneKey: "k"}, `{"laneKey":"k"}`},
	} {
		got, err := json.Marshal(tc.v)
		if err != nil || string(got) != tc.want {
			t.Errorf("%T marshals to %s (%v), want %s", tc.v, got, err, tc.want)
		}
	}
	var at modclient.AttachResult
	if err := json.Unmarshal([]byte(`{"laneKey":"k","live":true,"state":"live","reason":"r","sessionId":"s","claudeVersion":"1","peerVerified":false,"sinceMs":7,"zz":1}`), &at); err != nil ||
		at.SessionID != "s" || at.PeerVerified == nil || *at.PeerVerified || at.SinceMs != 7 || at.ClaudeVersion != "1" {
		t.Errorf("attach result decoding: %+v %v", at, err)
	}
	var sd modclient.SendResult
	if err := json.Unmarshal([]byte(`{"sendId":"s","written":true,"transcript":{"sessionId":"x","path":"/p","offset":9},"confirm":{"verdict":"queued","kind":"k","enqueued":true,"elapsedMs":3,"final":false}}`), &sd); err != nil ||
		sd.Transcript.Path != "/p" || sd.Transcript.Offset != 9 || sd.Confirm == nil || sd.Confirm.Verdict != "queued" || sd.Confirm.Final {
		t.Errorf("send result decoding: %+v %v", sd, err)
	}
}
