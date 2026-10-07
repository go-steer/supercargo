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
	"net/url"
	"os"
	"strings"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.43.0"

	"github.com/go-steer/supercargo/internal/config"
)

// Shutdown flushes and stops export.
type Shutdown func(context.Context) error

// Enabled reports whether c, or the standard environment, asks for
// export. The standard off switches (OTEL_SDK_DISABLED=true,
// OTEL_TRACES_EXPORTER=none) win over both.
func Enabled(c *config.Telemetry, getenv func(string) string) bool {
	if strings.EqualFold(getenv("OTEL_SDK_DISABLED"), "true") || strings.EqualFold(getenv("OTEL_TRACES_EXPORTER"), "none") {
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

// Setup installs the global TracerProvider for a node: OTLP export per c
// (and the OTEL_* environment), named service. With nothing configured it
// installs nothing and returns a no-op Shutdown: the node's spans are
// then never recorded.
func Setup(ctx context.Context, c *config.Telemetry, service string) (Shutdown, error) {
	if !Enabled(c, os.Getenv) {
		return func(context.Context) error { return nil }, nil
	}
	if c == nil {
		c = &config.Telemetry{Protocol: protocolFromEnv()}
	}
	exp, err := exporter(ctx, c)
	if err != nil {
		return nil, err
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
	tp := sdktrace.NewTracerProvider(sdktrace.WithBatcher(exp), sdktrace.WithResource(res),
		sdktrace.WithSampler(sdktrace.ParentBased(sdktrace.TraceIDRatioBased(ratio))))
	otel.SetTracerProvider(tp)
	return tp.Shutdown, nil
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
