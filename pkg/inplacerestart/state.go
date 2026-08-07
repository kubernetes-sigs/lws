package inplacerestart

import (
	"encoding/json"
	"time"
)

type RestartPhase string

const (
	Idle                       RestartPhase = "Idle"
	Quiescing                  RestartPhase = "Quiescing"
	Signaling                  RestartPhase = "Signaling"
	WaitingForAcknowledgements RestartPhase = "WaitingForAcknowledgements"
	OpeningBarrier             RestartPhase = "OpeningBarrier"
	WaitingForReadiness        RestartPhase = "WaitingForReadiness"
	Escalating                 RestartPhase = "Escalating"
)

type InPlaceRestartState struct {
	Phase                RestartPhase `json:"phase"`
	CurrentGeneration    int          `json:"currentGeneration"`
	DesiredGeneration    int          `json:"desiredGeneration"`
	AttemptsWithinWindow int          `json:"attemptsWithinWindow"`
	WindowStartedAt      time.Time    `json:"windowStartedAt,omitempty"`
	GroupUniqueHash      string       `json:"groupUniqueHash,omitempty"`
	ControllerRevision   string       `json:"controllerRevision,omitempty"`
	ExpectedGroupSize    int          `json:"expectedGroupSize,omitempty"`
	AgentProtocolVersion string       `json:"agentProtocolVersion,omitempty"`

	// Attempt-specific fields (cleared when returning to Idle)
	AttemptStartedAt time.Time `json:"attemptStartedAt,omitempty"`
	TriggerPodUID    string    `json:"triggerPodUID,omitempty"`
}

func MarshalState(state InPlaceRestartState) (string, error) {
	b, err := json.Marshal(state)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

func UnmarshalState(data string) (InPlaceRestartState, error) {
	var state InPlaceRestartState
	err := json.Unmarshal([]byte(data), &state)
	return state, err
}

func ClearAttemptState(state *InPlaceRestartState) {
	state.AttemptStartedAt = time.Time{}
	state.TriggerPodUID = ""
	state.DesiredGeneration = state.CurrentGeneration
}
