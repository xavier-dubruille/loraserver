package storage

import (
	"github.com/prometheus/client_golang/prometheus"

	"github.com/brocaar/loraserver/internal/metrics"
)

var (
	queryTimer func(string, func() error) error
)

func init() {
	qt := metrics.MustRegisterNewTimerWithError(
		"storage_function_query",
		"Per internal/storage function query duration tracking.",
		[]string{"function"},
	)

	queryTimer = func(fName string, f func() error) error {
		return qt(prometheus.Labels{"function": fName}, f)
	}
}
