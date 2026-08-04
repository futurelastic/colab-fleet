package stub

import (
	"context"
	"errors"
	"testing"

	fleet "github.com/godx-jp/colab-fleet"
	"github.com/godx-jp/colab-fleet/internal/driver"
)

var testCaller = fleet.Request{Caller: fleet.Caller{Principal: "test:unit"}}

func TestDriver_CapabilitiesDefaultsToPositiveDeadline(t *testing.T) {
	d := &Driver{}
	caps := d.Capabilities()
	if err := caps.Validate(); err != nil {
		t.Fatalf("Capabilities().Validate() = %v, want nil — a stub must still declare a deadline (§4.4)", err)
	}
}

func TestDriver_CapabilitiesHonoursExplicitDeadline(t *testing.T) {
	d := &Driver{DeadlineMs: 42}
	if got := d.Capabilities().DeadlineMs; got != 42 {
		t.Fatalf("DeadlineMs = %d, want 42", got)
	}
}

func TestDriver_EveryOperationReturnsUnsupported(t *testing.T) {
	d := &Driver{}
	ctx := context.Background()
	ref := fleet.SessionRef{Machine: "m1", ID: "x"}

	if _, err := d.Create(ctx, testCaller, "key", fleet.SessionSpec{}); !errors.Is(err, driver.ErrUnsupported) {
		t.Errorf("Create err = %v, want ErrUnsupported", err)
	}
	if _, err := d.Send(ctx, testCaller, ref, "hi", driver.SendOptions{}); !errors.Is(err, driver.ErrUnsupported) {
		t.Errorf("Send err = %v, want ErrUnsupported", err)
	}
	if _, err := d.State(ctx, testCaller, ref); !errors.Is(err, driver.ErrUnsupported) {
		t.Errorf("State err = %v, want ErrUnsupported", err)
	}
	if _, err := d.Interrupt(ctx, testCaller, ref); !errors.Is(err, driver.ErrUnsupported) {
		t.Errorf("Interrupt err = %v, want ErrUnsupported", err)
	}
	if _, err := d.Close(ctx, testCaller, ref); !errors.Is(err, driver.ErrUnsupported) {
		t.Errorf("Close err = %v, want ErrUnsupported", err)
	}
	if _, err := d.List(ctx, testCaller, driver.ListFilter{}); !errors.Is(err, driver.ErrUnsupported) {
		t.Errorf("List err = %v, want ErrUnsupported", err)
	}
	if _, err := d.Subscribe(ctx, testCaller, driver.SubscribeFilter{}); !errors.Is(err, driver.ErrUnsupported) {
		t.Errorf("Subscribe err = %v, want ErrUnsupported", err)
	}
}
