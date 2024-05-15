package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/andydunstall/piko/pkg/log"
	"github.com/andydunstall/piko/server/cluster"
	"go.uber.org/zap"
)

var (
	errEndpointNotFound = errors.New("not endpoint found")
)

// Proxy is responsible for forwarding requests to upstream endpoints.
type Proxy struct {
	local  *localProxy
	remote *remoteProxy

	metrics *Metrics

	logger log.Logger
}

func NewProxy(clusterState *cluster.State, opts ...Option) *Proxy {
	options := defaultOptions()
	for _, opt := range opts {
		opt.apply(&options)
	}

	metrics := NewMetrics()
	logger := options.logger.WithSubsystem("proxy")
	return &Proxy{
		local:   newLocalProxy(metrics, logger),
		remote:  newRemoteProxy(clusterState, options.forwarder, metrics, logger),
		metrics: metrics,
		logger:  logger,
	}
}

// Request forwards the given HTTP request to an upstream endpoint and returns
// the response.
//
// If the request fails returns a response with status:
// - Missing endpoint ID: 401 (Bad request)
// - Upstream unreachable: 503 (Service unavailable)
// - Timeout: 504 (Gateway timeout)
func (p *Proxy) Request(
	ctx context.Context,
	r *http.Request,
) *http.Response {
	// Whether the request was forwarded from another Piko node.
	forwarded := r.Header.Get("x-piko-forward") == "true"

	logger := p.logger.With(
		zap.String("host", r.Host),
		zap.String("method", r.Method),
		zap.String("path", r.URL.Path),
		zap.Bool("forwarded", forwarded),
	)

	endpointID := endpointIDFromRequest(r)
	if endpointID == "" {
		logger.Warn("request: missing endpoint id")
		return errorResponse(http.StatusBadRequest, "missing piko endpoint id")
	}

	logger = logger.With(zap.String("endpoint-id", endpointID))

	start := time.Now()

	// Attempt to send to an endpoint connected to the local node.
	resp, err := p.local.Request(ctx, endpointID, r)
	if err == nil {
		logger.Debug(
			"request: forwarded to local conn",
			zap.Duration("latency", time.Since(start)),
		)
		return resp
	}
	if !errors.Is(err, errEndpointNotFound) {
		if errors.Is(err, context.DeadlineExceeded) {
			logger.Warn("request: endpoint timeout", zap.Error(err))

			return errorResponse(
				http.StatusGatewayTimeout,
				"endpoint timeout",
			)
		}

		logger.Warn("request: endpoint unreachable", zap.Error(err))
		return errorResponse(
			http.StatusServiceUnavailable,
			"endpoint unreachable",
		)
	}

	// If the request is from another Piko node though we don't have a
	// connection for the endpoint, we don't forward again but return an
	// error.
	if forwarded {
		logger.Warn("request: endpoint not found")
		return errorResponse(http.StatusServiceUnavailable, "endpoint not found")
	}

	// Set the 'x-piko-forward' before forwarding to a remote node.
	r.Header.Set("x-piko-forward", "true")

	// Attempt to send the request to a Piko node with a connection for the
	// endpoint.
	resp, err = p.remote.Request(ctx, endpointID, r)
	if err == nil {
		logger.Debug(
			"request: forwarded to remote",
			zap.Duration("latency", time.Since(start)),
		)

		return resp
	}
	if !errors.Is(err, errEndpointNotFound) {
		if errors.Is(err, context.DeadlineExceeded) {
			logger.Warn("request: endpoint timeout", zap.Error(err))

			return errorResponse(
				http.StatusGatewayTimeout,
				"endpoint timeout",
			)
		}

		logger.Warn("request: endpoint unreachable", zap.Error(err))
		return errorResponse(
			http.StatusServiceUnavailable,
			"endpoint unreachable",
		)
	}

	logger.Warn("request: endpoint not found")
	return errorResponse(http.StatusServiceUnavailable, "endpoint not found")
}

// AddConn registers a connection for an endpoint.
func (p *Proxy) AddConn(conn Conn) {
	p.logger.Info(
		"add conn",
		zap.String("endpoint-id", conn.EndpointID()),
		zap.String("addr", conn.Addr()),
	)
	p.local.AddConn(conn)
	p.remote.AddConn(conn)
}

// RemoveConn removes a connection for an endpoint.
func (p *Proxy) RemoveConn(conn Conn) {
	p.logger.Info(
		"remove conn",
		zap.String("endpoint-id", conn.EndpointID()),
		zap.String("addr", conn.Addr()),
	)
	p.local.RemoveConn(conn)
	p.remote.RemoveConn(conn)
}

// ConnAddrs returns a mapping of endpoint ID to connection address for
// all local connected endpoints.
func (p *Proxy) ConnAddrs() map[string][]string {
	return p.local.ConnAddrs()
}

func (p *Proxy) Metrics() *Metrics {
	return p.metrics
}

// endpointIDFromRequest returns the endpoint ID from the HTTP request, or an
// empty string if no endpoint ID is specified.
//
// This will check both the 'x-piko-endpoint' header and 'Host' header, where
// x-piko-endpoint takes precedence.
func endpointIDFromRequest(r *http.Request) string {
	endpointID := r.Header.Get("x-piko-endpoint")
	if endpointID != "" {
		return endpointID
	}

	host := r.Host
	if host != "" && strings.Contains(host, ".") {
		// If a host is given and contains a separator, use the bottom-level
		// domain as the endpoint ID.
		//
		// Such as if the domain is 'xyz.piko.example.com', then 'xyz' is the
		// endpoint ID.
		return strings.Split(host, ".")[0]
	}

	return ""
}

type errorMessage struct {
	Error string `json:"error"`
}

func errorResponse(statusCode int, message string) *http.Response {
	m := &errorMessage{
		Error: message,
	}
	b, _ := json.Marshal(m)
	return &http.Response{
		StatusCode: statusCode,
		Body:       io.NopCloser(bytes.NewReader(b)),
	}
}
