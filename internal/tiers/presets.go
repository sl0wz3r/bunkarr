package tiers

import "encoding/json"

// Preset is a rule set the editor can load (GET /tiers/presets). A preset is never applied
// directly: the user loads it into the editor, previews it and saves it (design §8.4, D1).
type Preset struct {
	ID          string      `json:"id"`
	Name        string      `json:"name"`
	Description string      `json:"description"`
	Rules       []RuleInput `json:"rules"`
}

// Preset ids.
const (
	PresetEverything       = "everything"
	PresetSpecDefault      = "manifest-by-default"
	PresetMaintainerrLists = "maintainerr-manifest"
	// SpecFullTag is the tag the spec preset copies in full.
	SpecFullTag = "bunkarr-full"
)

func rawJSON(v any) json.RawMessage {
	b, _ := json.Marshal(v) // strings and bools always marshal
	return b
}

// Presets returns the presets (design §8.4). The first, "Back up everything", is the default:
// no rules, so every file is full.
func Presets() []Preset {
	return []Preset{
		{
			ID:   PresetEverything,
			Name: "Back up everything",
			Description: "The default: no rules, so every file is copied in full to every destination. " +
				"Plex DB and *arr config backups are always full.",
			Rules: []RuleInput{},
		},
		{
			ID:   PresetSpecDefault,
			Name: "Manifest by default",
			Description: "Only media tagged " + SpecFullTag + " in Sonarr, Radarr or Lidarr is copied in full; everything else is only " +
				"listed in the destination's manifests so it can be acquired again. This stops copying new untagged media. Media " +
				"already backed up is kept until you release it, or until it is deleted or replaced under another name at the " +
				"source (then it is retained for the destination's deletedDays, and the new version is not copied unless it is " +
				"tagged). A file whose *arr facts are unknown (a stale cache) stays full, and such copies are held by the " +
				"mass-change guard. A file outside every *arr root folder (home videos, a Plex-only source) gets manifest while " +
				"the *arr caches are fresh. Plex DB and *arr config backups are always full.",
			Rules: []RuleInput{
				{Name: "Tagged " + SpecFullTag, Match: MatchAll, Action: Full,
					Conditions: []Condition{{Field: FieldArrTag, Op: OpHas, Value: rawJSON(SpecFullTag)}}},
				{Name: "Everything else", Match: MatchAll, Action: Manifest, Conditions: []Condition{}},
			},
		},
		{
			ID:   PresetMaintainerrLists,
			Name: "Keep Maintainerr deletions as manifest only",
			Description: "Items Maintainerr is about to delete are not copied, but stay listed in the manifests so they can be " +
				"acquired again. Maintainerr has no authentication: anyone who can reach it can add items to a deleting " +
				"collection, so this preset never skips them entirely.",
			Rules: []RuleInput{
				{Name: "Pending deletion in Maintainerr", Match: MatchAll, Action: Manifest,
					Conditions: []Condition{{Field: FieldMaintainerrPending, Op: OpIs, Value: rawJSON(true)}}},
			},
		},
	}
}
