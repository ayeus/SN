package lifecycle

import "testing"

func TestRollupFollowsUMLStateMachine(t *testing.T) {
	cases := []struct {
		name             string
		desired, current string
		min              int
		counts           Counts
		want             string
	}{
		{"new deployment, nothing placed", DesiredRunning, Pending, 1, Counts{}, Pending},
		{"placed, not dispatched", DesiredRunning, Pending, 1, Counts{Pending: 1}, Scheduling},
		{"host pulling weights", DesiredRunning, Scheduling, 1, Counts{Pulling: 1}, Pulling},
		{"furthest replica wins", DesiredRunning, Pulling, 2, Counts{Pulling: 1, Loading: 1}, Loading},
		{"warming", DesiredRunning, Loading, 1, Counts{Warming: 1}, Warming},
		{"serving at min", DesiredRunning, Warming, 1, Counts{Serving: 1}, Serving},
		{"serving below min is degraded", DesiredRunning, Serving, 2, Counts{Serving: 1, Pulling: 1}, Degraded},
		{"lost every replica while serving", DesiredRunning, Serving, 1, Counts{Failed: 1}, Degraded},
		{"backfill in progress after loss", DesiredRunning, Degraded, 1, Counts{Failed: 1, Pulling: 1}, Degraded},
		{"backfill complete", DesiredRunning, Degraded, 1, Counts{Failed: 1, Serving: 1}, Serving},
		{"failed stays failed until retried", DesiredRunning, Failed, 1, Counts{Failed: 1}, Failed},
		{"user stop drains", DesiredStopped, Serving, 1, Counts{Serving: 1}, Stopping},
		{"user stop completes", DesiredStopped, Stopping, 1, Counts{Stopped: 1}, Stopped},
		{"pause completes", DesiredPaused, Stopping, 1, Counts{Stopped: 1}, Paused},
		{"min 0 treated as 1", DesiredRunning, Warming, 0, Counts{Serving: 1}, Serving},
	}
	for _, c := range cases {
		if got := Rollup(c.desired, c.current, c.min, c.counts); got != c.want {
			t.Errorf("%s: Rollup = %s, want %s", c.name, got, c.want)
		}
	}
}

func TestCountsActive(t *testing.T) {
	c := Counts{Serving: 2, Stopping: 1, Failed: 3, Stopped: 4, Pending: 1}
	if got := c.Active(); got != 4 {
		t.Fatalf("Active = %d, want 4", got)
	}
}
