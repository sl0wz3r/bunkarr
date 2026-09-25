package faultinject

import "testing"

func TestCrashAtNthHit(t *testing.T) {
	SetHook(CrashAt("copy.afterWrite", 2))
	defer SetHook(nil)
	Point("copy.afterWrite") // first hit: no crash
	Point("other")
	defer func() {
		r := recover()
		c, ok := r.(Crash)
		if !ok || c.Point != "copy.afterWrite" {
			t.Fatalf("recovered %v, want Crash{copy.afterWrite}", r)
		}
	}()
	Point("copy.afterWrite")
	t.Fatal("second hit did not crash")
}

func TestNoHookIsNoop(t *testing.T) {
	SetHook(nil)
	Point("anything")
}
