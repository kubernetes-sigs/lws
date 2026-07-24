package inplacerestart

import (
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
)

func TestStateSerialization(t *testing.T) {
	tests := []struct {
		name  string
		state InPlaceRestartState
	}{
		{
			name: "idle state",
			state: InPlaceRestartState{
				Phase:             Idle,
				CurrentGeneration: 0,
				DesiredGeneration: 0,
			},
		},
		{
			name: "quiescing state with attempts",
			state: InPlaceRestartState{
				Phase:                Quiescing,
				CurrentGeneration:    1,
				DesiredGeneration:    2,
				AttemptsWithinWindow: 3,
				WindowStartedAt:      time.Now().UTC().Truncate(time.Second), // Truncate because JSON drops sub-second precision by default if not careful, though time.RFC3339Nano handles it
				GroupUniqueHash:      "hash-123",
				ControllerRevision:   "rev-456",
				ExpectedGroupSize:    3,
				AttemptStartedAt:     time.Now().UTC().Truncate(time.Second),
				TriggerPodUID:        "uid-789",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			str, err := MarshalState(tt.state)
			if err != nil {
				t.Fatalf("MarshalState() error = %v", err)
			}

			parsed, err := UnmarshalState(str)
			if err != nil {
				t.Fatalf("UnmarshalState() error = %v", err)
			}

			if diff := cmp.Diff(tt.state, parsed); diff != "" {
				t.Errorf("Unmarshaled state mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

func TestClearAttemptState(t *testing.T) {
	now := time.Now()
	state := &InPlaceRestartState{
		Phase:                WaitingForReadiness,
		CurrentGeneration:    2,
		DesiredGeneration:    3,
		AttemptsWithinWindow: 1,
		WindowStartedAt:      now,
		AttemptStartedAt:     now,
		TriggerPodUID:        "uid-123",
	}

	ClearAttemptState(state)

	if !state.AttemptStartedAt.IsZero() {
		t.Errorf("Expected AttemptStartedAt to be cleared, got %v", state.AttemptStartedAt)
	}
	if state.TriggerPodUID != "" {
		t.Errorf("Expected TriggerPodUID to be cleared, got %s", state.TriggerPodUID)
	}
	if state.DesiredGeneration != state.CurrentGeneration {
		t.Errorf("Expected DesiredGeneration to equal CurrentGeneration, got %d vs %d", state.DesiredGeneration, state.CurrentGeneration)
	}
	// Verify rolling state is preserved
	if state.AttemptsWithinWindow != 1 {
		t.Errorf("Expected AttemptsWithinWindow to be preserved")
	}
	if !state.WindowStartedAt.Equal(now) {
		t.Errorf("Expected WindowStartedAt to be preserved")
	}
}
