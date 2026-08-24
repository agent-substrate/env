// Package grpcproxy forwards gRPC calls without parsing them. A server
// built with Codec and StreamHandler relays any method and streaming
// shape as opaque byte frames, so the contract between the caller and
// the target can evolve without touching the proxy.
//
// The target environment is named in the ate-env-id request metadata; a
// Director resolves that id to the connection the call is forwarded on.
package grpcproxy

import (
	"context"
	"errors"
	"fmt"
	"io"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/encoding"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

// MetadataKey is the request metadata key naming the target environment.
const MetadataKey = "ate-env-id"

// Director resolves the environment named in a proxied call to the
// client connection the call is forwarded on.
type Director func(ctx context.Context, envID string) (*grpc.ClientConn, error)

// proxyDesc lets the forwarded stream carry any shape; unary calls are
// just streams with one message each way.
var proxyDesc = &grpc.StreamDesc{ServerStreams: true, ClientStreams: true}

// frame is one message payload, kept as the raw bytes off the wire.
type frame struct {
	payload []byte
}

// codec passes frames through untouched and falls back to proto for
// the server's registered (typed) services, so one grpc.Server can
// serve both.
type codec struct{}

// Codec returns the pass-through codec. Install it on the proxying
// server with grpc.ForceServerCodec.
func Codec() encoding.Codec { return codec{} }

func (codec) Name() string { return "proto" }

func (codec) Marshal(v any) ([]byte, error) {
	if f, ok := v.(*frame); ok {
		return f.payload, nil
	}
	msg, ok := v.(proto.Message)
	if !ok {
		return nil, fmt.Errorf("grpcproxy: cannot marshal %T", v)
	}
	return proto.Marshal(msg)
}

func (codec) Unmarshal(data []byte, v any) error {
	if f, ok := v.(*frame); ok {
		f.payload = data
		return nil
	}
	msg, ok := v.(proto.Message)
	if !ok {
		return fmt.Errorf("grpcproxy: cannot unmarshal into %T", v)
	}
	return proto.Unmarshal(data, msg)
}

// StreamHandler returns the handler forwarding calls to the connection
// director resolves. Install it with grpc.UnknownServiceHandler, so
// every method not registered on the server is proxied.
func StreamHandler(director Director) grpc.StreamHandler {
	return func(srv any, serverStream grpc.ServerStream) error {
		return proxy(director, serverStream)
	}
}

func proxy(director Director, serverStream grpc.ServerStream) error {
	fullMethod, ok := grpc.MethodFromServerStream(serverStream)
	if !ok {
		return status.Error(codes.Internal, "grpcproxy: no method in stream")
	}
	ctx := serverStream.Context()
	md, _ := metadata.FromIncomingContext(ctx)
	envID := ""
	if vals := md.Get(MetadataKey); len(vals) > 0 {
		envID = vals[0]
	}
	if envID == "" {
		return status.Errorf(codes.InvalidArgument, "grpcproxy: missing %s metadata", MetadataKey)
	}

	conn, err := director(ctx, envID)
	if err != nil {
		if _, ok := status.FromError(err); ok {
			return err
		}
		return status.Errorf(codes.Unavailable, "grpcproxy: resolving %q: %v", envID, err)
	}

	outCtx, cancel := context.WithCancel(metadata.NewOutgoingContext(ctx, md.Copy()))
	defer cancel()
	clientStream, err := conn.NewStream(outCtx, proxyDesc, fullMethod, grpc.ForceCodec(codec{}))
	if err != nil {
		return err
	}

	c2sDone := forwardClientToServer(clientStream, serverStream)
	s2cDone := forwardServerToClient(serverStream, clientStream)
	for {
		select {
		case err := <-s2cDone:
			if errors.Is(err, io.EOF) {
				// The caller finished sending; tell the target and keep
				// relaying its responses.
				clientStream.CloseSend()
				s2cDone = nil
				continue
			}
			// The caller's stream broke; cancel the forwarded call.
			return status.Errorf(codes.Internal, "grpcproxy: relaying request: %v", err)
		case err := <-c2sDone:
			// The target finished (or failed): its status is the
			// call's status.
			serverStream.SetTrailer(clientStream.Trailer())
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
	}
}

// forwardClientToServer relays the target's responses to the caller,
// forwarding the response header before the first message.
func forwardClientToServer(src grpc.ClientStream, dst grpc.ServerStream) chan error {
	done := make(chan error, 1)
	go func() {
		for first := true; ; first = false {
			f := &frame{}
			if err := src.RecvMsg(f); err != nil {
				done <- err
				return
			}
			if first {
				header, err := src.Header()
				if err != nil {
					done <- err
					return
				}
				if err := dst.SendHeader(header); err != nil {
					done <- err
					return
				}
			}
			if err := dst.SendMsg(f); err != nil {
				done <- err
				return
			}
		}
	}()
	return done
}

// forwardServerToClient relays the caller's requests to the target.
func forwardServerToClient(src grpc.ServerStream, dst grpc.ClientStream) chan error {
	done := make(chan error, 1)
	go func() {
		for {
			f := &frame{}
			if err := src.RecvMsg(f); err != nil {
				done <- err // io.EOF when the caller finished sending
				return
			}
			if err := dst.SendMsg(f); err != nil {
				done <- err
				return
			}
		}
	}()
	return done
}
