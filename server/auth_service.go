package main

import (
	"context"
	"log"
	"time"

	pb "grpc_golang/proto"

	"github.com/golang-jwt/jwt/v5"
	"golang.org/x/crypto/bcrypt"
	"github.com/surrealdb/surrealdb.go"
)




type authServer struct {
	pb.UnimplementedAuthServer
	surrealDB *surrealdb.DB
}

func newAuthServer(db *surrealdb.DB) *authServer {
	return &authServer{surrealDB: db}
}

func (s *authServer) Login(ctx context.Context, req *pb.LoginRequest) (*pb.LoginResponse, error) {
	username := req.GetUsername()
	password := req.GetPassword()
	log.Printf("Login attempt for user: %s", username)

	// Fetch user from SurrealDB
	query := `SELECT * FROM users WHERE username = $username;`
	result, err := surrealdb.Query[[]User](s.surrealDB, query, map[string]interface{}{"username": username})
	if err != nil {
		log.Printf("Error fetching user %s from SurrealDB: %v", username, err)
		return &pb.LoginResponse{Error: "Internal server error"}, nil
	}

	log.Printf("SurrealDB query result for %s: %+v", username, result)

	if len(*result) == 0 || len((*result)[0].Result) == 0 {
		log.Printf("User %s not found in SurrealDB", username)
		return &pb.LoginResponse{Error: "Invalid credentials"}, nil
	}

	user := (*result)[0].Result[0]
	log.Printf("Found user %s in SurrealDB. Hashed password: %s", username, user.Password)

	// Compare hashed password
	if err := bcrypt.CompareHashAndPassword([]byte(user.Password), []byte(password)); err != nil {
		log.Printf("Password comparison failed for user %s: %v", username, err)
		return &pb.LoginResponse{Error: "Invalid credentials"}, nil
	}
	log.Printf("Password comparison successful for user: %s", username)

	expirationTime := time.Now().Add(24 * time.Hour)
	claims := &AuthClaims{
		Username: username,
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
	log.Printf("Registration attempt for user: %s", username)

	if username == "" || password == "" {
		return &pb.RegisterResponse{Success: false, Error: "Username and password cannot be empty"}, nil
	}

	// Check if user already exists in SurrealDB
	query := `SELECT * FROM users WHERE username = $username;`
	result, err := surrealdb.Query[[]User](s.surrealDB, query, map[string]interface{}{"username": username})
	if err != nil {
		log.Printf("Error checking for existing user %s in SurrealDB: %v", username, err)
		return &pb.RegisterResponse{Success: false, Error: "Internal server error"}, nil
	}

	if len(*result) > 0 && len((*result)[0].Result) > 0 {
		log.Printf("User %s already exists in SurrealDB", username)
		return &pb.RegisterResponse{Success: false, Error: "Username already exists"}, nil
	}

	// Hash password
	hashedPassword, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		log.Printf("Error hashing password for user %s: %v", username, err)
		return &pb.RegisterResponse{Success: false, Error: "Internal server error"}, nil
	}

	// Create new user in SurrealDB
	newUser := User{Username: username, Password: string(hashedPassword)}
	_, err = surrealdb.Create[User](s.surrealDB, "users", newUser)
	if err != nil {
		log.Printf("Error creating user %s in SurrealDB: %v", username, err)
		return &pb.RegisterResponse{Success: false, Error: "Internal server error"}, nil
	}

	log.Printf("New user %s registered successfully in SurrealDB", username)

	return &pb.RegisterResponse{Success: true}, nil
}
