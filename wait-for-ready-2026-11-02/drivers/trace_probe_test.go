package taskstore

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

func TestEvidenceColdRecoveryClientTraces(t *testing.T) {
	previous := otel.GetTracerProvider()
	exporter := tracetest.NewInMemoryExporter()
	provider := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exporter))
	otel.SetTracerProvider(provider)
	t.Cleanup(func() {
		otel.SetTracerProvider(previous)
		require.NoError(t, provider.Shutdown(context.Background()))
	})
	TestTaskStoreWaitsForTransportRecovery(t)
	require.NoError(t, provider.ForceFlush(t.Context()))
	spans := exporter.GetSpans()
	require.Len(t, spans, 6, "Memory fails fast, then all five TaskStore RPCs wait for recovery")
	output, err := json.MarshalIndent(spans, "", "  ")
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(os.Getenv("EVIDENCE_DIRECTORY"), "cold-recovery-client-spans.json"), output, 0600))
	for _, span := range spans {
		t.Logf("EVIDENCE span name=%s trace_id=%s span_id=%s duration_seconds=%.6f status=%s attributes=%v", span.Name, span.SpanContext.TraceID(), span.SpanContext.SpanID(), span.EndTime.Sub(span.StartTime).Seconds(), span.Status.Code, span.Attributes)
	}
}
