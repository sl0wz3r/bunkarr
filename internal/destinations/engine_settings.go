package destinations

import (
	"encoding/json"
	"fmt"
	"regexp"

	"github.com/sl0wz3r/bunkarr/internal/engines"
)

// Engines of a destination row (destinations.engine). Filecopy is EngineFilecopy.
const (
	EngineRestic = string(engines.Restic)
	EngineRclone = string(engines.Rclone)
)

// ResticSettings are a restic destination's settings (phase4.md §6.1-§6.6, §12).
type ResticSettings struct {
	// PackSizeMiB is restic's --pack-size (4-128; default 64 for remote kinds, 16 for local).
	PackSizeMiB int `json:"packSizeMiB"`
	// BatchBytes and BatchFiles cut a sync's content items into batches (§6.2 step 3; defaults
	// 64 GiB and 20 000).
	BatchBytes int64 `json:"batchBytes"`
	BatchFiles int   `json:"batchFiles"`
	// PruneEveryDays is how often the retention job prunes (1-90, default 7).
	PruneEveryDays int `json:"pruneEveryDays"`
	// PruneMaxUnused is prune's --max-unused: a percentage ("10%", the default), a size with a
	// unit ("5G") or "unlimited".
	PruneMaxUnused string `json:"pruneMaxUnused"`
}

// RcloneSettings are an rclone destination's settings (phase4.md §7.3, §12).
type RcloneSettings struct {
	// BatchFiles and BatchBytes cut the copy items into batches (defaults 1000 and 64 GiB).
	BatchFiles int   `json:"batchFiles"`
	BatchBytes int64 `json:"batchBytes"`
}

// Engine setting defaults and limits (phase4.md §6.1, §6.5, §6.6, §7.3, §7.6, §9.3).
const (
	DefaultTransfers         = 4
	MaxTransfers             = 32
	DefaultResticSampleBytes = 4 << 30
	DefaultRcloneSampleBytes = 16 << 30
	DefaultPackSizeRemoteMiB = 64
	DefaultPackSizeLocalMiB  = 16
	DefaultResticBatchBytes  = 64 << 30
	DefaultResticBatchFiles  = 20000
	DefaultPruneEveryDays    = 7
	DefaultPruneMaxUnused    = "10%"
	DefaultRcloneBatchFiles  = 1000
	DefaultRcloneBatchBytes  = 64 << 30
	DefaultSnapshotDaily     = 7
	DefaultSnapshotWeekly    = 4
	DefaultSnapshotMonthly   = 6
	DefaultSnapshotYearly    = 0

	minSampleBytes = 1 << 20
	maxSampleBytes = 1 << 50
	minBatchBytes  = 1 << 20
	maxBatchBytes  = 1 << 50
)

// maxUnusedPattern is prune's --max-unused: a percentage (0-100, up to two decimals), a size
// with an optional binary unit, or unlimited. Nothing else reaches restic's argv.
var maxUnusedPattern = regexp.MustCompile(`^(?:(?:100|[0-9]{1,2})(?:\.[0-9]{1,2})?%|[0-9]{1,12}[kKmMgGtT]?|unlimited)$`)

// NormalizeFor is Normalize for a destination of engine and kind: filecopy refuses the engine
// fields (so its JSON stays that of Phases 1-3); restic and rclone fill their defaults, validate
// the ranges and refuse the other engine's fields. An invalid value returns a ValidationError.
func (s Settings) NormalizeFor(engine string, kind engines.DestKind) (Settings, error) {
	s, err := s.Normalize()
	if err != nil {
		return Settings{}, err
	}
	switch engine {
	case "", EngineFilecopy:
		switch {
		case s.Transfers != 0:
			return Settings{}, ValidationError("settings.transfers applies to restic and rclone destinations only")
		case s.Verify.SampleMaxBytes != 0:
			return Settings{}, ValidationError("settings.verify.sampleMaxBytes applies to restic and rclone destinations only")
		case s.Restic != nil:
			return Settings{}, ValidationError("settings.restic applies to restic destinations only")
		case s.Rclone != nil:
			return Settings{}, ValidationError("settings.rclone applies to rclone destinations only")
		}
		return s, nil
	case EngineRestic, EngineRclone:
	default:
		return Settings{}, ValidationError(fmt.Sprintf("engine %q: use filecopy, restic or rclone", engine))
	}
	if s.Transfers == 0 {
		s.Transfers = DefaultTransfers
	}
	if s.Transfers < 1 || s.Transfers > MaxTransfers {
		return Settings{}, ValidationError(fmt.Sprintf("settings.transfers %d is out of range (1-%d)", s.Transfers, MaxTransfers))
	}
	if engine == EngineRestic {
		if s.Rclone != nil {
			return Settings{}, ValidationError("settings.rclone applies to rclone destinations only")
		}
		if s.Verify.SampleMaxBytes == 0 {
			s.Verify.SampleMaxBytes = DefaultResticSampleBytes
		}
		r, err := s.Restic.normalize(kind)
		if err != nil {
			return Settings{}, err
		}
		s.Restic = &r
	} else {
		if s.Restic != nil {
			return Settings{}, ValidationError("settings.restic applies to restic destinations only")
		}
		if s.Verify.SampleMaxBytes == 0 {
			s.Verify.SampleMaxBytes = DefaultRcloneSampleBytes
		}
		r, err := s.Rclone.normalize()
		if err != nil {
			return Settings{}, err
		}
		s.Rclone = &r
	}
	if s.Verify.SampleMaxBytes < minSampleBytes || s.Verify.SampleMaxBytes > maxSampleBytes {
		return Settings{}, ValidationError(fmt.Sprintf("settings.verify.sampleMaxBytes %d is out of range (%d-%d)", s.Verify.SampleMaxBytes,
			int64(minSampleBytes), int64(maxSampleBytes)))
	}
	return s, nil
}

// normalize fills the defaults of r (nil: all defaults) for a repository of kind.
func (r *ResticSettings) normalize(kind engines.DestKind) (ResticSettings, error) {
	var v ResticSettings
	if r != nil {
		v = *r
	}
	if v.PackSizeMiB == 0 {
		v.PackSizeMiB = DefaultPackSizeRemoteMiB
		if !kind.Remote() {
			v.PackSizeMiB = DefaultPackSizeLocalMiB
		}
	}
	if v.PackSizeMiB < 4 || v.PackSizeMiB > 128 {
		return ResticSettings{}, ValidationError(fmt.Sprintf("settings.restic.packSizeMiB %d is out of range (4-128)", v.PackSizeMiB))
	}
	if v.BatchBytes == 0 {
		v.BatchBytes = DefaultResticBatchBytes
	}
	if v.BatchBytes < minBatchBytes || v.BatchBytes > maxBatchBytes {
		return ResticSettings{}, ValidationError(fmt.Sprintf("settings.restic.batchBytes %d is out of range (%d-%d)", v.BatchBytes,
			int64(minBatchBytes), int64(maxBatchBytes)))
	}
	if v.BatchFiles == 0 {
		v.BatchFiles = DefaultResticBatchFiles
	}
	if v.BatchFiles < 1 || v.BatchFiles > 1_000_000 {
		return ResticSettings{}, ValidationError(fmt.Sprintf("settings.restic.batchFiles %d is out of range (1-1000000)", v.BatchFiles))
	}
	if v.PruneEveryDays == 0 {
		v.PruneEveryDays = DefaultPruneEveryDays
	}
	if v.PruneEveryDays < 1 || v.PruneEveryDays > 90 {
		return ResticSettings{}, ValidationError(fmt.Sprintf("settings.restic.pruneEveryDays %d is out of range (1-90)", v.PruneEveryDays))
	}
	if v.PruneMaxUnused == "" {
		v.PruneMaxUnused = DefaultPruneMaxUnused
	}
	if !maxUnusedPattern.MatchString(v.PruneMaxUnused) {
		return ResticSettings{}, ValidationError(fmt.Sprintf("settings.restic.pruneMaxUnused %q: use a percentage (10%%), a size (5G) or unlimited",
			v.PruneMaxUnused))
	}
	return v, nil
}

// normalize fills the defaults of r (nil: all defaults).
func (r *RcloneSettings) normalize() (RcloneSettings, error) {
	var v RcloneSettings
	if r != nil {
		v = *r
	}
	if v.BatchFiles == 0 {
		v.BatchFiles = DefaultRcloneBatchFiles
	}
	if v.BatchFiles < 1 || v.BatchFiles > 100_000 {
		return RcloneSettings{}, ValidationError(fmt.Sprintf("settings.rclone.batchFiles %d is out of range (1-100000)", v.BatchFiles))
	}
	if v.BatchBytes == 0 {
		v.BatchBytes = DefaultRcloneBatchBytes
	}
	if v.BatchBytes < minBatchBytes || v.BatchBytes > maxBatchBytes {
		return RcloneSettings{}, ValidationError(fmt.Sprintf("settings.rclone.batchBytes %d is out of range (%d-%d)", v.BatchBytes,
			int64(minBatchBytes), int64(maxBatchBytes)))
	}
	return v, nil
}

// SnapshotKeep is a restic destination's snapshot retention as plain numbers (phase4.md §6.5).
type SnapshotKeep struct {
	Daily, Weekly, Monthly, Yearly int
}

// SnapshotKeep returns the snapshot retention, with the defaults for the fields that are not set.
func (r Retention) SnapshotKeep() SnapshotKeep {
	get := func(p *int, def int) int {
		if p == nil {
			return def
		}
		return *p
	}
	return SnapshotKeep{Daily: get(r.SnapshotDaily, DefaultSnapshotDaily), Weekly: get(r.SnapshotWeekly, DefaultSnapshotWeekly),
		Monthly: get(r.SnapshotMonthly, DefaultSnapshotMonthly), Yearly: get(r.SnapshotYearly, DefaultSnapshotYearly)}
}

// NormalizeFor is Normalize for a destination of engine: restic fills and checks the snapshot
// retention (daily 0-3650, weekly 0-520, monthly 0-120, yearly 0-100); filecopy and rclone refuse
// it. An invalid value returns a ValidationError.
func (r Retention) NormalizeFor(engine string) (Retention, error) {
	r, err := r.Normalize()
	if err != nil {
		return Retention{}, err
	}
	fields := []struct {
		name     string
		p        **int
		def, max int
	}{
		{"snapshotDaily", &r.SnapshotDaily, DefaultSnapshotDaily, 3650},
		{"snapshotWeekly", &r.SnapshotWeekly, DefaultSnapshotWeekly, 520},
		{"snapshotMonthly", &r.SnapshotMonthly, DefaultSnapshotMonthly, 120},
		{"snapshotYearly", &r.SnapshotYearly, DefaultSnapshotYearly, 100},
	}
	if engine != EngineRestic {
		for _, f := range fields {
			if *f.p != nil {
				return Retention{}, ValidationError(fmt.Sprintf("retention.%s applies to restic destinations only", f.name))
			}
		}
		return r, nil
	}
	for _, f := range fields {
		v := f.def
		if *f.p != nil {
			v = **f.p
		}
		if v < 0 || v > f.max {
			return Retention{}, ValidationError(fmt.Sprintf("retention.%s %d is out of range (0-%d)", f.name, v, f.max))
		}
		*f.p = &v
	}
	return r, nil
}

// ParseSettingsFor reads a destination row's settings JSON with the defaults of its engine and
// kind (ParseSettings for filecopy).
func ParseSettingsFor(raw, engine string, kind engines.DestKind) (Settings, error) {
	var s Settings
	if raw != "" {
		if err := json.Unmarshal([]byte(raw), &s); err != nil {
			return Settings{}, fmt.Errorf("parse settings: %w", err)
		}
	}
	return s.NormalizeFor(engine, kind)
}

// ParseRetentionFor reads a destination row's retention JSON with the defaults of its engine
// (ParseRetention for filecopy).
func ParseRetentionFor(raw, engine string) (Retention, error) {
	var r Retention
	if raw != "" {
		var err error
		if r, err = decodeRetention([]byte(raw), false); err != nil {
			return Retention{}, fmt.Errorf("parse retention: %w", err)
		}
	}
	return r.NormalizeFor(engine)
}
