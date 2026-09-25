package jobqueue

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/sl0wz3r/bunkarr/internal/jobs"
	"github.com/sl0wz3r/bunkarr/internal/logging"
)

func newJob(t *testing.T, st *Store) jobs.Job {
	t.Helper()
	j, _, err := st.CreateJob(context.Background(), jobs.Spec{Type: jobs.TypeSync, Params: jobs.Params{DestinationID: 1}})
	if err != nil {
		t.Fatal(err)
	}
	return j
}

func planned(t *testing.T, st *Store, id int64) bool {
	t.Helper()
	p, err := st.Planned(context.Background(), id)
	if err != nil {
		t.Fatalf("Planned: %v", err)
	}
	return p
}

func TestItemStorePlanLifecycle(t *testing.T) {
	ctx := context.Background()
	st := NewStore(openDB(t))
	job := newJob(t, st)

	if planned(t, st, job.ID) {
		t.Fatal("new job is planned")
	}
	batch := func(prefix string, n int) []jobs.Item {
		out := make([]jobs.Item, n)
		for i := range out {
			out[i] = jobs.Item{RelPath: fmt.Sprintf("%s/%d.mkv", prefix, i), Action: jobs.ActionCopy, Bytes: 10, FileID: int64(i + 1)}
		}
		return out
	}

	// Killed while planning: items exist but the plan is not complete.
	if err := st.AddItems(ctx, job.ID, batch("a", 3), false); err != nil {
		t.Fatal(err)
	}
	if planned(t, st, job.ID) {
		t.Fatal("non-final batch marked the plan complete")
	}
	// The resumed runner deletes the partial plan and plans again.
	if err := st.DeleteItems(ctx, job.ID); err != nil {
		t.Fatal(err)
	}
	if p, _ := st.Pending(ctx, job.ID, 0, 0); len(p) != 0 {
		t.Fatalf("items left after DeleteItems: %d", len(p))
	}
	if err := st.AddItems(ctx, job.ID, batch("b", 3), false); err != nil {
		t.Fatal(err)
	}
	if err := st.AddItems(ctx, job.ID, batch("c", 2), true); err != nil {
		t.Fatal(err)
	}
	if !planned(t, st, job.ID) {
		t.Fatal("final batch did not mark the plan complete")
	}
	// An empty final batch also completes a plan; DeleteItems clears planned_at.
	if err := st.DeleteItems(ctx, job.ID); err != nil {
		t.Fatal(err)
	}
	if planned(t, st, job.ID) {
		t.Fatal("DeleteItems did not clear planned_at")
	}
	if err := st.AddItems(ctx, job.ID, batch("d", 5), false); err != nil {
		t.Fatal(err)
	}
	if err := st.AddItems(ctx, job.ID, nil, true); err != nil {
		t.Fatal(err)
	}
	if !planned(t, st, job.ID) {
		t.Fatal("empty final batch did not mark the plan complete")
	}

	// Pending pages through in id order.
	first, err := st.Pending(ctx, job.ID, 0, 2)
	if err != nil || len(first) != 2 || first[0].RelPath != "d/0.mkv" || first[1].RelPath != "d/1.mkv" {
		t.Fatalf("Pending page 1 = %+v, %v", first, err)
	}
	if first[0].Status != jobs.ItemPending || first[0].JobID != job.ID || first[0].FileID != 1 || string(first[0].Detail) != "{}" {
		t.Fatalf("item = %+v", first[0])
	}
	rest, _ := st.Pending(ctx, job.ID, first[1].ID, 0)
	if len(rest) != 3 || rest[0].RelPath != "d/2.mkv" {
		t.Fatalf("Pending after %d = %+v", first[1].ID, rest)
	}

	// SetDetail before the effect (e.g. the temp path), then Finish.
	detail := json.RawMessage(`{"temp":"d/.bunkarr-tmp-0.mkv-x1"}`)
	if err := st.SetDetail(ctx, first[0].ID, detail); err != nil {
		t.Fatal(err)
	}
	if err := st.SetDetail(ctx, first[0].ID, json.RawMessage(`{broken`)); !isValidation(err) {
		t.Fatalf("invalid detail accepted: %v", err)
	}
	const secret = "items-test-secret-value-42"
	logging.RegisterSecret(secret)
	if err := st.Finish(ctx, first[0].ID, jobs.ItemDone, 11, ""); err != nil {
		t.Fatal(err)
	}
	if err := st.Finish(ctx, first[1].ID, jobs.ItemFailed, 0, "read failed near "+secret); err != nil {
		t.Fatal(err)
	}
	if err := st.Finish(ctx, rest[0].ID, jobs.ItemHeld, 10, ""); err != nil {
		t.Fatal(err)
	}
	if err := st.Finish(ctx, rest[1].ID, jobs.ItemPending, 0, ""); !isValidation(err) {
		t.Fatalf("Finish(pending) = %v; want ValidationError", err)
	}
	if err := st.Finish(ctx, 999999, jobs.ItemDone, 0, ""); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Finish(unknown) = %v", err)
	}
	if err := st.SetDetail(ctx, 999999, nil); !errors.Is(err, ErrNotFound) {
		t.Fatalf("SetDetail(unknown) = %v", err)
	}

	pending, _ := st.Pending(ctx, job.ID, 0, 0)
	if len(pending) != 2 {
		t.Fatalf("pending after finishing 3 of 5 = %d", len(pending))
	}

	page, err := st.ListItems(ctx, job.ID, ItemQuery{Status: jobs.ItemFailed})
	if err != nil || page.TotalRecords != 1 || len(page.Records) != 1 {
		t.Fatalf("ListItems(failed) = %+v, %v", page, err)
	}
	if strings.Contains(page.Records[0].Error, secret) || !strings.Contains(page.Records[0].Error, logging.Redacted) {
		t.Fatalf("item error not redacted: %q", page.Records[0].Error)
	}
	page, _ = st.ListItems(ctx, job.ID, ItemQuery{Status: jobs.ItemDone})
	if len(page.Records) != 1 || page.Records[0].Bytes != 11 || string(page.Records[0].Detail) != string(detail) {
		t.Fatalf("done item = %+v", page.Records)
	}

	counts, err := st.Counts(ctx, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	want := []jobs.ItemCount{
		{Action: jobs.ActionCopy, Status: jobs.ItemDone, Files: 1, Bytes: 11},
		{Action: jobs.ActionCopy, Status: jobs.ItemFailed, Files: 1, Bytes: 0},
		{Action: jobs.ActionCopy, Status: jobs.ItemHeld, Files: 1, Bytes: 10},
		{Action: jobs.ActionCopy, Status: jobs.ItemPending, Files: 2, Bytes: 20},
	}
	if !reflect.DeepEqual(counts, want) {
		t.Fatalf("Counts = %+v\nwant %+v", counts, want)
	}
}

func TestItemStoreValidation(t *testing.T) {
	ctx := context.Background()
	st := NewStore(openDB(t))
	job := newJob(t, st)

	cases := []struct {
		name string
		item jobs.Item
	}{
		{"unknown action", jobs.Item{RelPath: "x", Action: "delete"}},
		{"unknown status", jobs.Item{RelPath: "x", Action: jobs.ActionCopy, Status: "running"}},
		{"invalid detail", jobs.Item{RelPath: "x", Action: jobs.ActionCopy, Detail: json.RawMessage("{")}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ok := jobs.Item{RelPath: "ok", Action: jobs.ActionCopy}
			if err := st.AddItems(ctx, job.ID, []jobs.Item{ok, tc.item}, true); !isValidation(err) {
				t.Fatalf("AddItems = %v; want ValidationError", err)
			}
			// Nothing of the batch was written and the plan is not complete.
			if p, _ := st.Pending(ctx, job.ID, 0, 0); len(p) != 0 || planned(t, st, job.ID) {
				t.Fatalf("a rejected batch left %d items (planned %v)", len(p), planned(t, st, job.ID))
			}
		})
	}
	if err := st.AddItems(ctx, 424242, []jobs.Item{{RelPath: "x", Action: jobs.ActionCopy}}, true); !errors.Is(err, ErrNotFound) {
		t.Fatalf("AddItems(unknown job) = %v", err)
	}
	if _, err := st.Planned(ctx, 424242); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Planned(unknown job) = %v", err)
	}
	if err := st.DeleteItems(ctx, 424242); !errors.Is(err, ErrNotFound) {
		t.Fatalf("DeleteItems(unknown job) = %v", err)
	}
	if _, err := st.ListItems(ctx, 424242, ItemQuery{}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("ListItems(unknown job) = %v", err)
	}
	if _, err := st.ListItems(ctx, job.ID, ItemQuery{Action: "nope"}); !isValidation(err) {
		t.Fatalf("ListItems(bad action) = %v", err)
	}
}

func TestListItemsPaging(t *testing.T) {
	ctx := context.Background()
	st := NewStore(openDB(t))
	job := newJob(t, st)
	var items []jobs.Item
	for i := range 7 {
		a := jobs.ActionCopy
		if i%2 == 1 {
			a = jobs.ActionRetain
		}
		items = append(items, jobs.Item{RelPath: fmt.Sprintf("f%d", i), Action: a})
	}
	if err := st.AddItems(ctx, job.ID, items, true); err != nil {
		t.Fatal(err)
	}
	p, err := st.ListItems(ctx, job.ID, ItemQuery{Action: jobs.ActionCopy, Page: 2, PageSize: 3})
	if err != nil {
		t.Fatal(err)
	}
	if p.TotalRecords != 4 || len(p.Records) != 1 || p.Records[0].RelPath != "f6" {
		t.Fatalf("page = %+v", p)
	}
}
