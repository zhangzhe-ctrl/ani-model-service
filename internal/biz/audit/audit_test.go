package audit

import "testing"

func TestEventRequiresTrustedContextAndChange(t *testing.T) {
	e := Event{TenantID: "t", Actor: "a", Workload: "w", RequestID: "r", Action: "model.create"}
	if e.Validate() == nil {
		t.Fatal("expected missing state rejection")
	}
	e.AfterState = "pending"
	if err := e.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestOperationResultDoesNotConfuseAcceptanceWithCompletion(t *testing.T) {
	for _, tc := range []struct {
		event Event
		want  string
	}{
		{Event{Action: "model.create", AfterState: "pending"}, "succeeded"},
		{Event{Action: "model_import.create", AfterState: "pending"}, "accepted"},
		{Event{Action: "model_import.retry", AfterState: "importing"}, "accepted"},
		{Event{Action: "model_import.create", AfterState: "failed"}, "failed"},
		{Event{Action: "model.delete", ErrorClass: "MODEL_IN_USE"}, "failed"},
	} {
		if got := tc.event.Result(); got != tc.want {
			t.Fatalf("event=%+v result=%s want=%s", tc.event, got, tc.want)
		}
	}
}
