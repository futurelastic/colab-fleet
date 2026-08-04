package driver

import (
	"context"
	"errors"
	"testing"

	fleet "github.com/godx-jp/colab-fleet"
)

var testCaller = fleet.Request{Caller: fleet.Caller{Principal: "test:unit"}}

// fakeDriver exists only to prove the Driver interface's shape compiles and
// that a wrapped ErrUnsupported is still recognisable via errors.Is —
// exactly what a caller mapping driver errors to wire error kinds relies
// on (see internal/service's error mapping).
type fakeDriver struct{}

func (fakeDriver) Capabilities() fleet.DriverCapabilities {
	return fleet.DriverCapabilities{DeadlineMs: 1000}
}

func (fakeDriver) Create(ctx context.Context, req fleet.Request, key string, spec fleet.SessionSpec) (fleet.SessionRef, error) {
	return fleet.SessionRef{}, ErrUnsupported
}

func (fakeDriver) Send(ctx context.Context, req fleet.Request, ref fleet.SessionRef, text string, opts SendOptions) (fleet.DeliveryReceipt, error) {
	return fleet.DeliveryReceipt{}, ErrUnsupported
}

func (fakeDriver) Respond(ctx context.Context, req fleet.Request, ref fleet.SessionRef, resp fleet.Response) (fleet.DeliveryReceipt, error) {
	return fleet.DeliveryReceipt{}, ErrUnsupported
}

func (fakeDriver) State(ctx context.Context, req fleet.Request, ref fleet.SessionRef) (fleet.SessionState, error) {
	return fleet.SessionState{}, ErrUnsupported
}

func (fakeDriver) Interrupt(ctx context.Context, req fleet.Request, ref fleet.SessionRef) (fleet.Ack, error) {
	return fleet.Ack{}, ErrUnsupported
}

func (fakeDriver) Close(ctx context.Context, req fleet.Request, ref fleet.SessionRef) (fleet.Ack, error) {
	return fleet.Ack{}, ErrUnsupported
}

func (fakeDriver) List(ctx context.Context, req fleet.Request, filter ListFilter) (fleet.Collection[fleet.Session], error) {
	return fleet.Collection[fleet.Session]{}, ErrUnsupported
}

func (fakeDriver) Subscribe(ctx context.Context, req fleet.Request, filter SubscribeFilter) (EventStream, error) {
	return nil, ErrUnsupported
}

var _ Driver = fakeDriver{}

func TestErrUnsupported_WrapsThroughErrorsIs(t *testing.T) {
	var d Driver = fakeDriver{}
	_, err := d.State(context.Background(), testCaller, fleet.SessionRef{})
	if !errors.Is(err, ErrUnsupported) {
		t.Fatalf("State() err = %v, want ErrUnsupported", err)
	}
}

func TestListFilter_ZeroValueMeansNoFilter(t *testing.T) {
	var f ListFilter
	if f.Status != "" || f.Agent != "" || f.CwdPrefix != "" {
		t.Fatalf("zero-value ListFilter is not empty: %+v", f)
	}
}
