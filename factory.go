package networkdevicereceiver

import (
	"context"
	"fmt"

	"go.opentelemetry.io/collector/component"
	"go.opentelemetry.io/collector/consumer"
	"go.opentelemetry.io/collector/receiver"
)

// componentType is the name used in a collector configuration.
//
// The design document used the working name "snmpauto"; the type is named after
// the component directory instead, which is what contrib requires.
var componentType = component.MustNewType("networkdevice")

// Stability is alpha: the metric names depend on the hardware semantic
// conventions, which are still in Development and expected to change.
const stability = component.StabilityLevelAlpha

// NewFactory returns the receiver factory.
func NewFactory() receiver.Factory {
	return receiver.NewFactory(
		componentType,
		createDefaultConfig,
		receiver.WithMetrics(createMetricsReceiver, stability),
	)
}

func createMetricsReceiver(
	_ context.Context,
	set receiver.Settings,
	rawCfg component.Config,
	next consumer.Metrics,
) (receiver.Metrics, error) {
	cfg, ok := rawCfg.(*Config)
	if !ok {
		return nil, fmt.Errorf("expected a %s config, got %T", componentType, rawCfg)
	}
	return newReceiver(cfg, set, next)
}
