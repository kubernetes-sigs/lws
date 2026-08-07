package v1

const (
	// InPlaceRestartStateAnnotationKey is the annotation on the leader pod that stores the structured JSON durable state.
	// It tracks phase, desiredGeneration, attemptsWithinWindow, groupUniqueHash, etc.
	InPlaceRestartStateAnnotationKey = "leaderworkerset.sigs.k8s.io/in-place-restart-state"

	// HandledRestartCountsAnnotationKey is the annotation on each pod that stores the handled container restart counts
	// and marker counts. It prevents duplicate failure processing.
	HandledRestartCountsAnnotationKey = "leaderworkerset.sigs.k8s.io/handled-restart-counts"

	// DesiredRestartGenerationAnnotationKey is projected into the agent to signal it to adopt a new generation.
	DesiredRestartGenerationAnnotationKey = "leaderworkerset.sigs.k8s.io/desired-restart-generation"

	// BarrierOpenAnnotationKey is projected into the agent/barrier to unblock the physical startup barrier.
	BarrierOpenAnnotationKey = "leaderworkerset.sigs.k8s.io/barrier-open"
)
