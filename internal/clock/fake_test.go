package clock

import (
	"testing"
	"time"
)

func TestFakeFiresInOrderIncludingNestedSchedules(t *testing.T) {
	start := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	clk := NewFake(start)
	var order []string

	clk.AfterFunc(2*time.Second, func() { order = append(order, "b") })
	clk.AfterFunc(1*time.Second, func() {
		order = append(order, "a")
		// Nested timer due within the same advance window.
		clk.AfterFunc(500*time.Millisecond, func() { order = append(order, "a.5") })
	})
	clk.Advance(3 * time.Second)

	want := []string{"a", "a.5", "b"}
	if len(order) != len(want) {
		t.Fatalf("order = %v, want %v", order, want)
	}
	for i := range want {
		if order[i] != want[i] {
			t.Fatalf("order = %v, want %v", order, want)
		}
	}
	if !clk.Now().Equal(start.Add(3 * time.Second)) {
		t.Errorf("Now = %s", clk.Now())
	}
}

func TestFakeStopPreventsFiring(t *testing.T) {
	clk := NewFake(time.Unix(0, 0))
	fired := false
	timer := clk.AfterFunc(time.Second, func() { fired = true })
	if !timer.Stop() {
		t.Errorf("Stop = false, want true")
	}
	clk.Advance(2 * time.Second)
	if fired {
		t.Errorf("stopped timer fired")
	}
	if timer.Stop() {
		t.Errorf("second Stop = true, want false")
	}
}

func TestFakeExactSubSecondFireTime(t *testing.T) {
	start := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	clk := NewFake(start)
	var firedAt time.Time
	d := 90*time.Minute + 123456789*time.Nanosecond
	clk.AfterFunc(d, func() { firedAt = clk.Now() })

	pending := clk.PendingAt()
	if len(pending) != 1 || !pending[0].Equal(start.Add(d)) {
		t.Fatalf("pending = %v, want exactly %s", pending, start.Add(d))
	}
	clk.Advance(2 * time.Hour)
	if !firedAt.Equal(start.Add(d)) {
		t.Errorf("fired at %s, want %s", firedAt, start.Add(d))
	}
}
