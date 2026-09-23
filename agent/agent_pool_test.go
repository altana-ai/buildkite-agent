package agent

import (
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/buildkite/agent/v3/api"
	"github.com/buildkite/agent/v3/internal/jobcgroup"
	"github.com/buildkite/agent/v3/logger"
	"github.com/buildkite/agent/v3/metrics"
	"github.com/google/uuid"
)

func TestAgentPool_StopsEveryWorkerWhenJobCgroupIsTainted(t *testing.T) {
	t.Parallel()

	server := NewFakeAPIServer(func(_ *FakeAPIServer, mux *http.ServeMux) {
		accept := func(rw http.ResponseWriter, _ *http.Request) { _, _ = rw.Write([]byte("{}")) }
		mux.HandleFunc("POST /connect", accept)
		mux.HandleFunc("POST /disconnect", accept)
	})
	defer server.Close()

	apiClient := api.NewClient(logger.Discard, api.Config{Endpoint: server.URL, Token: "llamas"})
	conf := AgentConfiguration{
		PingMode:  PingModePollOnly,
		JobCgroup: jobcgroup.NewManager(jobcgroup.ModeEnforce, t.TempDir()),
	}

	var workers []*AgentWorker
	for i := range 2 {
		token := fmt.Sprintf("alpacas-%d", i)
		server.AddAgent(token)
		workers = append(workers, NewAgentWorker(
			logger.Discard,
			&api.AgentRegisterResponse{
				UUID:              uuid.New().String(),
				Name:              fmt.Sprintf("agent-%d", i),
				AccessToken:       token,
				Endpoint:          server.URL,
				PingInterval:      1,
				JobStatusInterval: 5,
				HeartbeatInterval: 60,
			},
			metrics.NewCollector(logger.Discard, metrics.CollectorConfig{}),
			apiClient,
			AgentWorkerConfig{AgentConfiguration: conf, SpawnIndex: i + 1},
		))
	}
	pool := NewAgentPool(workers, &conf)

	done := make(chan error, 1)
	go func() { done <- pool.Start(t.Context()) }()
	conf.JobCgroup.Taint()

	select {
	case err := <-done:
		// A nil error is what makes the agent exit 0, which stops systemd
		// restarting it on a host that is about to be replaced.
		if err != nil {
			t.Errorf("pool.Start() = %v, want nil", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("pool.Start() did not return after the job cgroup was tainted")
	}
}
