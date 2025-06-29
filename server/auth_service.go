package main

import (
	"context"
	"log"
	"time"

	pb "grpc_golang/proto"

	"github.com/golang-jwt/jwt/v5"
)

// In-memory user store for demonstration
var users = map[string]string{
	"user1": "pass1",
	"user2": "pass2",
}


type authServer struct {
	pb.UnimplementedAuthServer
}

func newAuthServer() *authServer {
	return &authServer{}
}

func (s *authServer) Login(ctx context.Context, req *pb.LoginRequest) (*pb.LoginResponse, error) {
	// Validate against in-memory user store
	password, ok := users[req.GetUsername()]
	if !ok || password != req.GetPassword() {
		return &pb.LoginResponse{Error: "Invalid credentials"}, nil
	}

	expirationTime := time.Now().Add(24 * time.Hour)
	claims := &AuthClaims{
		Username: req.GetUsername(),
		RegisteredClaims: jwt.RegisteredClaims{
			ExpiresAt: jwt.NewNumericDate(expirationTime),
		},
	}

	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	tokenString, err := token.SignedString(JwtKey)
	if err != nil {
		log.Printf("Error signing token: %v", err)
		return &pb.LoginResponse{Error: "Internal server error"}, nil
	}

	return &pb.LoginResponse{Token: tokenString}, nil
}

func (s *authServer) Register(ctx context.Context, req *pb.RegisterRequest) (*pb.RegisterResponse, error) {
	username := req.GetUsername()
	password := req.GetPassword()

	if username == "" || password == "" {
		return &pb.RegisterResponse{Success: false, Error: "Username and password cannot be empty"}, nil
	}

	// Check if user already exists
	if _, exists := users[username]; exists {
		return &pb.RegisterResponse{Success: false, Error: "Username already exists"}, nil
	}

	// Store new user (in-memory for now)
	users[username] = password
	log.Printf("New user registered: %s", username)

	return &pb.RegisterResponse{Success: true}, nil
}
