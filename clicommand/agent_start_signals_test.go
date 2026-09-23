package clicommand

import (
	"fmt"
	"os"
	"syscall"
	"testing"
	"time"

	"github.com/buildkite/agent/v3/agent"
	"github.com/buildkite/agent/v3/logger"
)

func TestPoolSignals_ExitingImmediatelyRunsBeforeExitFirst(t *testing.T) {
	t.Parallel()

	for name, sigs := range map[string][]os.Signal{
		"third interrupt": {syscall.SIGTERM, syscall.SIGTERM, syscall.SIGTERM},
		"quit, once the cancel grace period is over": {syscall.SIGQUIT},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			// Both paths can exit, so each records every call it makes.
			calls := make(chan string, 8)
			ps := &poolSignals{
				log:        logger.Discard,
				pool:       agent.NewAgentPool(nil, &agent.AgentConfiguration{}),
				beforeExit: func() { calls <- "beforeExit" },
				exit:       func(code int) { calls <- fmt.Sprintf("exit(%d)", code) },
			}
			signals := make(chan os.Signal, len(sigs))
			for _, sig := range sigs {
				signals <- sig
			}
			go ps.handleLoop(t.Context(), signals)
			defer close(signals)

			for _, want := range []string{"beforeExit", "exit(1)"} {
				select {
				case got := <-calls:
					if got != want {
						t.Fatalf("next call = %s, want %s", got, want)
					}
				case <-time.After(10 * time.Second):
					t.Fatalf("no call to %s", want)
				}
			}
		})
	}
}
