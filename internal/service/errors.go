package service

import (
	"context"
	"errors"

	"github.com/godx-jp/colab-fleet/internal/driver"
)

func isUnsupported(err error) bool {
	return errors.Is(err, driver.ErrUnsupported)
}

func isDeadlineExceeded(err error) bool {
	return errors.Is(err, context.DeadlineExceeded)
}
