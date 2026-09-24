package tmux

import (
	"os"
	"testing"

	"github.com/godx-jp/colab-fleet/internal/delivery/modclient/modtest"
)

// TestMain lets this test binary stand in for a real external delivery module
// (#185): when it is started with a behaviour in its environment it serves the
// module protocol on stdio and exits; otherwise it runs the tests. That is what
// lets modulelane_e2e_test.go drive a REAL child process through the real
// launcher — pipes, a minimal environment, process groups — with nothing built.
func TestMain(m *testing.M) {
	modtest.MaybeServe()
	os.Exit(m.Run())
}
