// The MIT License
//
// Copyright (c) 2020 Temporal Technologies Inc.  All rights reserved.
//
// Copyright (c) 2020 Uber Technologies, Inc.
//
// Permission is hereby granted, free of charge, to any person obtaining a copy
// of this software and associated documentation files (the "Software"), to deal
// in the Software without restriction, including without limitation the rights
// to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
// copies of the Software, and to permit persons to whom the Software is
// furnished to do so, subject to the following conditions:
//
// The above copyright notice and this permission notice shall be included in
// all copies or substantial portions of the Software.
//
// THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
// IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
// FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
// AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
// LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
// OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN
// THE SOFTWARE.

package internal

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gogo/status"
	"github.com/pborman/uuid"
	"github.com/stretchr/testify/require"
	"go.temporal.io/api/common/v1"
	"go.temporal.io/api/errordetails/v1"
	"go.temporal.io/api/serviceerror"
	"go.temporal.io/api/workflowservice/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/health"
	"google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/resolver"
	"google.golang.org/grpc/resolver/manual"
)

func TestErrorWrapper_SimpleError(t *testing.T) {
	require := require.New(t)

	svcerr := errorInterceptor(context.Background(), "method", "request", "reply", nil,
		func(ctx context.Context, method string, req, reply interface{}, cc *grpc.ClientConn, opts ...grpc.CallOption) error {
			return status.Error(codes.NotFound, "Something not found")
		})

	require.IsType(&serviceerror.NotFound{}, svcerr)
	require.Equal("Something not found", svcerr.Error())
}

func TestErrorWrapper_ErrorWithFailure(t *testing.T) {
	require := require.New(t)

	svcerr := errorInterceptor(context.Background(), "method", "request", "reply", nil,
		func(ctx context.Context, method string, req, reply interface{}, cc *grpc.ClientConn, opts ...grpc.CallOption) error {
			st, _ := status.New(codes.AlreadyExists, "Something started").WithDetails(&errordetails.WorkflowExecutionAlreadyStartedFailure{
				StartRequestId: "srId",
				RunId:          "rId",
			})

			return st.Err()
		})

	require.IsType(&serviceerror.WorkflowExecutionAlreadyStarted{}, svcerr)
	require.Equal("Something started", svcerr.Error())
	weasErr := svcerr.(*serviceerror.WorkflowExecutionAlreadyStarted)
	require.Equal("rId", weasErr.RunId)
	require.Equal("srId", weasErr.StartRequestId)
}

type authHeadersProvider struct {
	token string
	err   error
}

func (a authHeadersProvider) GetHeaders(context.Context) (map[string]string, error) {
	if a.err != nil {
		return nil, a.err
	}
	headers := make(map[string]string)
	headers["authorization"] = a.token
	return headers, nil
}

func TestHeadersProvider_PopulateAuthToken(t *testing.T) {
	require.NoError(t, headersProviderInterceptor(authHeadersProvider{token: "test-auth-token"})(context.Background(), "method", "request", "reply", nil,
		func(ctx context.Context, method string, req, reply interface{}, cc *grpc.ClientConn, opts ...grpc.CallOption) error {
			md, ok := metadata.FromOutgoingContext(ctx)
			if !ok {
				return errors.New("unable to get outgoing context metadata")
			}
			require.Equal(t, 1, len(md.Get("authorization")))
			if md.Get("authorization")[0] != "test-auth-token" {
				return errors.New("auth token hasn't been set")
			}
			return nil
		}))
}

func TestHeadersProvider_Error(t *testing.T) {
	require.Error(t, headersProviderInterceptor(authHeadersProvider{err: errors.New("failed to populate headers")})(context.Background(), "method", "request", "reply", nil,
		func(ctx context.Context, method string, req, reply interface{}, cc *grpc.ClientConn, opts ...grpc.CallOption) error {
			return nil
		}))
}

func TestHeadersProvider_NotIncludedWhenNil(t *testing.T) {
	interceptors := requiredInterceptors(nil, nil, nil)
	require.Equal(t, 5, len(interceptors))
}

func TestHeadersProvider_IncludedWithHeadersProvider(t *testing.T) {
	interceptors := requiredInterceptors(nil, authHeadersProvider{token: "test-auth-token"}, nil)
	require.Equal(t, 6, len(interceptors))
}

func TestDialOptions(t *testing.T) {
	// Start an unimplemented gRPC server
	l, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	srv := grpc.NewServer()
	workflowservice.RegisterWorkflowServiceServer(srv, &workflowservice.UnimplementedWorkflowServiceServer{})
	healthServer := health.NewServer()
	healthServer.SetServingStatus(healthCheckServiceName, grpc_health_v1.HealthCheckResponse_SERVING)
	grpc_health_v1.RegisterHealthServer(srv, healthServer)
	defer srv.Stop()
	go func() { _ = srv.Serve(l) }()

	// Connect with unary outer and unary inner interceptors
	var trace []string
	tracer := func(name string) grpc.UnaryClientInterceptor {
		return func(
			ctx context.Context,
			method string,
			req interface{},
			reply interface{},
			cc *grpc.ClientConn,
			invoker grpc.UnaryInvoker,
			opts ...grpc.CallOption,
		) error {
			if strings.HasSuffix(method, "/SignalWorkflowExecution") {
				trace = append(trace, "begin "+name)
				defer func() { trace = append(trace, "end "+name) }()
			}
			return invoker(ctx, method, req, reply, cc, opts...)
		}
	}
	client, err := NewClient(ClientOptions{
		HostPort: l.Addr().String(),
		ConnectionOptions: ConnectionOptions{
			DialOptions: []grpc.DialOption{
				grpc.WithUnaryInterceptor(tracer("outer")),
				grpc.WithChainUnaryInterceptor(tracer("inner1"), tracer("inner2")),
			},
		},
	})
	require.NoError(t, err)
	defer client.Close()

	// Make call we know will error (ignore error)
	_, _ = client.WorkflowService().SignalWorkflowExecution(context.TODO(),
		&workflowservice.SignalWorkflowExecutionRequest{})

	// Confirm trace
	expected := []string{"begin outer", "begin inner1", "begin inner2", "end inner2", "end inner1", "end outer"}
	require.Equal(t, expected, trace)
}

func TestCustomResolver(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	// Create two gRPC servers
	s1, err := startAdditionalHostPortsGRPCServer()
	require.NoError(t, err)
	defer s1.Stop()
	s2, err := startAdditionalHostPortsGRPCServer()
	require.NoError(t, err)
	defer s2.Stop()

	// Register resolver for both IPs and create client using it
	scheme := "test-resolve-" + uuid.New()
	builder := manual.NewBuilderWithScheme(scheme)
	builder.InitialState(resolver.State{Addresses: []resolver.Address{{Addr: s1.addr}, {Addr: s2.addr}}})
	resolver.Register(builder)
	client, err := NewClient(ClientOptions{HostPort: scheme + ":///whatever"})
	require.NoError(t, err)
	defer client.Close()

	// Round-robin appears to apply to transport _connections_ rather than just
	// addresses. As such we spin here until we have round-tripped an RPC to
	// both servers to guarantee that connections to both have been established.
	// This test can fail spuriously without this section as the calls to
	// SignalWorkflow below will race with grpc-go's connection establishment.
	// This technique is consistent with the approach used in the grpc-go
	// codebase itself:
	// https://github.com/grpc/grpc-go/blob/bd7076973b45b81e37a45eb761efb789e2001618/balancer/roundrobin/roundrobin_test.go#L196-L212
	connected := map[net.Addr]struct{}{}
	req := workflowservice.SignalWorkflowExecutionRequest{
		WorkflowExecution: &common.WorkflowExecution{WorkflowId: "workflowid", RunId: "runid"},
		SignalName:        "signal",
		Namespace:         DefaultNamespace,
		Identity:          t.Name(),
	}
	var peerOut peer.Peer
	for len(connected) < 2 {
		req.RequestId = uuid.New()
		_, err := client.WorkflowService().SignalWorkflowExecution(context.Background(), &req, grpc.Peer(&peerOut))
		if err == nil {
			connected[peerOut.Addr] = struct{}{}
		}
	}

	// reset invocation counts to initial state
	s1.resetSignalWorkflowInvokeCount()
	s2.resetSignalWorkflowInvokeCount()

	// Confirm round robin'd
	require.NoError(t, client.SignalWorkflow(ctx, "workflowid", "runid", "signalname", nil))
	require.NoError(t, client.SignalWorkflow(ctx, "workflowid", "runid", "signalname", nil))
	require.Equal(t, 1, s1.signalWorkflowInvokeCount())
	require.Equal(t, 1, s2.signalWorkflowInvokeCount())
	require.NoError(t, client.SignalWorkflow(ctx, "workflowid", "runid", "signalname", nil))
	require.NoError(t, client.SignalWorkflow(ctx, "workflowid", "runid", "signalname", nil))
	require.Equal(t, 2, s1.signalWorkflowInvokeCount())
	require.Equal(t, 2, s2.signalWorkflowInvokeCount())

	// Now shutdown the first one and confirm second now receives requests
	s1.Stop()
	require.NoError(t, client.SignalWorkflow(ctx, "workflowid", "runid", "signalname", nil))
	require.Equal(t, 2, s1.signalWorkflowInvokeCount())
	require.Equal(t, 3, s2.signalWorkflowInvokeCount())
	require.NoError(t, client.SignalWorkflow(ctx, "workflowid", "runid", "signalname", nil))
	require.Equal(t, 2, s1.signalWorkflowInvokeCount())
	require.Equal(t, 4, s2.signalWorkflowInvokeCount())
}

type customResolverGRPCServer struct {
	workflowservice.UnimplementedWorkflowServiceServer
	*grpc.Server
	addr       string
	sigWfCount int32
}

func startAdditionalHostPortsGRPCServer() (*customResolverGRPCServer, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	s := &customResolverGRPCServer{Server: grpc.NewServer(), addr: l.Addr().String()}
	workflowservice.RegisterWorkflowServiceServer(s.Server, s)
	healthServer := health.NewServer()
	healthServer.SetServingStatus(healthCheckServiceName, grpc_health_v1.HealthCheckResponse_SERVING)
	grpc_health_v1.RegisterHealthServer(s.Server, healthServer)
	go func() {
		if err := s.Serve(l); err != nil {
			log.Fatal(err)
		}
	}()

	// Wait until health reports serving
	return s, s.waitUntilServing()
}

func (c *customResolverGRPCServer) waitUntilServing() error {
	// Try 20 times, waiting 100ms between
	var lastErr error
	for i := 0; i < 20; i++ {
		conn, err := grpc.Dial(c.addr, grpc.WithInsecure())
		if err != nil {
			lastErr = err
		} else {
			resp, err := grpc_health_v1.NewHealthClient(conn).Check(context.Background(), &grpc_health_v1.HealthCheckRequest{
				Service: healthCheckServiceName,
			})
			_ = conn.Close()
			if err != nil {
				lastErr = err
			} else if resp.Status != grpc_health_v1.HealthCheckResponse_SERVING {
				lastErr = fmt.Errorf("last status: %v", resp.Status)
			} else {
				return nil
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	return fmt.Errorf("failed waiting, last error: %w", lastErr)
}

func (c *customResolverGRPCServer) SignalWorkflowExecution(
	context.Context,
	*workflowservice.SignalWorkflowExecutionRequest,
) (*workflowservice.SignalWorkflowExecutionResponse, error) {
	atomic.AddInt32(&c.sigWfCount, 1)
	return &workflowservice.SignalWorkflowExecutionResponse{}, nil
}

func (c *customResolverGRPCServer) signalWorkflowInvokeCount() int {
	return int(atomic.LoadInt32(&c.sigWfCount))
}

func (c *customResolverGRPCServer) resetSignalWorkflowInvokeCount() {
	atomic.StoreInt32(&c.sigWfCount, 0)
}
