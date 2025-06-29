package main

import (
	"context"
	"log"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

// AuthInterceptor is a gRPC interceptor for authentication
func AuthInterceptor(ctx context.Context, req interface{}, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (interface{}, error) {
	// Skip authentication for the Login and Register RPCs
	if info.FullMethod == "/chat.Auth/Login" || info.FullMethod == "/chat.Auth/Register" {
		return handler(ctx, req)
	}

	md, ok := metadata.FromIncomingContext(ctx)
	if !ok {
		return nil, status.Errorf(codes.Unauthenticated, "missing metadata")
	}

	token := md["authorization"]
	if len(token) == 0 {
		return nil, status.Errorf(codes.Unauthenticated, "missing authorization token")
	}

	username, err := ValidateToken(token[0])
	if err != nil {
		log.Printf("Invalid token: %v", err)
		return nil, status.Errorf(codes.Unauthenticated, "invalid token: %v", err)
	}

	// Add username to context for downstream handlers
	newCtx := context.WithValue(ctx, "username", username)

	return handler(newCtx, req)
}

// AuthStreamInterceptor is a gRPC stream interceptor for authentication
func AuthStreamInterceptor(srv interface{}, ss grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
	// Skip authentication for the Chat RPC (handled within the Chat method)
	if info.FullMethod == "/chat.Chat/Chat" {
		return handler(srv, ss)
	}

	md, ok := metadata.FromIncomingContext(ss.Context())
	if !ok {
		return status.Errorf(codes.Unauthenticated, "missing metadata")
	}

	token := md["authorization"]
	if len(token) == 0 {
		return status.Errorf(codes.Unauthenticated, "missing authorization token")
	}

	username, err := ValidateToken(token[0])
	if err != nil {
		log.Printf("Invalid token: %v", err)
		return status.Errorf(codes.Unauthenticated, "invalid token: %v", err)
	}

	// Add username to context for downstream handlers
	newCtx := context.WithValue(ss.Context(), "username", username)

	// Create a new ServerStream with the updated context
	wrappedStream := grpc.ServerStream(newStream{ss, newCtx})

	return handler(srv, wrappedStream)
}

// newStream wraps around a grpc.ServerStream to provide a new context
type newStream struct {
	grpc.ServerStream
	ctx context.Context
}

func (s newStream) Context() context.Context {
	return s.ctx
}
