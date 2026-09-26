package service

import (
	"context"

	fleet "github.com/godx-jp/colab-fleet"
	"github.com/godx-jp/colab-fleet/internal/driver"
)

// syncTitle is handleRename's second half (colab-fleet #222). Once the
// multiplexer-level id change has already succeeded and been announced
// (svc.publishRename), this brings the runtime's OWN title to the same
// string, on any LOCAL driver that keeps one apart from the id.
//
// A driver that does not implement driver.TitleSyncer reports
// fleet.TitleNotApplicable — this is the ONLY place that value is ever
// produced; a driver's own SyncTitle never returns it itself (every real
// implementation resolves to synced/pending/failed — see
// internal/drivers/tmux/titlesync.go).
//
// Never called for a peer-routed rename (see handleRename): a peer's own
// service already ran this exact step against its own driver, and whatever
// fleet.RenameAck.Title it answered with is forwarded by the remote
// driver's Rename verbatim — overwriting it here with not_applicable would
// be reporting on the WRONG machine's capability.
func syncTitle(ctx context.Context, d driver.Driver, req fleet.Request, ref fleet.SessionRef) *fleet.TitleSync {
	ts, ok := d.(driver.TitleSyncer)
	if !ok {
		t := fleet.TitleSyncNotApplicable("this session's runtime keeps no title of its own apart from its id")
		return &t
	}
	t := ts.SyncTitle(ctx, req, ref)
	return &t
}
