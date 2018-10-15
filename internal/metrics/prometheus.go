package metrics

import (
	"time"

	"github.com/lib/pq"
	"github.com/prometheus/client_golang/prometheus"
)

// MustRegisterNewTimerWithError registers and returns a function for timing functions.
// It returns the same error as returned by the given function that is timed.
func MustRegisterNewTimerWithError(name string, help string, labels []string) func(prometheus.Labels, func() error) error {
	labels = append(labels, "error", "error_code")

	timer := prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name: "ns_" + name + "_duration_seconds",
		Help: help,
	}, labels)

	prometheus.MustRegister(timer)

	return func(labels prometheus.Labels, f func() error) error {
		labels["error"] = "f"
		labels["error_code"] = ""

		start := time.Now()
		err := f()
		elasped := time.Since(start)

		if err != nil {
			labels["error"] = "t"
			switch v := err.(type) {
			case *pq.Error:
				labels["error_code"] = v.Code.Name()
			}
		} else {
			labels["error"] = ""
		}

		timer.With(labels).Observe(float64(elasped) / float64(time.Second))

		return err
	}
}

// MustRegisterNewCounter registers and returns a function for counting.
func MustRegisterNewCounter(name string, help string, labels []string) func(prometheus.Labels) {
	counter := prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "ns_" + name + "_total",
		Help: help,
	}, labels)

	prometheus.MustRegister(counter)

	return func(labels prometheus.Labels) {
		counter.With(labels).Inc()
	}
}
