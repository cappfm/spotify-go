package spotify

import (
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/metric"
)

var meter = otel.GetMeterProvider().Meter("github.com/cappfm/spotify-go")

var metricLatencyHist, _ = meter.Int64Histogram("spotify.requests.latency",
	metric.WithUnit("ms"),
	metric.WithDescription("Spotify HTTP request latency."),
)
