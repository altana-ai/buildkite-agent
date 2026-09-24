package jobcontainers

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/buildkite/agent/v3/logger"
	"github.com/google/go-cmp/cmp"
)

// fakeDocker is a daemon whose containers are all running.
type fakeDocker struct {
	mu         sync.Mutex
	containers map[string]Container
	networks   []string

	// stuck containers survive being removed.
	stuck map[string]bool

	listErr, inspectErr, networksErr error

	calls []string
}

func (f *fakeDocker) record(call string, ids []string) {
	f.calls = append(f.calls, call+" "+strings.Join(ids, ","))
}

func (f *fakeDocker) RunningContainers(context.Context) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.listErr != nil {
		return nil, f.listErr
	}
	ids := make([]string, 0, len(f.containers))
	for id := range f.containers {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	return ids, nil
}

func (f *fakeDocker) Inspect(_ context.Context, ids []string) ([]Container, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("inspect", ids)
	if f.inspectErr != nil {
		return nil, f.inspectErr
	}
	var found []Container
	for _, id := range ids {
		if c, ok := f.containers[id]; ok {
			found = append(found, c)
		}
	}
	return found, nil
}

func (f *fakeDocker) Networks(context.Context) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.networks), f.networksErr
}

func (f *fakeDocker) Stop(_ context.Context, ids []string, timeout time.Duration) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("stop "+timeout.String(), ids)
	return nil
}

func (f *fakeDocker) Remove(_ context.Context, ids []string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("rm", ids)
	var err error
	for _, id := range ids {
		if f.stuck[id] {
			err = errors.New("removal of container " + id + " is already in progress")
			continue
		}
		delete(f.containers, id)
	}
	return err
}

func (f *fakeDocker) RemoveNetworks(_ context.Context, ids []string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("network rm", ids)
	f.networks = slices.DeleteFunc(f.networks, func(id string) bool { return slices.Contains(ids, id) })
	return nil
}

func (f *fakeDocker) start(cs ...Container) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.containers == nil {
		f.containers = make(map[string]Container)
	}
	for _, c := range cs {
		f.containers[c.ID] = c
	}
}

const buildDir = "/var/lib/buildkite-agent/builds/agent-1"

var testJob = Job{ID: "job-1", BuildDir: buildDir}

func TestLeftovers_OneAgentTakesEveryContainerStartedDuringTheJob(t *testing.T) {
	t.Parallel()

	f := &fakeDocker{}
	f.start(Container{ID: "before", Name: "service", Image: "redis"})
	s := New(f, false)
	snap, err := s.Snapshot(t.Context())
	if err != nil {
		t.Fatalf("s.Snapshot() error = %v", err)
	}
	f.start(
		Container{ID: "unlabelled", Name: "itest-db", Image: "postgres:16"},
		Container{ID: "other-job", Name: "x", Image: "alpine", Labels: map[string]string{"com.buildkite.job-id": "job-2"}},
	)

	got, err := s.Leftovers(t.Context(), snap, testJob)
	if err != nil {
		t.Fatalf("s.Leftovers() error = %v", err)
	}
	want := []Leftover{
		{ID: "other-job", Name: "x", Image: "alpine", Reason: ReasonStartedDuringJob},
		{ID: "unlabelled", Name: "itest-db", Image: "postgres:16", Reason: ReasonStartedDuringJob},
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("s.Leftovers() diff (-want +got):\n%s", diff)
	}
}

func TestLeftovers_SharedHostTakesOnlyTheJobsContainers(t *testing.T) {
	t.Parallel()

	f := &fakeDocker{}
	s := New(f, true)
	snap, err := s.Snapshot(t.Context())
	if err != nil {
		t.Fatalf("s.Snapshot() error = %v", err)
	}
	f.start(
		Container{ID: "a-underscore-label", Labels: map[string]string{"com.buildkite.job_id": "job-1"}},
		Container{ID: "b-dash-label", Labels: map[string]string{"com.buildkite.job-id": "job-1"}},
		Container{ID: "c-bind", BindSources: []string{"/tmp/x", buildDir + "/org/pipeline"}},
		Container{ID: "d-bind-dir-itself", BindSources: []string{buildDir}},
		Container{ID: "e-compose", Labels: map[string]string{"com.docker.compose.project.working_dir": buildDir + "/org/pipeline"}},
		Container{ID: "f-other-job", Labels: map[string]string{"com.buildkite.job-id": "job-2"}},
		Container{ID: "g-sibling-dir", BindSources: []string{buildDir + "0/org/pipeline"}},
		Container{ID: "h-parent-dir", BindSources: []string{"/var/lib/buildkite-agent/builds"}},
		Container{ID: "i-sibling-compose", Labels: map[string]string{"com.docker.compose.project.working_dir": "/var/lib/buildkite-agent/builds/agent-2/org/p"}},
		Container{ID: "j-nothing"},
	)

	got, err := s.Leftovers(t.Context(), snap, testJob)
	if err != nil {
		t.Fatalf("s.Leftovers() error = %v", err)
	}
	want := []Leftover{
		{ID: "a-underscore-label", Reason: ReasonJobLabel},
		{ID: "b-dash-label", Reason: ReasonJobLabel},
		{ID: "c-bind", Reason: ReasonBindMount},
		{ID: "d-bind-dir-itself", Reason: ReasonBindMount},
		{ID: "e-compose", Reason: ReasonComposeDir},
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("s.Leftovers() diff (-want +got):\n%s", diff)
	}
}

func TestLeftovers_SharedHostWithoutAJobIDOrBuildDirMatchesNothing(t *testing.T) {
	t.Parallel()

	f := &fakeDocker{}
	f.start(Container{
		ID:          "unlabelled",
		Labels:      map[string]string{"com.buildkite.job-id": "", "com.docker.compose.project.working_dir": "/"},
		BindSources: []string{"/"},
	})
	s := New(f, true)
	got, err := s.Leftovers(t.Context(), &Snapshot{}, Job{})
	if err != nil {
		t.Fatalf("s.Leftovers() error = %v", err)
	}
	if len(got) != 0 {
		t.Errorf("s.Leftovers() = %v, want none", got)
	}
}

func TestLeftovers_NothingNewNeedsNoInspect(t *testing.T) {
	t.Parallel()

	f := &fakeDocker{}
	f.start(Container{ID: "before"})
	s := New(f, false)
	snap, err := s.Snapshot(t.Context())
	if err != nil {
		t.Fatalf("s.Snapshot() error = %v", err)
	}
	got, err := s.Leftovers(t.Context(), snap, testJob)
	if err != nil || len(got) != 0 {
		t.Errorf("s.Leftovers() = %v, %v, want none", got, err)
	}
	if len(f.calls) != 0 {
		t.Errorf("docker calls = %q, want none", f.calls)
	}
}

func TestLeftovers_Errors(t *testing.T) {
	t.Parallel()

	for name, f := range map[string]*fakeDocker{
		"list":    {listErr: errors.New("daemon gone")},
		"inspect": {inspectErr: errors.New("daemon gone")},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f.start(Container{ID: "new"})
			if _, err := New(f, false).Leftovers(t.Context(), &Snapshot{}, testJob); err == nil {
				t.Error("Leftovers() error = nil, want the daemon's error")
			}
		})
	}
}

func TestSnapshot_Errors(t *testing.T) {
	t.Parallel()

	for name, f := range map[string]*fakeDocker{
		"containers": {listErr: errors.New("no daemon")},
		"networks":   {networksErr: errors.New("no daemon")},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if snap, err := New(f, false).Snapshot(t.Context()); err == nil {
				t.Errorf("Snapshot() = %v, nil, want an error", snap)
			}
		})
	}
}

func TestRemove(t *testing.T) {
	t.Parallel()

	leftovers := []Leftover{{ID: "a"}, {ID: "b"}}
	for name, test := range map[string]struct {
		shared    bool
		wantCalls []string
	}{
		"one agent removes at once":    {shared: false, wantCalls: []string{"rm a,b"}},
		"shared host stops them first": {shared: true, wantCalls: []string{"stop 10s a,b", "rm a,b"}},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			f := &fakeDocker{}
			f.start(Container{ID: "a"}, Container{ID: "b"}, Container{ID: "other"})
			if err := New(f, test.shared).Remove(t.Context(), leftovers); err != nil {
				t.Fatalf("Remove() error = %v", err)
			}
			if diff := cmp.Diff(test.wantCalls, f.calls); diff != "" {
				t.Errorf("docker calls diff (-want +got):\n%s", diff)
			}
			if got, _ := f.RunningContainers(t.Context()); !slices.Equal(got, []string{"other"}) {
				t.Errorf("running containers = %q, want only %q", got, "other")
			}
		})
	}
}

func TestRemove_NothingToRemove(t *testing.T) {
	t.Parallel()

	f := &fakeDocker{}
	if err := New(f, true).Remove(t.Context(), nil); err != nil {
		t.Fatalf("Remove() error = %v", err)
	}
	if len(f.calls) != 0 {
		t.Errorf("docker calls = %q, want none", f.calls)
	}
}

func TestRemove_Survivors(t *testing.T) {
	t.Parallel()

	f := &fakeDocker{stuck: map[string]bool{"b": true}}
	f.start(Container{ID: "a"}, Container{ID: "b"})
	err := New(f, false).Remove(t.Context(), []Leftover{{ID: "a"}, {ID: "b"}})
	if !errors.Is(err, ErrNotRemoved) {
		t.Fatalf("Remove() error = %v, want %v", err, ErrNotRemoved)
	}
	if !strings.Contains(err.Error(), "b") || strings.Contains(err.Error(), "a,") {
		t.Errorf("Remove() error = %q, want it to name only the survivor", err)
	}
}

// A container that the daemon fails to remove but that has stopped anyway,
// for example because it exited while being removed, has not outlived the
// job.
func TestRemove_StoppedDespiteAnErrorIsRemoved(t *testing.T) {
	t.Parallel()

	f := &fakeDocker{stuck: map[string]bool{"a": true}}
	f.start(Container{ID: "a"})
	d := &exitsOnRemove{fakeDocker: f}
	if err := New(d, false).Remove(t.Context(), []Leftover{{ID: "a"}}); err != nil {
		t.Errorf("Remove() error = %v, want nil since nothing is running", err)
	}
}

type exitsOnRemove struct{ *fakeDocker }

func (d *exitsOnRemove) Remove(ctx context.Context, ids []string) error {
	err := d.fakeDocker.Remove(ctx, ids)
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, id := range ids {
		delete(d.containers, id)
	}
	return err
}

func TestRemove_UnverifiableIsNotRemoved(t *testing.T) {
	t.Parallel()

	f := &fakeDocker{}
	f.start(Container{ID: "a"})
	d := &listFailsAfterRemove{fakeDocker: f}
	if err := New(d, false).Remove(t.Context(), []Leftover{{ID: "a"}}); !errors.Is(err, ErrNotRemoved) {
		t.Errorf("Remove() error = %v, want %v", err, ErrNotRemoved)
	}
}

type listFailsAfterRemove struct{ *fakeDocker }

func (d *listFailsAfterRemove) Remove(ctx context.Context, ids []string) error {
	err := d.fakeDocker.Remove(ctx, ids)
	d.mu.Lock()
	defer d.mu.Unlock()
	d.listErr = errors.New("daemon gone")
	return err
}

func TestRemoveNetworks(t *testing.T) {
	t.Parallel()

	for name, test := range map[string]struct {
		shared bool
		want   []string
	}{
		"one agent removes the job's networks": {shared: false, want: []string{"bridge", "old"}},
		"shared host leaves every network":     {shared: true, want: []string{"bridge", "old", "new"}},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			f := &fakeDocker{networks: []string{"bridge", "old"}}
			s := New(f, test.shared)
			snap, err := s.Snapshot(t.Context())
			if err != nil {
				t.Fatalf("s.Snapshot() error = %v", err)
			}
			f.networks = append(f.networks, "new")
			if err := s.RemoveNetworks(t.Context(), snap); err != nil {
				t.Fatalf("s.RemoveNetworks() error = %v", err)
			}
			if diff := cmp.Diff(test.want, f.networks); diff != "" {
				t.Errorf("networks diff (-want +got):\n%s", diff)
			}
		})
	}
}

func TestRemoveNetworks_NoNewNetworksNeedsNoRemove(t *testing.T) {
	t.Parallel()

	f := &fakeDocker{networks: []string{"bridge"}}
	s := New(f, false)
	snap, err := s.Snapshot(t.Context())
	if err != nil {
		t.Fatalf("s.Snapshot() error = %v", err)
	}
	if err := s.RemoveNetworks(t.Context(), snap); err != nil {
		t.Fatalf("s.RemoveNetworks() error = %v", err)
	}
	if len(f.calls) != 0 {
		t.Errorf("docker calls = %q, want none", f.calls)
	}
}

func TestWarnUnavailable_WarnsOnce(t *testing.T) {
	t.Parallel()

	var buf strings.Builder
	l := logger.NewConsoleLogger(logger.NewTextPrinter(&buf), func(int) {})
	l.SetLevel(logger.DEBUG)
	s := New(&fakeDocker{}, false)
	for range 3 {
		s.WarnUnavailable(l, errors.New("no daemon"))
	}
	if got := strings.Count(buf.String(), "WARN"); got != 1 {
		t.Errorf("warnings = %d, want 1 in:\n%s", got, buf.String())
	}
	if got := strings.Count(buf.String(), "DEBUG"); got != 2 {
		t.Errorf("debug messages = %d, want 2 in:\n%s", got, buf.String())
	}
}

func TestWithin(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		dir, path string
		want      bool
	}{
		{"/b/agent-1", "/b/agent-1", true},
		{"/b/agent-1", "/b/agent-1/org/p", true},
		{"/b/agent-1", "/b/agent-1/../agent-2", false},
		{"/b/agent-1", "/b/agent-10", false},
		{"/b/agent-1", "/b", false},
		{"/b/agent-1", "/b/agent-1/..foo", true},
		{"/b/agent-1", "", false},
		{"", "/b/agent-1", false},
		{"/b/agent-1", "relative", false},
	} {
		if got := within(test.dir, test.path); got != test.want {
			t.Errorf("within(%q, %q) = %t, want %t", test.dir, test.path, got, test.want)
		}
	}
}
