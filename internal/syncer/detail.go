package syncer

import (
	"encoding/json"
	"fmt"

	"github.com/sl0wz3r/bunkarr/internal/jobs"
)

// Detail is the JSON detail of a sync, verify or retention item: what the planner decided (the
// dry-run preview shows it) and the execution state a resumed item continues from.
type Detail struct {
	// SourceID and Source are the file inside the source this item is about.
	SourceID int64  `json:"sourceId,omitempty"`
	Source   string `json:"source,omitempty"`
	// Size and MtimeNs are the source file's metadata at planning.
	Size    int64 `json:"size,omitempty"`
	MtimeNs int64 `json:"mtimeNs,omitempty"`
	// Group is the file's per-scan hardlink group.
	Group string `json:"group,omitempty"`
	// RecordID is the destination_files row the item changes (update, repair, relink, move,
	// retain, promote: the primary; verify; expire).
	RecordID int64 `json:"recordId,omitempty"`
	// OldSize is the recorded size an update (or a repair) replaces.
	OldSize int64 `json:"oldSize,omitempty"`
	// From is the old destination path of a move, or the primary's path of a promote.
	From string `json:"from,omitempty"`
	// Primary is, for a link, the path inside the source of the name that holds the content.
	Primary string `json:"primary,omitempty"`
	// TargetID is, for a promote, the dependent row that takes over the content.
	TargetID int64 `json:"targetId,omitempty"`
	// For is, for a promote, "retain" or "update": what happens to the primary afterwards.
	For string `json:"for,omitempty"`
	// Reason says why the planner chose the action: new, changed, repair, renamed, vanished,
	// hardlink, relink, adopt.
	Reason string `json:"reason,omitempty"`
	// Displace is set when the planner saw an unrecorded file at the path (it is displaced into
	// retention unless it can be adopted).
	Displace bool `json:"displace,omitempty"`
	// Check is, for a verify item, "stat" (a cheap check failed at planning) or "hash".
	Check string `json:"check,omitempty"`
	// RetainedPath is, for an expire item, the retained file.
	RetainedPath string `json:"retainedPath,omitempty"`

	// Execution state. Temp is recorded before the temp file is created (S7).
	Temp        string `json:"temp,omitempty"`
	TempDone    bool   `json:"tempDone,omitempty"`
	TempSize    int64  `json:"tempSize,omitempty"`
	TempMtimeNs int64  `json:"tempMtimeNs,omitempty"`
	TempHash    string `json:"tempHash,omitempty"`
	// Retained is where the old version (update, retain) goes, chosen and recorded before the
	// rename or link; RetainedSize/RetainedMtimeNs/RetainedHash describe that version.
	Retained        string `json:"retained,omitempty"`
	RetainedSize    int64  `json:"retainedSize,omitempty"`
	RetainedMtimeNs int64  `json:"retainedMtimeNs,omitempty"`
	RetainedHash    string `json:"retainedHash,omitempty"`
	RetainedReason  string `json:"retainedReason,omitempty"`
	// Displaced is where an unmanaged file in the way went.
	Displaced        string `json:"displaced,omitempty"`
	DisplacedSize    int64  `json:"displacedSize,omitempty"`
	DisplacedMtimeNs int64  `json:"displacedMtimeNs,omitempty"`
	// Outcome records what execution did when it differs from the action (adopted, copied
	// instead of linked, ...).
	Outcome string `json:"outcome,omitempty"`
}

// Outcomes (Detail.Outcome).
const (
	outcomeAdopted       = "adopted"
	outcomeFallbackCopy  = "copied"
	outcomeAlreadyDone   = "already done"
	outcomeLinkRecorded  = "recorded"
	outcomeRecordRemoved = "record removed"
)

func (d Detail) raw() json.RawMessage {
	b, _ := json.Marshal(d) // a struct of strings, numbers and bools always marshals
	return b
}

// parseDetail reads an item's detail.
func parseDetail(it jobs.Item) (Detail, error) {
	var d Detail
	if len(it.Detail) == 0 {
		return d, nil
	}
	if err := json.Unmarshal(it.Detail, &d); err != nil {
		return d, fmt.Errorf("item %d: detail: %w", it.ID, err)
	}
	return d, nil
}
