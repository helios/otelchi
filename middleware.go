package otelchi

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"sync"

	"github.com/felixge/httpsnoop"
	"github.com/go-chi/chi/v5"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/propagation"

	otelcontrib "go.opentelemetry.io/contrib"
	semconv "go.opentelemetry.io/otel/semconv/v1.4.0"
	oteltrace "go.opentelemetry.io/otel/trace"
)

const (
	tracerName = "github.com/helios/otelchi"
)

type bodyWrapper struct {
	io.ReadCloser

	read         int64
	err          error
	requestBody  []byte
	metadataOnly bool
}

func (w *bodyWrapper) Read(b []byte) (int, error) {
	n, err := w.ReadCloser.Read(b)
	if n > 0 {
		if !w.metadataOnly {
			w.requestBody = append(w.requestBody, b[0:n]...)
		}
	}
	n1 := int64(n)
	w.read += n1
	w.err = err
	return n, err
}

func (w *bodyWrapper) Close() error {
	return w.ReadCloser.Close()
}

// Middleware sets up a handler to start tracing the incoming
// requests. The serverName parameter should describe the name of the
// (virtual) server handling the request.
func Middleware(serverName string, opts ...Option) func(next http.Handler) http.Handler {
	cfg := config{}
	for _, opt := range opts {
		opt.apply(&cfg)
	}
	if cfg.TracerProvider == nil {
		cfg.TracerProvider = otel.GetTracerProvider()
	}
	tracer := cfg.TracerProvider.Tracer(
		tracerName,
		oteltrace.WithInstrumentationVersion(otelcontrib.SemVersion()),
	)
	if cfg.Propagators == nil {
		cfg.Propagators = otel.GetTextMapPropagator()
	}
	return func(handler http.Handler) http.Handler {
		return traceware{
			serverName:          serverName,
			tracer:              tracer,
			propagators:         cfg.Propagators,
			handler:             handler,
			chiRoutes:           cfg.ChiRoutes,
			reqMethodInSpanName: cfg.RequestMethodInSpanName,
			metadataOnly:        os.Getenv("HS_METADATA_ONLY") == "true",
			filter:              cfg.Filter,
		}
	}
}

type traceware struct {
	serverName          string
	tracer              oteltrace.Tracer
	propagators         propagation.TextMapPropagator
	handler             http.Handler
	chiRoutes           chi.Routes
	reqMethodInSpanName bool
	metadataOnly        bool
	filter              func(r *http.Request) bool
}

type recordingResponseWriter struct {
	writer       http.ResponseWriter
	written      bool
	status       int
	responseBody []byte
	metadataOnly bool
}

var rrwPool = &sync.Pool{
	New: func() interface{} {
		return &recordingResponseWriter{}
	},
}

func getRRW(writer http.ResponseWriter) *recordingResponseWriter {
	rrw := rrwPool.Get().(*recordingResponseWriter)
	rrw.written = false
	rrw.status = 0
	rrw.responseBody = []byte{}
	rrw.writer = httpsnoop.Wrap(writer, httpsnoop.Hooks{
		Write: func(next httpsnoop.WriteFunc) httpsnoop.WriteFunc {
			return func(b []byte) (int, error) {
				if !rrw.written {
					rrw.written = true
					rrw.status = http.StatusOK
				}

				if !rrw.metadataOnly && len(b) > 0 {
					rrw.responseBody = append(rrw.responseBody, b...)
				}

				return next(b)
			}
		},
		WriteHeader: func(next httpsnoop.WriteHeaderFunc) httpsnoop.WriteHeaderFunc {
			return func(statusCode int) {
				if !rrw.written {
					rrw.written = true
					rrw.status = statusCode
				}
				next(statusCode)
			}
		},
	})
	return rrw
}

func putRRW(rrw *recordingResponseWriter) {
	rrw.writer = nil
	rrwPool.Put(rrw)
}

func collectRequestHeaders(r *http.Request, span oteltrace.Span) {
	headersStr, err := json.Marshal(r.Header)
	if err == nil {
		span.SetAttributes(attribute.KeyValue{Key: "http.request.headers", Value: attribute.StringValue(string(headersStr))})
	}
}

// ServeHTTP implements the http.Handler interface. It does the actual
// tracing of the request.
func (tw traceware) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// skip if filter returns false
	if tw.filter != nil && !tw.filter(r) {
		tw.handler.ServeHTTP(w, r)
		return
	}

	metadataOnly := tw.metadataOnly

	// extract tracing header using propagator
	ctx := tw.propagators.Extract(r.Context(), propagation.HeaderCarrier(r.Header))
	// create span, based on specification, we need to set already known attributes
	// when creating the span, the only thing missing here is HTTP route pattern since
	// in go-chi/chi route pattern could only be extracted once the request is executed
	// check here for details:
	//
	// https://github.com/go-chi/chi/issues/150#issuecomment-278850733
	//
	// if we have access to chi routes, we could extract the route pattern beforehand.
	spanName := ""
	routePattern := ""
	if tw.chiRoutes != nil {
		rctx := chi.NewRouteContext()
		if tw.chiRoutes.Match(rctx, r.Method, r.URL.Path) {
			routePattern = rctx.RoutePattern()
			spanName = addPrefixToSpanName(tw.reqMethodInSpanName, r.Method, routePattern)
		}
	}

	var bw bodyWrapper
	bw.metadataOnly = metadataOnly
	if r.Body != nil && r.Body != http.NoBody {
		bw.ReadCloser = r.Body
		r.Body = &bw
	}

	ctx, span := tw.tracer.Start(
		ctx, spanName,
		oteltrace.WithAttributes(semconv.NetAttributesFromHTTPRequest("tcp", r)...),
		oteltrace.WithAttributes(semconv.EndUserAttributesFromHTTPRequest(r)...),
		oteltrace.WithAttributes(semconv.HTTPServerAttributesFromHTTPRequest(tw.serverName, routePattern, r)...),
		oteltrace.WithSpanKind(oteltrace.SpanKindServer),
	)
	defer span.End()

	// get recording response writer
	rrw := getRRW(w)
	rrw.metadataOnly = metadataOnly
	defer putRRW(rrw)

	// Add traceresponse header
	if span.IsRecording() {
		spanCtx := span.SpanContext()
		rrw.writer.Header().Add("traceresponse", fmt.Sprintf("00-%s-%s-01", spanCtx.TraceID().String(), spanCtx.SpanID().String()))
	}

	// execute next http handler
	r = r.WithContext(ctx)
	tw.handler.ServeHTTP(rrw.writer, r)

	// set span name & http route attribute if necessary
	if len(routePattern) == 0 {
		routePattern = chi.RouteContext(r.Context()).RoutePattern()
		span.SetAttributes(semconv.HTTPRouteKey.String(routePattern))

		spanName = addPrefixToSpanName(tw.reqMethodInSpanName, r.Method, routePattern)
		span.SetName(spanName)
	}

	// set status code attribute
	span.SetAttributes(semconv.HTTPStatusCodeKey.Int(rrw.status))

	// set span status
	spanStatus, spanMessage := semconv.SpanStatusFromHTTPStatusCode(rrw.status)
	span.SetStatus(spanStatus, spanMessage)

	if !metadataOnly {
		collectRequestHeaders(r, span)
		if len(bw.requestBody) > 0 {
			span.SetAttributes(attribute.KeyValue{Key: "http.request.body", Value: attribute.StringValue(string(bw.requestBody))})
		}

		if len(rrw.responseBody) > 0 {
			span.SetAttributes(attribute.KeyValue{Key: "http.response.body", Value: attribute.StringValue(string(rrw.responseBody))})
		}
	}
}

func addPrefixToSpanName(shouldAdd bool, prefix, spanName string) string {
	if shouldAdd && len(spanName) > 0 {
		spanName = prefix + " " + spanName
	}
	return spanName
}
