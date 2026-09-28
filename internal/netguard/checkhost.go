package netguard

import "context"

// CheckHost refuses host (a name or an IP literal, without port or brackets) when it is a blocked
// address or when its name resolves to one. It is for connections Bunkarr does not dial itself:
// the restic and rclone processes connect to a destination's hosts on their own, so each host
// they will dial is checked when the destination is saved and at each job start
// (docs/design/phase4.md S25; the window for DNS rebinding in between is documented there). A
// name that cannot be resolved is not refused here: the engine's own connection then fails.
func CheckHost(ctx context.Context, host string) error {
	return checkHost(ctx, host)
}
