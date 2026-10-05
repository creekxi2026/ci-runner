package main

import (
	"fmt"
	"strings"
)

// Event selection is an operator trust decision, not fork/source attestation.
func allowedEvents(config string) (map[string]bool, error) {
	if config == "" {
		config = "workflow_dispatch"
	}
	allowed := map[string]bool{}
	for _, event := range strings.Split(config, ",") {
		event = strings.TrimSpace(event)
		switch event {
		case "workflow_dispatch", "push", "pull_request", "pull_request_target", "schedule":
			allowed[event] = true
		default:
			return nil, fmt.Errorf("invalid RUNNER_ALLOWED_EVENTS")
		}
	}
	return allowed, nil
}
func (f *fleet) admits(event string) bool {
	if f.allowedEvents == nil {
		return event == "workflow_dispatch"
	}
	return f.allowedEvents[event]
}
