//go:build race

package manifest

// raceEnabled: the race detector drops sync.Pool items, so encoding/json allocates a new buffer
// for every value and allocation counts say nothing about streaming.
const raceEnabled = true
