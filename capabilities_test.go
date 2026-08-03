package fleet

import (
	"errors"
	"testing"
)

func TestDriverCapabilities_Validate_RejectsZeroDeadline(t *testing.T) {
	c := DriverCapabilities{DeadlineMs: 0}
	if err := c.Validate(); !errors.Is(err, ErrNoDeadline) {
		t.Fatalf("Validate() = %v, want ErrNoDeadline", err)
	}
}

func TestDriverCapabilities_Validate_RejectsNegativeDeadline(t *testing.T) {
	c := DriverCapabilities{DeadlineMs: -1}
	if err := c.Validate(); !errors.Is(err, ErrNoDeadline) {
		t.Fatalf("Validate() = %v, want ErrNoDeadline", err)
	}
}

func TestDriverCapabilities_Validate_AcceptsPositiveDeadline(t *testing.T) {
	c := DriverCapabilities{DeadlineMs: 3000}
	if err := c.Validate(); err != nil {
		t.Fatalf("Validate() = %v, want nil", err)
	}
}
