package agent

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/buildkite/agent/v3/internal/jobcgroup"
	"github.com/buildkite/agent/v3/logger"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

var (
	jobCgroupLeftoverProcesses = promauto.NewCounter(prometheus.CounterOpts{
		Namespace: metricsNamespace,
		Subsystem: "job_cgroup",
		Name:      "leftover_processes_total",
		Help:      "Count of processes still running in a job's cgroup after its bootstrap exited",
	})
	jobCgroupKillFailures = promauto.NewCounter(prometheus.CounterOpts{
		Namespace: metricsNamespace,
		Subsystem: "job_cgroup",
		Name:      "kill_failures_total",
		Help:      "Count of job cgroups that still had processes after being killed",
	})
)

// jobCgroupMode is off when the job is not running in a cgroup, including
// when its group could not be created.
func (r *JobRunner) jobCgroupMode() jobcgroup.Mode {
	if r.jobCgroup == nil {
		return jobcgroup.ModeOff
	}
	return r.conf.AgentConfiguration.JobCgroup.Mode()
}

// reportLeftoverProcesses writes whatever is still running in the job's
// cgroup to the job log, so it must run before the log's final flush.
//
// Messages after the bootstrap exits go to r.output, not r.jobLogs: the
// header scanner's pipe in r.jobLogs closes when the process ends, and
// io.MultiWriter drops the write at the first writer that fails.
func (r *JobRunner) reportLeftoverProcesses() {
	procs, err := r.jobCgroup.Processes()
	if err != nil {
		r.agentLogger.Warnf("[JobRunner] Couldn't list the processes in %s: %v", r.jobCgroup.Path(), err)
		return
	}
	if len(procs) == 0 {
		return
	}
	jobCgroupLeftoverProcesses.Add(float64(len(procs)))

	lines := make([]string, 0, len(procs))
	for _, p := range procs {
		lines = append(lines, fmt.Sprintf("pid=%d ppid=%d name=%q", p.PID, p.PPID, p.Name))
	}

	when := "now"
	if r.jobCgroupMode() == jobcgroup.ModeReport {
		when = "after the job finishes"
	}
	_, _ = fmt.Fprintf(r.output, "~~~ ⚠️ %d processes outlived the job and will be killed %s\n", len(procs), when)
	_, _ = fmt.Fprintln(r.output, strings.Join(lines, "\n"))

	r.agentLogger.WithFields(
		logger.StringField("jobID", r.conf.Job.ID),
		logger.IntField("count", len(procs)),
		logger.StringField("leftover_processes", strings.Join(lines, "; ")),
	).Warnf("Job left processes running after its bootstrap exited")
}

// killLeftovers kills everything the job left running, then removes and
// releases its cgroup, so it acts at most once per job. It reports false only
// when processes survived, in which case enforce mode taints the manager so
// the agent stops accepting jobs.
func (r *JobRunner) killLeftovers() bool {
	g, mode := r.jobCgroup, r.jobCgroupMode()
	r.jobCgroup = nil

	start := time.Now()
	if err := g.Close(); err != nil {
		r.agentLogger.Warnf("[JobRunner] Couldn't close %s: %v", g.Path(), err)
	}

	err := g.Kill(jobcgroup.DrainTimeout)
	switch {
	case err == nil:
		r.agentLogger.Debugf("[JobRunner] Killed and removed %s in %v", g.Path(), time.Since(start))
		return true

	case errors.Is(err, jobcgroup.ErrNotEmpty):
		jobCgroupKillFailures.Inc()
		r.agentLogger.Errorf("Job %s: %s still has processes %v after SIGKILL", r.conf.Job.ID, g.Path(), jobcgroup.DrainTimeout)
		if mode != jobcgroup.ModeEnforce {
			return true
		}
		_, _ = fmt.Fprintf(r.output, "+++ ⛔ Processes left by this job survived SIGKILL for %v, so this agent will stop accepting jobs\n", jobcgroup.DrainTimeout)
		r.conf.AgentConfiguration.JobCgroup.Taint()
		return false

	default:
		// The group emptied, or its state is unreadable. Either way, a stuck
		// process has not been shown, so this is not a reason to stop.
		r.agentLogger.Warnf("[JobRunner] Couldn't kill and remove %s: %v", g.Path(), err)
		return true
	}
}

// releaseJobCgroup removes the job's cgroup and closes its descriptor unless
// cleanup already has, because the job can end before cleanup is set up.
func (r *JobRunner) releaseJobCgroup() {
	if r.jobCgroup != nil {
		r.killLeftovers()
	}
}
