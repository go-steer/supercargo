// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Package telemetry sets up OpenTelemetry export for a node
// (docs/DESIGN.md, A.16): traces over OTLP to a collector, or nothing.
package telemetry

import (
	"context"
	"errors"
	"net/url"
	"os"
	"strings"
	"sync"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetricgrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.43.0"

	"github.com/go-steer/supercargo/internal/config"
)

// Shutdown flushes and stops export.
type Shutdown func(context.Context) error

// Enabled reports whether c, or the standard environment, asks for
// export. The standard off switches win over both: OTEL_SDK_DISABLED=true,
// or OTEL_TRACES_EXPORTER and OTEL_METRICS_EXPORTER both none.
func Enabled(c *config.Telemetry, getenv func(string) string) bool {
	if strings.EqualFold(getenv("OTEL_SDK_DISABLED"), "true") || (none(getenv, "TRACES") && none(getenv, "METRICS")) {
		return false
	}
	if c != nil {
		return true
	}
	for _, k := range []string{"OTEL_EXPORTER_OTLP_ENDPOINT", "OTEL_EXPORTER_OTLP_TRACES_ENDPOINT"} {
		if getenv(k) != "" {
			return true
		}
	}
	return false
}

// Setup installs the global TracerProvider and MeterProvider for a node:
// OTLP export per c (and the OTEL_* environment), named service. With
// nothing configured it installs nothing and returns a no-op Shutdown:
// the node's spans and metrics are then never recorded.
func Setup(ctx context.Context, c *config.Telemetry, service string) (Shutdown, error) {
	if !Enabled(c, os.Getenv) {
		return func(context.Context) error { return nil }, nil
	}
	if c == nil {
		c = &config.Telemetry{Protocol: protocolFromEnv()}
	}
	if c.ServiceName != "" {
		service = c.ServiceName
	}
	// Ours is schemaless, so the merge can't conflict.
	res, _ := resource.Merge(resource.Default(), resource.NewSchemaless(semconv.ServiceName(service)))
	ratio := 1.0
	if c.SampleRatio != nil {
		ratio = *c.SampleRatio
	}
	var shutdowns []Shutdown
	if !none(os.Getenv, "TRACES") {
		exp, err := exporter(ctx, c)
		if err != nil {
			return nil, err
		}
		tp := sdktrace.NewTracerProvider(sdktrace.WithBatcher(exp), sdktrace.WithResource(res),
			sdktrace.WithSampler(sampler(ratio)))
		otel.SetTracerProvider(tp)
		shutdowns = append(shutdowns, tp.Shutdown)
	}
	if !none(os.Getenv, "METRICS") {
		mexp, err := metricExporter(ctx, c)
		if err != nil {
			_ = all(shutdowns)(ctx)
			return nil, err
		}
		var ropts []sdkmetric.PeriodicReaderOption
		if c.MetricInterval > 0 {
			ropts = append(ropts, sdkmetric.WithInterval(c.MetricInterval))
		}
		mp := sdkmetric.NewMeterProvider(sdkmetric.WithResource(res), sdkmetric.WithReader(sdkmetric.NewPeriodicReader(mexp, ropts...)))
		otel.SetMeterProvider(mp)
		shutdowns = append(shutdowns, mp.Shutdown)
	}
	return all(shutdowns), nil
}

// none reports whether OTEL_<SIGNAL>_EXPORTER is "none".
func none(getenv func(string) string, signal string) bool {
	return strings.EqualFold(getenv("OTEL_"+signal+"_EXPORTER"), "none")
}

// all shuts every provider down at once, each with the whole budget: a
// trace collector that hangs mustn't cost the last metrics.
func all(shutdowns []Shutdown) Shutdown {
	return func(ctx context.Context) error {
		errs := make([]error, len(shutdowns))
		var wg sync.WaitGroup
		for i, s := range shutdowns {
			wg.Add(1)
			go func() {
				defer wg.Done()
				errs[i] = s(ctx)
			}()
		}
		wg.Wait()
		return errors.Join(errs...)
	}
}

// metricExporter exports metrics to c.MetricsEndpoint, else to the
// traces' collector.
func metricExporter(ctx context.Context, c *config.Telemetry) (sdkmetric.Exporter, error) {
	endpoint := c.MetricsEndpoint
	if endpoint == "" {
		endpoint = c.Endpoint
	}
	if c.Protocol == "grpc" {
		var opts []otlpmetricgrpc.Option
		if endpoint != "" {
			opts = append(opts, otlpmetricgrpc.WithEndpoint(endpoint))
		}
		if c.Insecure {
			opts = append(opts, otlpmetricgrpc.WithInsecure())
		}
		return otlpmetricgrpc.New(ctx, opts...)
	}
	var opts []otlpmetrichttp.Option
	switch {
	case strings.HasPrefix(endpoint, "http://") || strings.HasPrefix(endpoint, "https://"):
		u, err := url.Parse(endpoint)
		if err != nil {
			return nil, err
		}
		switch {
		case u.Path == "" || u.Path == "/":
			u.Path = "/v1/metrics"
		case c.MetricsEndpoint == "" && strings.HasSuffix(u.Path, "/v1/traces"):
			// The traces' collector, behind any prefix: its metrics path.
			u.Path = strings.TrimSuffix(u.Path, "/v1/traces") + "/v1/metrics"
		}
		return otlpmetrichttp.New(ctx, otlpmetrichttp.WithEndpointURL(u.String()))
	case endpoint != "":
		opts = append(opts, otlpmetrichttp.WithEndpoint(endpoint))
	}
	if c.Insecure {
		opts = append(opts, otlpmetrichttp.WithInsecure())
	}
	return otlpmetrichttp.New(ctx, opts...)
}

// sampler keeps ratio of traces, by trace ID. A local parent's decision
// is followed (a trace is kept or dropped whole); a remote parent's flag
// never decides, so a partner can't switch our tracing on or off, even a
// trusted one (whose context is continued) or a stranger claiming to be
// one.
func sampler(ratio float64) sdktrace.Sampler {
	root := sdktrace.TraceIDRatioBased(ratio)
	return sdktrace.ParentBased(root, sdktrace.WithRemoteParentSampled(root), sdktrace.WithRemoteParentNotSampled(root))
}

func protocolFromEnv() string {
	for _, k := range []string{"OTEL_EXPORTER_OTLP_TRACES_PROTOCOL", "OTEL_EXPORTER_OTLP_PROTOCOL"} {
		if strings.HasPrefix(os.Getenv(k), "grpc") {
			return "grpc"
		}
	}
	return "http"
}

func exporter(ctx context.Context, c *config.Telemetry) (sdktrace.SpanExporter, error) {
	if c.Protocol == "grpc" {
		var opts []otlptracegrpc.Option
		if c.Endpoint != "" {
			opts = append(opts, otlptracegrpc.WithEndpoint(c.Endpoint))
		}
		if c.Insecure {
			opts = append(opts, otlptracegrpc.WithInsecure())
		}
		return otlptracegrpc.New(ctx, opts...)
	}
	var opts []otlptracehttp.Option
	switch {
	case strings.HasPrefix(c.Endpoint, "http://") || strings.HasPrefix(c.Endpoint, "https://"):
		// The URL's scheme decides TLS (config validation ties it to
		// insecure). A URL without a path means the collector: the traces
		// path is added, as for OTEL_EXPORTER_OTLP_ENDPOINT.
		u, err := url.Parse(c.Endpoint)
		if err != nil {
			return nil, err
		}
		if u.Path == "" || u.Path == "/" {
			u.Path = "/v1/traces"
		}
		return otlptracehttp.New(ctx, otlptracehttp.WithEndpointURL(u.String()))
	case c.Endpoint != "":
		opts = append(opts, otlptracehttp.WithEndpoint(c.Endpoint))
	}
	if c.Insecure {
		opts = append(opts, otlptracehttp.WithInsecure())
	}
	return otlptracehttp.New(ctx, opts...)
}
