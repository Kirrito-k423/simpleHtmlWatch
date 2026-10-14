package watch

import (
	"strings"
	"testing"
	"time"
)

// Exercise the SSH reader -> worker -> live Snapshot -> resource gate path.
// These are local synthetic fixtures, not remote-device measurements.
func TestMonitorSnapshotResourceGate(t *testing.T) {
	cases := []struct {
		name  string
		setup func(*fixture)
		age   time.Duration
		want  string
	}{
		{"idle", func(f *fixture) { f.npuOutput = idleNPUTable() }, 0, ""},
		{"busy", func(f *fixture) {
			f.npuOutput = strings.Replace(idleNPUTable(), "| No running processes found |", "| 0 | 0 | 99 | train | 100 |", 1)
		}, 0, "npu_process_busy"},
		{"missing_process_table", func(f *fixture) { f.npuOutput = strings.Split(idleNPUTable(), "| NPU ID")[0] }, 0, "npu_process_unknown"},
		{"empty", func(f *fixture) { f.emptyNPU.Store(true) }, 0, "npu_process_unknown"},
		{"nonzero_exit", func(f *fixture) { f.failNPU.Store(true) }, 0, "npu_sample_unknown"},
		{"missing_exit", func(f *fixture) { f.npuOutput = idleNPUTable(); f.omitExitStatus.Store(true) }, 0, "npu_sample_unknown"},
		{"stale", func(f *fixture) { f.npuOutput = idleNPUTable() }, 121 * time.Second, "npu_sample_stale"},
		{"truncated", func(f *fixture) { f.npuOutput = idleNPUTable() + strings.Repeat("x", maxOutput+16) }, 0, "npu_sample_unknown"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			tc.setup(f)
			monitor := startTrustedMonitor(t, f, []string{"npu"})
			state := waitState(t, monitor, func(s map[string]State) bool { return !s["m1"].UpdatedAt.IsZero() })["m1"]
			devices, _, err := resourceSample(testConfig().Machines[0], state, ResourceRequirements{
				Model: "910B", DevicesPerMachine: 2, MinFreeHBMMB: 60000,
				MaxUtilization: 5, MaxSampleAge: 120,
			}, time.Now().Add(tc.age))
			got := ""
			if err != nil {
				got = err.Error()
			}
			if got != tc.want {
				t.Fatalf("live Snapshot resource gate: got %q, want %q (stdout bytes=%d)", got, tc.want, len(state.Results[0].Stdout))
			}
			if tc.want == "" && len(devices) != 2 {
				t.Fatalf("idle fixture yielded %d devices", len(devices))
			}
		})
	}
}

func TestMonitorSnapshotRetainsOnlyRequiredStdout(t *testing.T) {
	f := newFixture(t)
	f.npuOutput = idleNPUTable()
	monitor := startTrustedMonitor(t, f, []string{"npu", "python", "usage"})
	state := waitState(t, monitor, func(s map[string]State) bool { return !s["m1"].UpdatedAt.IsZero() })["m1"]
	if len(state.Results) != 3 {
		t.Fatalf("live results: %d", len(state.Results))
	}
	for _, r := range state.Results {
		if r.CommandID == "npu" {
			if r.Stdout != idleNPUTable() {
				t.Fatal("raw NPU stdout was lost or replaced by combined output")
			}
		} else if r.Stdout != "" {
			t.Fatalf("unneeded %s stdout was retained", r.CommandID)
		}
		if r.Stderr != "" {
			t.Fatalf("%s stderr was duplicated", r.CommandID)
		}
		if r.Output == "" {
			t.Fatalf("%s lost its display output", r.CommandID)
		}
	}
}
