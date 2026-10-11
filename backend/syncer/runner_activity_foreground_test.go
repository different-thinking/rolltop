package syncer

import "testing"

// A send that gave up waiting names what held it up; work it never waited on,
// and its own reservation, must not be named.
func TestWorkerActivityHoldsUpForeground(t *testing.T) {
	cases := []struct {
		activity WorkerActivity
		want     bool
	}{
		{WorkerActivity{Kind: runnerWorkMailboxSync}, true},
		{WorkerActivity{Kind: runnerWorkMailboxMaintenance}, true},
		{WorkerActivity{Kind: runnerWorkMailboxSync, Waiting: true}, false},
		{WorkerActivity{Kind: runnerWorkForeground}, false},
		{WorkerActivity{Kind: runnerWorkMailboxSearchMaintenance}, false},
	}
	for _, tc := range cases {
		if got := tc.activity.HoldsUpForeground(); got != tc.want {
			t.Fatalf("%+v.HoldsUpForeground() = %t, want %t", tc.activity, got, tc.want)
		}
	}
}
