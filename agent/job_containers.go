package agent

import (
	"context"
	"fmt"
	"strings"

	"github.com/buildkite/agent/v3/internal/job"
	"github.com/buildkite/agent/v3/internal/jobcgroup"
	"github.com/buildkite/agent/v3/internal/jobcontainers"
	"github.com/buildkite/agent/v3/logger"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

var (
	jobCgroupLeftoverContainers = promauto.NewCounter(prometheus.CounterOpts{
		Namespace: metricsNamespace,
		Subsystem: "job_cgroup",
		Name:      "leftover_containers_total",
		Help:      "Count of Docker containers still running after the job that started them ended",
	})
	jobCgroupContainerRemovalFailures = promauto.NewCounter(prometheus.CounterOpts{
		Namespace: metricsNamespace,
		Subsystem: "job_cgroup",
		Name:      "container_removal_failures_total",
		Help:      "Count of jobs whose leftover Docker containers could not all be removed",
	})
)

// snapshotContainers records the host's Docker state just before the
// bootstrap starts, which the sweep after the job compares against.
func (r *JobRunner) snapshotContainers(ctx context.Context) {
	s := r.conf.AgentConfiguration.JobContainers
	if s == nil || r.jobCgroupSetting() == jobcgroup.ModeOff {
		return
	}
	snap, err := s.Snapshot(context.WithoutCancel(ctx))
	if err != nil {
		s.WarnUnavailable(r.agentLogger, err)
		return
	}
	r.containerSnapshot = snap
}

// sweepContainers reports the containers the job left running and, in enforce
// mode, removes them and the job's networks, tainting the manager if any
// survive. It writes to the job log, so it must run before the final flush.
func (r *JobRunner) sweepContainers(ctx context.Context) {
	s, snap, mode := r.conf.AgentConfiguration.JobContainers, r.containerSnapshot, r.jobCgroupSetting()
	if s == nil || snap == nil || mode == jobcgroup.ModeOff {
		return
	}
	// Cleanup must finish even when the agent is stopping.
	ctx = context.WithoutCancel(ctx)

	leftovers, err := s.Leftovers(ctx, snap, jobcontainers.Job{
		ID:       r.conf.Job.ID,
		BuildDir: job.AgentBuildDir(r.conf.AgentConfiguration.BuildPath, r.conf.AgentName),
	})
	if err != nil {
		// Docker answered when the job started, so leftovers can't be ruled
		// out.
		r.containersNotRemoved(mode, fmt.Errorf("couldn't list the job's containers: %w", err))
		return
	}
	r.reportLeftoverContainers(leftovers, mode)
	if mode != jobcgroup.ModeEnforce {
		return
	}

	if err := s.Remove(ctx, leftovers); err != nil {
		r.containersNotRemoved(mode, err)
		return
	}
	if err := s.RemoveNetworks(ctx, snap); err != nil {
		r.agentLogger.Warnf("[JobRunner] Couldn't remove the networks job %s created: %v", r.conf.Job.ID, err)
	}
}

func (r *JobRunner) reportLeftoverContainers(leftovers []jobcontainers.Leftover, mode jobcgroup.Mode) {
	if len(leftovers) == 0 {
		return
	}
	jobCgroupLeftoverContainers.Add(float64(len(leftovers)))

	lines := leftoverContainerLines(leftovers)

	what := "will be removed now"
	if mode == jobcgroup.ModeReport {
		what = "are left running, since job-cgroup is report"
	}
	_, _ = fmt.Fprintf(r.postExitLogs, "~~~ ⚠️ %d containers outlived the job and %s\n", len(leftovers), what)
	_, _ = fmt.Fprintln(r.postExitLogs, strings.Join(lines, "\n"))

	r.agentLogger.WithFields(
		logger.StringField("jobID", r.conf.Job.ID),
		logger.IntField("count", len(leftovers)),
		logger.StringField("leftover_containers", strings.Join(lines, "; ")),
	).Warnf("Job left Docker containers running")
}

// leftoverContainerLines describes each leftover container on its own line,
// up to maxLeftoverLines, then says how many more there are.
func leftoverContainerLines(leftovers []jobcontainers.Leftover) []string {
	lines := make([]string, 0, min(len(leftovers), maxLeftoverLines)+1)
	for _, c := range leftovers[:min(len(leftovers), maxLeftoverLines)] {
		lines = append(lines, fmt.Sprintf("id=%s name=%q image=%q matched=%q", shortID(c.ID), c.Name, c.Image, c.Reason))
	}
	if more := len(leftovers) - maxLeftoverLines; more > 0 {
		lines = append(lines, fmt.Sprintf("and %d more", more))
	}
	return lines
}

// shortID is the 12-character form of a container ID that docker ps shows.
func shortID(id string) string {
	return id[:min(len(id), 12)]
}

// containersNotRemoved records that the job's containers may still be
// running, and in enforce mode stops the agent accepting jobs.
func (r *JobRunner) containersNotRemoved(mode jobcgroup.Mode, err error) {
	if mode != jobcgroup.ModeEnforce {
		r.agentLogger.Warnf("[JobRunner] Job %s: %v", r.conf.Job.ID, err)
		return
	}
	jobCgroupContainerRemovalFailures.Inc()
	r.agentLogger.Errorf("Job %s: couldn't remove every container the job left running: %v", r.conf.Job.ID, err)
	_, _ = fmt.Fprintln(r.postExitLogs, "+++ ⛔ Containers left by this job could not all be removed, so this agent will stop accepting jobs")
	r.conf.AgentConfiguration.JobCgroup.Taint()
}
