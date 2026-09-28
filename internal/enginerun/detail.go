package enginerun

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"

	"github.com/sl0wz3r/bunkarr/internal/jobqueue"
	"github.com/sl0wz3r/bunkarr/internal/jobs"
	"github.com/sl0wz3r/bunkarr/internal/syncer"
)

// engineDetail is an item's detail on an engine destination: the planner's syncer.Detail (the
// dry-run preview and everything the planner decided) plus the execution state of the engines. It
// marshals flat, so syncer.ParseDetail reads the planner's part of it unchanged.
type engineDetail struct {
	syncer.Detail
	// HeadTail is the head/tail hash of the source file read before its upload, with the size and
	// mtime the file had then (§6.2 step 3, §7.3): stored with the record when the uploaded
	// version is that one.
	HeadTail        string `json:"engineHeadTail,omitempty"`
	HeadTailSize    int64  `json:"engineHeadTailSize,omitempty"`
	HeadTailMtimeNs int64  `json:"engineHeadTailMtimeNs,omitempty"`
	// Batch is the batch the item last ran in (restic, rclone).
	Batch int `json:"batch,omitempty"`
	// Snapshot and Group are a forget item's snapshot and snapshot group (§6.5 step 3; Reason
	// says why it is forgotten).
	Snapshot string `json:"snapshot,omitempty"`
	Group    string `json:"group,omitempty"`
	// Request marks a forget item that serves an engine_forget request.
	Request bool `json:"request,omitempty"`
	// OverrunWaits counts how often a file larger than the transfer window waited for a window's
	// opening (allowOverrun, §9.2): after one wait it starts at the next attempt whenever that
	// begins. The destination's waiting files (statWindowWaits) keep the count across the plans of
	// superseding jobs; this one covers a file the full list had no room for.
	OverrunWaits int `json:"overrunWaits,omitempty"`
}

// Expire kinds of engine retention items (engineDetail.Check).
const (
	checkExpireRow      = "row"
	checkExpireSnapshot = "snapshot"
	checkExpireObject   = "object"
	checkRepository     = "repository"
	checkSample         = "hash"
	checkListing        = "listing"
	checkVersion        = "version"
)

func parseItem(it jobs.Item) (engineDetail, error) {
	var d engineDetail
	if len(it.Detail) == 0 {
		return d, nil
	}
	if err := json.Unmarshal(it.Detail, &d); err != nil {
		return d, fmt.Errorf("item %d: detail: %w", it.ID, err)
	}
	return d, nil
}

func (d engineDetail) raw() json.RawMessage {
	b, _ := json.Marshal(d) // strings, numbers and bools always marshal
	return b
}

// setDetail persists an item's detail. It does not stop for a cancelled job: execution state must
// reach the database before the step it describes.
func (r *jobRun) setDetail(ctx context.Context, itemID int64, d engineDetail) error {
	return r.env.Items.SetDetail(context.WithoutCancel(ctx), itemID, d.raw())
}

// finish records an item's outcome.
func (r *jobRun) finish(ctx context.Context, itemID int64, status jobs.ItemStatus, bytes int64, msg string) error {
	return r.env.Items.Finish(context.WithoutCancel(ctx), itemID, status, bytes, msg)
}

// failItem fails an item with a warning line (the job's warnings count failed items).
func (r *jobRun) failItem(ctx context.Context, it jobs.Item, msg string) error {
	r.log(slog.LevelWarn, "item failed", "action", string(it.Action), "path", it.RelPath, "error", msg)
	return r.finish(ctx, it.ID, jobs.ItemFailed, 0, msg)
}

// pending returns every pending item of the job, in plan (id) order.
func (r *jobRun) pending(ctx context.Context) ([]jobs.Item, error) {
	var out []jobs.Item
	after := int64(0)
	for {
		page, err := r.env.Items.Pending(ctx, r.job.ID, after, pendingPage)
		if err != nil {
			return nil, err
		}
		out = append(out, page...)
		if len(page) < pendingPage {
			return out, nil
		}
		after = page[len(page)-1].ID
	}
}

// addItems persists items in pages; final marks the plan complete with the last page.
func (r *jobRun) addItems(ctx context.Context, items []jobs.Item, final bool) error {
	for len(items) > planPage {
		if err := r.env.Items.AddItems(ctx, r.job.ID, items[:planPage], false); err != nil {
			return err
		}
		items = items[planPage:]
	}
	return r.env.Items.AddItems(ctx, r.job.ID, items, final)
}

// counts returns the job's item counts by action and status.
func (r *jobRun) counts(ctx context.Context) (map[jobs.ItemAction]map[jobs.ItemStatus]jobs.ItemCount, error) {
	list, err := r.env.Items.Counts(context.WithoutCancel(ctx), r.job.ID)
	if err != nil {
		return nil, err
	}
	out := map[jobs.ItemAction]map[jobs.ItemStatus]jobs.ItemCount{}
	for _, c := range list {
		if out[c.Action] == nil {
			out[c.Action] = map[jobs.ItemStatus]jobs.ItemCount{}
		}
		out[c.Action][c.Status] = c
	}
	return out, nil
}

// itemLister is the job manager's full item listing (jobqueue.Store.ListItems): the stats of
// items by their detail need every item, not only the pending ones.
type itemLister interface {
	ListItems(ctx context.Context, jobID int64, q jobqueue.ItemQuery) (jobqueue.Page[jobs.Item], error)
}

// allItems returns every item of the job with action (all when ""), in plan order; ok is false
// when the item store cannot list them.
func (r *jobRun) allItems(ctx context.Context, action jobs.ItemAction) (items []jobs.Item, ok bool, err error) {
	l, ok := r.env.Items.(itemLister)
	if !ok {
		return nil, false, nil
	}
	for page := 1; ; page++ {
		p, err := l.ListItems(context.WithoutCancel(ctx), r.job.ID, jobqueue.ItemQuery{Action: action, Page: page, PageSize: jobqueue.MaxPageSize})
		if err != nil {
			return nil, true, err
		}
		items = append(items, p.Records...)
		if len(p.Records) < jobqueue.MaxPageSize || int64(len(items)) >= p.TotalRecords {
			return items, true, nil
		}
	}
}
