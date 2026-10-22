package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/grpc-ecosystem/grpc-gateway/v2/runtime"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.opentelemetry.io/contrib/instrumentation/google.golang.org/grpc/otelgrpc"
	"go.opentelemetry.io/contrib/instrumentation/google.golang.org/grpc/otelgrpc/filters"
	"go.opentelemetry.io/otel/trace"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/keepalive"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"

	fraudv1 "github.com/ROBERTG99/fraud-scoring-service/gen/go/fraud/v1"
	"github.com/ROBERTG99/fraud-scoring-service/internal/api"
	"github.com/ROBERTG99/fraud-scoring-service/internal/telemetry"
)

const maxBody = api.MaxBodyBytes

// serverConfig wires the transport layer.
type serverConfig struct {
	GRPCAddr string
	HTTPAddr string
	// AdminAddr serves /metrics and a detailed /readyz, kept off the public HTTP port (security audit).
	AdminAddr string
	Auth      api.AuthConfig
	Limiter   *api.RateLimiter
	Scorer    fraudv1.ScorerServer
	Cases     fraudv1.CaseServiceServer
	Ready     func(ctx context.Context) error
	Log       *slog.Logger
	// TracerProvider for gRPC server spans; nil uses the global provider (telemetry.InitTracing).
	TracerProvider trace.TracerProvider
}

// gatewayHeader forwards W3C trace context to the gRPC server in addition to the gateway's defaults.
func gatewayHeader(key string) (string, bool) {
	switch strings.ToLower(key) {
	case "traceparent", "tracestate":
		return strings.ToLower(key), true
	}
	return runtime.DefaultHeaderMatcher(key)
}

// servers is the running gRPC server (:8510) and its HTTP/JSON gateway (:8511).
type servers struct {
	grpc     *grpc.Server
	http     *http.Server
	admin    *http.Server
	grpcLis  net.Listener
	httpLis  net.Listener
	adminLis net.Listener
	health   *health.Server
	conn     *grpc.ClientConn
}

func newServers(ctx context.Context, cfg serverConfig) (*servers, error) {
	traceOpts := []otelgrpc.Option{otelgrpc.WithFilter(filters.Not(filters.HealthCheck())), otelgrpc.WithPropagators(telemetry.Propagator())}
	if cfg.TracerProvider != nil {
		traceOpts = append(traceOpts, otelgrpc.WithTracerProvider(cfg.TracerProvider))
	}
	gs := grpc.NewServer(
		grpc.MaxRecvMsgSize(maxBody),
		// Transport limits (security audit): metadata is bounded like HTTP headers, a connection carries a bounded
		// number of streams, must finish its handshake in 5 s, and may not ping more than every 10 s.
		grpc.MaxHeaderListSize(api.MaxHeaderBytes),
		grpc.MaxConcurrentStreams(api.MaxConcurrentStreams),
		grpc.ConnectionTimeout(5*time.Second),
		grpc.KeepaliveEnforcementPolicy(keepalive.EnforcementPolicy{MinTime: 10 * time.Second, PermitWithoutStream: true}),
		grpc.KeepaliveParams(keepalive.ServerParameters{MaxConnectionIdle: 5 * time.Minute, Time: time.Minute, Timeout: 20 * time.Second}),
		// Server spans continue a W3C traceparent from gRPC metadata (forwarded by the gateway from HTTP).
		grpc.StatsHandler(otelgrpc.NewServerHandler(traceOpts...)),
		grpc.ChainUnaryInterceptor(api.UnaryRecover(cfg.Log), cfg.Auth.UnaryAuth(), cfg.Limiter.Unary()),
		grpc.ChainStreamInterceptor(cfg.Auth.StreamAuth(), cfg.Limiter.Stream()),
	)
	fraudv1.RegisterScorerServer(gs, cfg.Scorer)
	fraudv1.RegisterCaseServiceServer(gs, cfg.Cases)
	hs := health.NewServer()
	healthpb.RegisterHealthServer(gs, hs)

	grpcLis, err := net.Listen("tcp", cfg.GRPCAddr)
	if err != nil {
		return nil, err
	}
	httpLis, err := net.Listen("tcp", cfg.HTTPAddr)
	if err != nil {
		_ = grpcLis.Close()
		return nil, err
	}
	adminLis, err := net.Listen("tcp", cfg.AdminAddr)
	if err != nil {
		_ = grpcLis.Close()
		_ = httpLis.Close()
		return nil, err
	}

	// The gateway calls the gRPC server over loopback so HTTP requests pass the same interceptors.
	conn, err := grpc.NewClient(loopback(grpcLis.Addr()), grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithDefaultCallOptions(grpc.MaxCallSendMsgSize(maxBody)))
	if err != nil {
		_ = grpcLis.Close()
		_ = httpLis.Close()
		_ = adminLis.Close()
		return nil, err
	}
	gw := runtime.NewServeMux(runtime.WithRoutingErrorHandler(routingError), runtime.WithIncomingHeaderMatcher(gatewayHeader), runtime.WithMarshalerOption(runtime.MIMEWildcard, &runtime.JSONPb{
		// Canonical proto3 JSON: zero values are omitted (no nulls for unset messages), snake_case names.
		MarshalOptions:   protojson.MarshalOptions{UseProtoNames: true},
		UnmarshalOptions: protojson.UnmarshalOptions{DiscardUnknown: false},
	}))
	if err := fraudv1.RegisterScorerHandler(ctx, gw, conn); err != nil {
		return nil, err
	}
	if err := fraudv1.RegisterCaseServiceHandler(ctx, gw, conn); err != nil {
		return nil, err
	}

	readyz := func(detail bool) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			rctx, cancel := context.WithTimeout(r.Context(), 500*time.Millisecond)
			defer cancel()
			if cfg.Ready != nil {
				if err := cfg.Ready(rctx); err != nil {
					msg := "not ready" // the cause names hosts and drivers: admin listener only
					if detail {
						msg = err.Error()
					}
					http.Error(w, msg, http.StatusServiceUnavailable)
					return
				}
			}
			_, _ = w.Write([]byte("ready"))
		}
	}
	healthz := func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok")) }
	adminMux := http.NewServeMux()
	adminMux.HandleFunc("GET /healthz", healthz)
	adminMux.HandleFunc("GET /readyz", readyz(true))
	adminMux.Handle("GET /metrics", promhttp.HandlerFor(telemetry.Registry, promhttp.HandlerOpts{}))

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", healthz)
	mux.HandleFunc("GET /readyz", readyz(false))
	mux.Handle("/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Reject anonymous calls before the gateway decodes the body, so a malformed body without
		// credentials is a 401, not a 400. The gRPC interceptors still verify the token itself.
		if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") {
			writeStatus(w, http.StatusUnauthorized, codes.Unauthenticated, "missing bearer token")
			return
		}
		rt, allow := match(routes, r.Method, r.URL.Path)
		if rt == nil && allow != "" {
			w.Header().Set("Allow", allow)
			writeStatus(w, http.StatusMethodNotAllowed, codes.Unimplemented, "method not allowed")
			return
		}
		if rt != nil {
			if msg := checkQuery(rt, r); msg != "" {
				writeStatus(w, http.StatusBadRequest, codes.InvalidArgument, msg)
				return
			}
		}
		if r.Body != nil && r.Method == http.MethodPost {
			body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBody))
			if err != nil {
				writeStatus(w, http.StatusRequestEntityTooLarge, codes.ResourceExhausted, "body larger than 64 KB")
				return
			}
			if hasJSONNull(body) {
				writeStatus(w, http.StatusBadRequest, codes.InvalidArgument, "null values are not accepted; omit the field")
				return
			}
			r.Body = io.NopCloser(bytes.NewReader(body))
		}
		r.Body = http.MaxBytesReader(w, r.Body, maxBody)
		gw.ServeHTTP(w, r)
	}))
	hsrv := &http.Server{Handler: mux, ReadHeaderTimeout: 2 * time.Second, ReadTimeout: 5 * time.Second, IdleTimeout: 60 * time.Second,
		MaxHeaderBytes: api.MaxHeaderBytes}
	asrv := &http.Server{Handler: adminMux, ReadHeaderTimeout: 2 * time.Second, ReadTimeout: 5 * time.Second,
		WriteTimeout: 10 * time.Second, MaxHeaderBytes: api.MaxHeaderBytes}
	return &servers{grpc: gs, http: hsrv, admin: asrv, grpcLis: grpcLis, httpLis: httpLis, adminLis: adminLis, health: hs, conn: conn}, nil
}

var routes = httpRoutes()

// routingError answers a wrong method with 405 and an Allow header (the gateway default is 501 Unimplemented).
func routingError(ctx context.Context, mux *runtime.ServeMux, m runtime.Marshaler, w http.ResponseWriter, r *http.Request, code int) {
	if code == http.StatusMethodNotAllowed {
		w.Header().Set("Allow", allowedMethods(routes, r.URL.Path))
		writeStatus(w, http.StatusMethodNotAllowed, codes.Unimplemented, "method not allowed")
		return
	}
	runtime.DefaultRoutingErrorHandler(ctx, mux, m, w, r, code)
}

// enumQueryParams lists enum query parameters; the gateway also accepts their numeric values, which the
// OpenAPI contract does not, so numbers are rejected here.
var enumQueryParams = map[string]map[string]int32{
	"status": fraudv1.CaseStatus_value,
}

// checkQuery rejects unknown query parameters and numeric enum values.
func checkQuery(rt *route, r *http.Request) string {
	for name, vals := range r.URL.Query() {
		if !rt.query[name] {
			return "unknown query parameter " + strconv.Quote(name)
		}
		if values, ok := enumQueryParams[name]; ok {
			for _, v := range vals {
				if _, ok := values[v]; !ok {
					return name + ": unknown value"
				}
			}
		}
	}
	return ""
}

// hasJSONNull reports whether a JSON document contains a null anywhere. protojson treats null as "unset",
// which the OpenAPI contract does not allow; invalid JSON is left for the gateway to reject.
func hasJSONNull(body []byte) bool {
	dec := json.NewDecoder(bytes.NewReader(body))
	for {
		tok, err := dec.Token()
		if err != nil {
			return false
		}
		if tok == nil {
			return true
		}
	}
}

// writeStatus writes a google.rpc.Status body, the same shape the gateway uses for errors.
func writeStatus(w http.ResponseWriter, httpCode int, code codes.Code, msg string) {
	body, _ := protojson.Marshal(status.New(code, msg).Proto())
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(httpCode)
	_, _ = w.Write(body)
}

func loopback(a net.Addr) string {
	if tcp, ok := a.(*net.TCPAddr); ok && tcp.IP.IsUnspecified() {
		return net.JoinHostPort("127.0.0.1", strconv.Itoa(tcp.Port))
	}
	return a.String()
}

// serve blocks until ctx is cancelled or a server fails, then shuts both down.
func (s *servers) serve(ctx context.Context) error {
	errc := make(chan error, 3)
	go func() { errc <- s.grpc.Serve(s.grpcLis) }()
	for srv, lis := range map[*http.Server]net.Listener{s.http: s.httpLis, s.admin: s.adminLis} {
		go func() {
			if err := srv.Serve(lis); !errors.Is(err, http.ErrServerClosed) {
				errc <- err
				return
			}
			errc <- nil
		}()
	}
	var err error
	select {
	case <-ctx.Done():
	case err = <-errc:
	}
	s.shutdown()
	return err
}

func (s *servers) shutdown() {
	s.health.Shutdown()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = s.http.Shutdown(ctx)
	_ = s.admin.Shutdown(ctx)
	stopped := make(chan struct{})
	go func() { s.grpc.GracefulStop(); close(stopped) }()
	select {
	case <-stopped:
	case <-ctx.Done():
		s.grpc.Stop()
	}
	_ = s.conn.Close()
}
