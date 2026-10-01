package agent

import (
	"fmt"
	"testing"

	"github.com/buildkite/agent/v3/internal/jobcgroup"
)

func TestLeftoverLinesIsBounded(t *testing.T) {
	t.Parallel()

	procs := make([]jobcgroup.Process, maxLeftoverLines+7)
	for i := range procs {
		procs[i] = jobcgroup.Process{PID: 1000 + i, PPID: 1, Name: "worker"}
	}

	lines := leftoverLines(procs)
	if got, want := len(lines), maxLeftoverLines+1; got != want {
		t.Fatalf("len(leftoverLines()) = %d, want %d", got, want)
	}
	if got, want := lines[0], `pid=1000 ppid=1 name="worker"`; got != want {
		t.Errorf("leftoverLines()[0] = %q, want %q", got, want)
	}
	if got, want := lines[maxLeftoverLines], fmt.Sprintf("and %d more", 7); got != want {
		t.Errorf("leftoverLines()[last] = %q, want %q", got, want)
	}
	if got := leftoverLines(procs[:2]); len(got) != 2 {
		t.Errorf("leftoverLines(2 processes) = %q, want 2 lines and no summary", got)
	}
}
