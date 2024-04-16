package metrics

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

func TestRecreatingGroup(t *testing.T) {
	prometheus.MustRegister(recreateGroupTimes)

	RecreatingGroup("lws-sample-0")
	RecreatingGroup("lws-sample-1")
	RecreatingGroup("lws-sample-0")

	if count := testutil.CollectAndCount(recreateGroupTimes); count != 2 {
		t.Errorf("Expecting %d metrics, got: %d", 2, count)
	}

	if count := testutil.ToFloat64(recreateGroupTimes.WithLabelValues("lws-sample-0")); count != float64(2) {
		t.Errorf("Expecting %s to have value %d, but got %f", "lws-sample-0", 2, count)
	}

	if count := testutil.ToFloat64(recreateGroupTimes.WithLabelValues("lws-sample-1")); count != float64(1) {
		t.Errorf("Expecting %s to have value %d, but got %f", "lws-sample-1", 1, count)
	}
}
