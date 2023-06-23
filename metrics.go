package spotify

import (
	"go.opentelemetry.io/otel/metric/global"
	"go.opentelemetry.io/otel/metric/instrument"
	"go.opentelemetry.io/otel/metric/unit"
)

var meter = global.Meter("github.com/cappfm/spotify-go")

var metricLatencyHist, _ = meter.Int64Histogram("spotify.requests.latency",
	instrument.WithUnit(unit.Milliseconds),
	instrument.WithDescription("Spotify HTTP request latency."),
)
