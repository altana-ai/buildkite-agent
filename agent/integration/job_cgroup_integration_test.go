//go:build linux

package integration

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/buildkite/agent/v3/agent"
	"github.com/buildkite/agent/v3/api"
	"github.com/buildkite/agent/v3/internal/jobcgroup"
	"github.com/buildkite/agent/v3/logger"
	"github.com/buildkite/agent/v3/metrics"
)

// leakingBootstrap starts one process in a new session and one double-forked
// orphan, records their pids in dir, and exits 0 while both still run. Their
// output goes to /dev/null so that neither holds the job's stdout open, and
// the orphan ignores the SIGHUP a PTY sends when its session leader exits.
const leakingBootstrap = `
setsid nohup sh -c 'echo $$ > "$DIR/setsid.pid"; exec sleep 300' >/dev/null 2>&1 &
sh -c 'nohup sh -c "echo \$\$ > \"\$DIR/doublefork.pid\"; exec sleep 300" &' >/dev/null 2>&1
while [ ! -s "$DIR/setsid.pid" ] || [ ! -s "$DIR/doublefork.pid" ]; do sleep 0.05; done
echo bootstrap exiting
`

// testJobCgroup returns a Manager rooted in a fresh group under the test's
// own cgroup, or skips the test when that cgroup is not delegated to the user
// running it.
func testJobCgroup(t *testing.T, mode jobcgroup.Mode) *jobcgroup.Manager {
	t.Helper()

	data, err := os.ReadFile("/proc/self/cgroup")
	own, ok := strings.CutPrefix(strings.TrimSpace(string(data)), "0::")
	if err != nil || !ok {
		t.Skipf("no cgroup v2: %v %q", err, data)
	}
	parent := filepath.Join("/sys/fs/cgroup", own)
	if _, err := os.Stat(filepath.Join(parent, "cgroup.kill")); err != nil {
		t.Skipf("no cgroup.kill: %v", err)
	}
	root := filepath.Join(parent, fmt.Sprintf("jobcgroup-itest-%d-%d", os.Getpid(), time.Now().UnixNano()))
	if err := os.Mkdir(root, 0o755); err != nil {
		t.Skipf("the test's cgroup is not delegated: %v", err)
	}

	m := jobcgroup.NewManager(mode, root)
	t.Cleanup(func() {
		if err := m.KillAll(jobcgroup.DrainTimeout); err != nil {
			t.Errorf("KillAll() error = %v", err)
		}
		if err := syscall.Rmdir(root); err != nil {
			t.Errorf("rmdir %q: %v", root, err)
		}
	})
	return m
}

// newScriptJobRunner returns a JobRunner whose bootstrap is script, run by sh
// with $DIR set to dir.
func newScriptJobRunner(t *testing.T, server, jobID, dir, script string, conf agent.AgentConfiguration) *agent.JobRunner {
	t.Helper()
	return newLoggedScriptJobRunner(t, logger.Discard, io.Discard, server, jobID, dir, script, conf)
}

// newLoggedScriptJobRunner is newScriptJobRunner with the agent log going to
// l and the agent's stdout to stdout. Each opt changes the runner's config.
func newLoggedScriptJobRunner(t *testing.T, l logger.Logger, stdout io.Writer, server, jobID, dir, script string, conf agent.AgentConfiguration, opts ...func(*agent.JobRunnerConfig)) *agent.JobRunner {
	t.Helper()

	path := filepath.Join(dir, "bootstrap.sh")
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("os.WriteFile(%q) error = %v", path, err)
	}
	conf.BootstrapScript = "/bin/sh " + path

	jrConf := agent.JobRunnerConfig{
		Job: &api.Job{
			ID:                 jobID,
			ChunksMaxSizeBytes: 1024,
			Env:                map[string]string{"DIR": dir},
			Token:              "bkaj_job-token",
		},
		AgentConfiguration: conf,
		AgentStdout:        stdout,
		MetricsScope:       metrics.NewCollector(l, metrics.CollectorConfig{}).Scope(metrics.Tags{}),
		JobStatusInterval:  1 * time.Second,
	}
	for _, opt := range opts {
		opt(&jrConf)
	}
	jr, err := agent.NewJobRunner(t.Context(), l, api.NewClient(l, api.Config{Endpoint: server, Token: "llamasrock"}), jrConf)
	if err != nil {
		t.Fatalf("agent.NewJobRunner() error = %v", err)
	}
	return jr
}

func readPID(t *testing.T, path string) int {
	t.Helper()

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("os.ReadFile(%q) error = %v", path, err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		t.Fatalf("parsing pid in %q: %v", path, err)
	}
	return pid
}

// running reports whether pid is a live process. A zombie is not: it runs
// nothing and holds no files.
func running(pid int) bool {
	stat, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "stat"))
	if err != nil {
		return false
	}
	i := strings.LastIndexByte(string(stat), ')')
	return i < 0 || !strings.HasPrefix(string(stat[i+1:]), " Z")
}

// awaitExit reports whether pid stops running within a few seconds. Killed
// orphans are reaped by init, not by the test.
func awaitExit(pid int) bool {
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if !running(pid) {
			return true
		}
		time.Sleep(10 * time.Millisecond)
	}
	return false
}

func assertNoJobGroups(t *testing.T, m *jobcgroup.Manager) {
	t.Helper()

	entries, err := os.ReadDir(m.Root())
	if err != nil {
		t.Fatalf("os.ReadDir(%q) error = %v", m.Root(), err)
	}
	for _, e := range entries {
		if e.IsDir() {
			t.Errorf("job group %s remains after the job", e.Name())
		}
	}

	fds, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		t.Fatalf("os.ReadDir(/proc/self/fd) error = %v", err)
	}
	for _, fd := range fds {
		if target, err := os.Readlink(filepath.Join("/proc/self/fd", fd.Name())); err == nil && strings.HasPrefix(target, m.Root()+"/") {
			t.Errorf("descriptor %s for %s remains open after the job", fd.Name(), target)
		}
	}
}

// lockedBuffer is a bytes.Buffer that a logger can write to from several
// goroutines.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func TestJobCgroup_KillsProcessesThatEscapedTheProcessGroup(t *testing.T) {
	t.Parallel()

	for _, mode := range []jobcgroup.Mode{jobcgroup.ModeReport, jobcgroup.ModeEnforce} {
		for _, pty := range []bool{true, false} {
			t.Run(fmt.Sprintf("%s/pty=%t", mode, pty), func(t *testing.T) {
				t.Parallel()

				m := testJobCgroup(t, mode)
				e := createTestAgentEndpoint()
				server := e.server()
				defer server.Close()

				dir := t.TempDir()
				jr := newScriptJobRunner(t, server.URL, "leaky-job", dir, leakingBootstrap, agent.AgentConfiguration{
					RunInPty:  pty,
					JobCgroup: m,
				})
				if err := jr.Run(t.Context(), nil); err != nil {
					t.Fatalf("jr.Run() error = %v", err)
				}

				if got, want := e.finishesFor(t, "leaky-job")[0].ExitStatus, "0"; got != want {
					t.Errorf("finish.ExitStatus = %q, want %q", got, want)
				}
				for _, name := range []string{"setsid.pid", "doublefork.pid"} {
					if pid := readPID(t, filepath.Join(dir, name)); !awaitExit(pid) {
						syscall.Kill(pid, syscall.SIGKILL) //nolint:errcheck // Test cleanup.
						t.Errorf("process %d from %s outlived the job", pid, name)
					}
				}
				logs := e.logsFor(t, "leaky-job")
				if want := "2 processes outlived the job"; !strings.Contains(logs, want) {
					t.Errorf("job log = %q, want it to contain %q", logs, want)
				}
				if want := `name="sleep"`; !strings.Contains(logs, want) {
					t.Errorf("job log = %q, want it to contain %q", logs, want)
				}
				assertNoJobGroups(t, m)
			})
		}
	}
}

func TestJobCgroup_OffLeavesProcessesRunningAsBefore(t *testing.T) {
	t.Parallel()

	e := createTestAgentEndpoint()
	server := e.server()
	defer server.Close()

	dir := t.TempDir()
	jr := newScriptJobRunner(t, server.URL, "off-job", dir, leakingBootstrap, agent.AgentConfiguration{RunInPty: true})
	if err := jr.Run(t.Context(), nil); err != nil {
		t.Fatalf("jr.Run() error = %v", err)
	}

	for _, name := range []string{"setsid.pid", "doublefork.pid"} {
		pid := readPID(t, filepath.Join(dir, name))
		if !running(pid) {
			t.Errorf("process %d from %s was killed, want it left running as without job-cgroup", pid, name)
		}
		syscall.Kill(pid, syscall.SIGKILL) //nolint:errcheck // Test cleanup.
	}
	if logs := e.logsFor(t, "off-job"); strings.Contains(logs, "outlived the job") {
		t.Errorf("job log = %q, want no leftover process report", logs)
	}
}

func TestJobCgroup_CancelSendsTermBeforeKill(t *testing.T) {
	t.Parallel()

	m := testJobCgroup(t, jobcgroup.ModeEnforce)
	e := createTestAgentEndpoint()
	server := e.server()
	defer server.Close()

	// The bootstrap survives SIGTERM, so it is only stopped by the SIGKILL
	// that follows the signal grace period.
	dir := t.TempDir()
	jr := newScriptJobRunner(t, server.URL, "cancelled-job", dir, `
trap 'echo TERM >> "$DIR/signals"' TERM
setsid nohup sh -c 'echo $$ > "$DIR/setsid.pid"; exec sleep 300' >/dev/null 2>&1 &
while [ ! -s "$DIR/setsid.pid" ]; do sleep 0.05; done
touch "$DIR/ready"
while :; do sleep 0.1; done
`, agent.AgentConfiguration{
		RunInPty:          true,
		JobCgroup:         m,
		SignalGracePeriod: time.Second,
	})

	runErr := make(chan error, 1)
	go func() { runErr <- jr.Run(t.Context(), nil) }()

	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		if _, err := os.Stat(filepath.Join(dir, "ready")); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the bootstrap never became ready")
		}
	}
	if err := jr.Cancel(agent.CancelReasonJobState); err != nil {
		t.Fatalf("jr.Cancel() error = %v", err)
	}
	if err := <-runErr; err != nil {
		t.Fatalf("jr.Run() error = %v", err)
	}

	if signals, err := os.ReadFile(filepath.Join(dir, "signals")); err != nil || !strings.Contains(string(signals), "TERM") {
		t.Errorf("signals file = %q, %v, want the bootstrap to have trapped SIGTERM", signals, err)
	}
	finish := e.finishesFor(t, "cancelled-job")[0]
	if got, want := finish.Signal, "SIGKILL"; got != want {
		t.Errorf("finish.Signal = %q, want %q", got, want)
	}
	if got, want := finish.SignalReason, "cancel"; got != want {
		t.Errorf("finish.SignalReason = %q, want %q", got, want)
	}
	if pid := readPID(t, filepath.Join(dir, "setsid.pid")); !awaitExit(pid) {
		syscall.Kill(pid, syscall.SIGKILL) //nolint:errcheck // Test cleanup.
		t.Errorf("process %d outlived the cancelled job", pid)
	}
	assertNoJobGroups(t, m)
}

func TestJobCgroup_JobRunsOutsideAGroupThatCannotBeCreated(t *testing.T) {
	t.Parallel()

	// The job still runs, but nothing will kill what it leaves, so enforce
	// mode takes no more jobs.
	for mode, wantTainted := range map[jobcgroup.Mode]bool{jobcgroup.ModeEnforce: true, jobcgroup.ModeReport: false} {
		t.Run(string(mode), func(t *testing.T) {
			t.Parallel()

			e := createTestAgentEndpoint()
			server := e.server()
			defer server.Close()

			missing := filepath.Join(t.TempDir(), "missing")
			m := jobcgroup.NewManager(mode, missing)
			dir := t.TempDir()
			jr := newScriptJobRunner(t, server.URL, "no-group-job", dir, "echo ran\n", agent.AgentConfiguration{JobCgroup: m})
			if err := jr.Run(t.Context(), nil); err != nil {
				t.Fatalf("jr.Run() error = %v", err)
			}
			if got, want := e.finishesFor(t, "no-group-job")[0].ExitStatus, "0"; got != want {
				t.Errorf("finish.ExitStatus = %q, want %q", got, want)
			}
			if _, err := os.Stat(missing); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("os.Stat(%q) error = %v, want it still missing", missing, err)
			}
			if got := ignoreInDispatches(t, e, "no-group-job"); (got != nil && *got) != wantTainted {
				t.Errorf("finish.IgnoreAgentInDispatches = %v, want %t", ptrString(got), wantTainted)
			}
			select {
			case <-m.Tainted():
				if !wantTainted {
					t.Error("the manager is tainted, want it untainted in report mode")
				}
			default:
				if wantTainted {
					t.Error("the manager is not tainted, want it tainted in enforce mode")
				}
			}
		})
	}
}

func TestJobCgroup_LeftoverReportShowsNoArguments(t *testing.T) {
	t.Parallel()

	m := testJobCgroup(t, jobcgroup.ModeEnforce)
	e := createTestAgentEndpoint()
	server := e.server()
	defer server.Close()

	// The secret reaches the leftover's arguments without passing through the
	// job's environment, as one that a hook exports would.
	const secret = "s3cret-from-a-hook"
	var agentLog, stdout lockedBuffer
	l := logger.NewConsoleLogger(logger.NewJSONPrinter(&agentLog), func(int) {})
	dir := t.TempDir()
	jr := newLoggedScriptJobRunner(t, l, &stdout, server.URL, "secretive-job", dir, `
setsid nohup sh -c 'echo $$ > "$DIR/leak.pid"; while :; do sleep 1; done' leaky --token=`+secret+` >/dev/null 2>&1 &
while [ ! -s "$DIR/leak.pid" ]; do sleep 0.05; done
`, agent.AgentConfiguration{RunInPty: true, JobCgroup: m, WriteJobLogsToStdout: true})
	if err := jr.Run(t.Context(), nil); err != nil {
		t.Fatalf("jr.Run() error = %v", err)
	}

	logs := e.logsFor(t, "secretive-job")
	if want := `name="sh"`; !strings.Contains(logs, want) {
		t.Errorf("job log = %q, want it to contain %q", logs, want)
	}
	if want := "leftover_processes"; !strings.Contains(agentLog.String(), want) {
		t.Errorf("agent log = %q, want it to contain %q", agentLog.String(), want)
	}
	// The copy of the job log on the agent's stdout gets the report too.
	if want := "processes outlived the job"; !strings.Contains(stdout.String(), want) {
		t.Errorf("agent stdout = %q, want it to contain %q", stdout.String(), want)
	}
	for name, log := range map[string]string{"job log": logs, "agent log": agentLog.String(), "agent stdout": stdout.String()} {
		if strings.Contains(log, secret) {
			t.Errorf("%s = %q, want no leftover's arguments in it", name, log)
		}
	}
	if pid := readPID(t, filepath.Join(dir, "leak.pid")); !awaitExit(pid) {
		syscall.Kill(pid, syscall.SIGKILL) //nolint:errcheck // Test cleanup.
		t.Errorf("process %d outlived the job", pid)
	}
	assertNoJobGroups(t, m)
}

func TestJobCgroup_ReleasesTheGroupOfAJobThatNeverRuns(t *testing.T) {
	t.Parallel()

	for name, test := range map[string]struct {
		routes []route
		cancel bool
	}{
		// For example, because another agent has already started the job.
		"start rejected": {routes: []route{{
			Method:      "PUT",
			Path:        "/jobs/{id}/start",
			HandlerFunc: func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusUnprocessableEntity) },
		}}},
		"cancelled before running": {cancel: true},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			m := testJobCgroup(t, jobcgroup.ModeEnforce)
			e := createTestAgentEndpoint()
			server := e.server(test.routes...)
			defer server.Close()

			dir := t.TempDir()
			jr := newScriptJobRunner(t, server.URL, "unrun-job", dir, `touch "$DIR/ran"`+"\n", agent.AgentConfiguration{JobCgroup: m})
			if test.cancel {
				if err := jr.Cancel(agent.CancelReasonJobState); err != nil {
					t.Fatalf("jr.Cancel() error = %v", err)
				}
			}
			if err := jr.Run(t.Context(), nil); err == nil {
				t.Fatal("jr.Run() error = nil, want the job not to run")
			}

			if _, err := os.Stat(filepath.Join(dir, "ran")); err == nil {
				t.Error("the bootstrap ran, want it never started")
			}
			assertNoJobGroups(t, m)
		})
	}
}

// ignoreInDispatches returns ignore_agent_in_dispatches from the job's only
// finish request.
func ignoreInDispatches(t *testing.T, e *testAgentEndpoint, jobID string) *bool {
	t.Helper()
	e.mtx.Lock()
	defer e.mtx.Unlock()

	calls := e.calls[fmt.Sprintf("/jobs/%s/finish", jobID)]
	if len(calls) != 1 {
		t.Fatalf("%d finish requests for %s, want 1", len(calls), jobID)
	}
	var finish api.JobFinishRequest
	if err := json.Unmarshal(calls[0], &finish); err != nil {
		t.Fatalf("decoding the finish request: %v", err)
	}
	return finish.IgnoreAgentInDispatches
}

func ptrString(b *bool) string {
	if b == nil {
		return "nil"
	}
	return strconv.FormatBool(*b)
}
