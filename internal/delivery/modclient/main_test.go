package modclient_test

import (
	"os"
	"testing"

	"github.com/godx-jp/colab-fleet/internal/delivery/modclient/modtest"
)

// TestMain turns this test binary into a fake delivery module when it is
// started by a wrapper written by modtest.Install; otherwise it runs the tests.
// That lets the real-process tests exercise the real launcher with nothing to
// build, offline, under the race detector.
func TestMain(m *testing.M) {
	modtest.MaybeServe()
	os.Exit(m.Run())
}
