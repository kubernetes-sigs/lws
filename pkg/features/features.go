package features

import (
	"k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/component-base/featuregate"
)

const (
	// InPlaceGroupRestart enables the InPlaceGroupRestart feature for LeaderWorkerSet.
	// This feature requires Kubernetes 1.36+ and the RestartAllContainersOnContainerExits feature gate.
	InPlaceGroupRestart featuregate.Feature = "InPlaceGroupRestart"
)

var (
	// FeatureGate is a shared global FeatureGate.
	FeatureGate featuregate.MutableFeatureGate = featuregate.NewFeatureGate()

	defaultLeaderWorkerSetFeatureGates = map[featuregate.Feature]featuregate.FeatureSpec{
		InPlaceGroupRestart: {Default: false, PreRelease: featuregate.Alpha},
	}
)

func init() {
	runtime.Must(FeatureGate.Add(defaultLeaderWorkerSetFeatureGates))
}
