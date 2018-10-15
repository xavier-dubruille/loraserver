package mqtt

import (
	"github.com/brocaar/loraserver/internal/metrics"
	"github.com/prometheus/client_golang/prometheus"
)

var (
	receivedCounter func(string)
	sentTimer       func(string, func() error) error
)

func init() {
	counter := metrics.MustRegisterNewCounter(
		"backend_gateway_mqtt_messages_received",
		"Total number of received messages by the MQTT gateway backend.",
		[]string{"type"},
	)

	receivedCounter = func(typ string) {
		counter(prometheus.Labels{"type": typ})
	}

	timer := metrics.MustRegisterNewTimerWithError(
		"backend_gateway_mqtt_messages_sent",
		"Messages sent duration tracking of the MQTT gateway backend.",
		[]string{"type"},
	)

	sentTimer = func(typ string, f func() error) error {
		return timer(prometheus.Labels{"type": typ}, f)
	}
}
