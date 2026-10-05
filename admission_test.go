package main

import (
	"context"
	"github.com/actions/scaleset"
	"testing"
)

func TestExplicitEventAdmission(t *testing.T) {
	for _, config := range []string{"", "push,pull_request,pull_request_target,schedule,workflow_dispatch"} {
		t.Run(config, func(t *testing.T) {
			allowed, err := allowedEvents(config)
			if err != nil {
				t.Fatal(err)
			}
			for i, event := range []string{"workflow_dispatch", "push", "pull_request", "pull_request_target", "schedule", "workflow_run", "", "Push"} {
				s := &fakeSession{}
				f := &fleet{session: s, allowedEvents: allowed}
				err := f.Scale(context.Background(), &scaleset.RunnerScaleSetMessage{MessageID: i + 1, JobAvailableMessages: []*scaleset.JobAvailable{{JobMessageBase: scaleset.JobMessageBase{RunnerRequestID: 1, EventName: event}}}})
				want := 0
				if event == "workflow_dispatch" || (config != "" && i < 5) {
					want = 1
				}
				if err != nil || s.acquired != want {
					t.Fatalf("event %q acquired=%d want=%d err=%v", event, s.acquired, want, err)
				}
			}
		})
	}
}
func TestInvalidEventConfigurationFailsClosed(t *testing.T) {
	for _, config := range []string{"push,", ",push", "push,,schedule", "Push", "workflow_run", "push,unknown", "*"} {
		allowed, err := allowedEvents(config)
		if err == nil || allowed != nil {
			t.Fatalf("accepted %q", config)
		}
	}
}
