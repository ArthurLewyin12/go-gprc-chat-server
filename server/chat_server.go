package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"time"

	pb "grpc_golang/proto"

	"github.com/golang-jwt/jwt/v5"
	"github.com/redis/go-redis/v9"
	"github.com/surrealdb/surrealdb.go"
	"github.com/surrealdb/surrealdb.go/pkg/models"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
)

// AuthClaims defines the JWT claims
type AuthClaims struct {
	Username string `json:"username"`
	jwt.RegisteredClaims
}

// Define a secret key for JWT signing (should be loaded from config in real app)
var JwtKey = []byte("my_secret_key")

// ValidateToken validates the JWT and returns the username if valid
func ValidateToken(tokenString string) (string, error) {
	claims := &AuthClaims{}

	token, err := jwt.ParseWithClaims(tokenString, claims, func(token *jwt.Token) (interface{}, error) {
		if _, ok := token.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, fmt.Errorf("unexpected signing method: %v", token.Header["alg"])
		}
		return JwtKey, nil
	})

	if err != nil {
		return "", err
	}

	if !token.Valid {
		return "", fmt.Errorf("invalid token")
	}

	return claims.Username, nil
}

const (
	port = ":50051"
)

// Message représente un enregistrement dans la table 'message' de SurrealDB
type Message struct {
	ID        *models.RecordID `json:"id,omitempty"`
	User      string           `json:"user"`
	Message   string           `json:"message"`
	Channel   string           `json:"channel"`
	Timestamp string           `json:"timestamp"`
}

// User represents a user record in SurrealDB
type User struct {
	ID       *models.RecordID `json:"id,omitempty"`
	Username string           `json:"username"`
	Password string           `json:"password"` // Hashed password
}

type chatServer struct {
	pb.UnimplementedChatServer
	redisClient *redis.Client
	surrealDB   *surrealdb.DB
	onlineUsers map[string]map[string]bool // channel -> user -> onlineStatus
}

func newServer() *chatServer {
	// Initialisation de Redis
	rdb := redis.NewClient(&redis.Options{Addr: "localhost:6379"})

	// Test de la connexion Redis
	ctx := context.Background()
	if err := rdb.Ping(ctx).Err(); err != nil {
		log.Fatalf("Failed to connect to Redis: %v", err)
	}

	// Initialisation de SurrealDB
	db, err := surrealdb.New("ws://localhost:8000/rpc")
	if err != nil {
		log.Fatalf("Failed to connect to SurrealDB: %v", err)
	}

	// Configuration du namespace et de la base de données
	if err = db.Use("chatapp", "main"); err != nil {
		log.Fatalf("Failed to use SurrealDB namespace/database: %v", err)
	}

	// Authentification
	authData := &surrealdb.Auth{
		Username: "root",
		Password: "root",
	}

	token, err := db.SignIn(authData)
	if err != nil {
		log.Fatalf("Failed to signin to SurrealDB: %v", err)
	}

	if err = db.Authenticate(token); err != nil {
		log.Fatalf("Failed to authenticate with SurrealDB: %v", err)
	}

	log.Println("✅ Connexions SurrealDB et Redis établies")

	return &chatServer{redisClient: rdb, surrealDB: db, onlineUsers: make(map[string]map[string]bool)}
}

func (s *chatServer) Chat(stream pb.Chat_ChatServer) error {
	ctx := stream.Context()

	// Recevoir le premier message pour identifier l'utilisateur et le canal
	initEvent, err := stream.Recv()
	if err != nil {
		return err
	}

	// Validate the auth token from the initial ClientEvent
	authToken := initEvent.GetAuthToken()
	if authToken == "" {
		return fmt.Errorf("missing authentication token in initial event")
	}

	usernameFromToken, err := ValidateToken(authToken)
	if err != nil {
		return fmt.Errorf("invalid authentication token: %v", err)
	}

	var channel string
	var user string

	switch event := initEvent.Event.(type) {
	case *pb.ClientEvent_ChatMessage:
		channel = event.ChatMessage.Channel
		user = event.ChatMessage.User
		// Ensure the user from the token matches the user in the message
		if user != usernameFromToken {
			return fmt.Errorf("username mismatch: token user %s, message user %s", usernameFromToken, user)
		}
		// Process the initial chat message if it exists
		if event.ChatMessage.Message != "" {
			s.storeAndPublishChatMessage(ctx, event.ChatMessage)
		}
	default:
		return fmt.Errorf("first event must be a chat message to join a channel, received %T", event)
	}

	log.Printf("User %s joined channel %s", user, channel)

	// Annoncer la présence de l'utilisateur
	s.publishPresence(ctx, channel, user, true)
	defer s.publishPresence(ctx, channel, user, false)

	// Envoyer l'historique des messages du canal
	if err := s.sendMessageHistory(stream, channel); err != nil {
		log.Printf("Error sending message history: %v", err)
	}

	// Lancer les goroutines pour écouter les événements
	go s.subscribeToSurrealDBMessages(stream, channel)
	go s.subscribeToPresenceAndTyping(stream, channel)
	go s.subscribeToDirectMessages(stream, user)

	

	// Recevoir les messages du client
	for {
		clientEvent, err := stream.Recv()
		if err != nil {
			log.Printf("Client disconnected: %v", err)
			return nil // Déconnexion normale
		}

		// Validate the auth token for subsequent events
		authToken := clientEvent.GetAuthToken()
		if authToken == "" {
			log.Printf("Missing authentication token in subsequent event")
			continue // Or return an error, depending on desired behavior
		}

		usernameFromToken, err := ValidateToken(authToken)
		if err != nil {
			log.Printf("Invalid authentication token in subsequent event: %v", err)
			continue // Or return an error
		}

		// Ensure the user from the token matches the user in the message
		// This assumes the user field in ChatMessage/TypingEvent/DirectMessage is the sender
		var eventUser string
		switch event := clientEvent.Event.(type) {
		case *pb.ClientEvent_ChatMessage:
			eventUser = event.ChatMessage.User
		case *pb.ClientEvent_TypingEvent:
			eventUser = event.TypingEvent.User
		case *pb.ClientEvent_DirectMessage:
			eventUser = event.DirectMessage.Sender
		}

		if eventUser != usernameFromToken {
			log.Printf("Username mismatch in subsequent event: token user %s, event user %s", usernameFromToken, eventUser)
			continue // Or return an error
		}

		// Vérifier le type d'événement reçu
		switch event := clientEvent.Event.(type) {
		case *pb.ClientEvent_ChatMessage:
			s.storeAndPublishChatMessage(ctx, event.ChatMessage)
		case *pb.ClientEvent_TypingEvent:
			s.publishTypingEvent(ctx, event.TypingEvent.Channel, event.TypingEvent.User, event.TypingEvent.IsTyping)
		case *pb.ClientEvent_DirectMessage:
			s.handleDirectMessage(ctx, event.DirectMessage)
		default:
			log.Printf("Unknown client event type: %T", event)
		}
	}
}

// sendMessageHistory envoie l'historique des messages du canal
func (s *chatServer) sendMessageHistory(stream pb.Chat_ChatServer, channel string) error {
	query := `SELECT * FROM messages WHERE channel = $channel ORDER BY timestamp ASC LIMIT 50;`

	result, err := surrealdb.Query[[]Message](s.surrealDB, query, map[string]any{
		"channel": channel,
	})
	if err != nil {
		return fmt.Errorf("failed to fetch message history: %v", err)
	}

	if len(*result) > 0 && len((*result)[0].Result) > 0 {
		for _, msg := range (*result)[0].Result {
			chatMsg := &pb.ChatMessage{
				User:    msg.User,
				Message: msg.Message,
				Channel: msg.Channel,
			}
			if err := stream.Send(&pb.ServerEvent{
				Event: &pb.ServerEvent_ChatMessage{ChatMessage: chatMsg},
			}); err != nil {
				return err
			}
		}
	}

	return nil
}

// storeAndPublishChatMessage stocke le message dans SurrealDB et le publie sur Redis
func (s *chatServer) storeAndPublishChatMessage(ctx context.Context, msg *pb.ChatMessage) {
	messageToStore := Message{
		User:      msg.User,
		Message:   msg.Message,
		Channel:   msg.Channel,
		Timestamp: time.Now().Format(time.RFC3339),
	}

	// Insérer le message dans la table 'messages' de SurrealDB
	createdMsg, err := surrealdb.Create[Message](s.surrealDB, models.Table("messages"), messageToStore)
	if err != nil {
		log.Printf("Failed to store message in SurrealDB: %v", err)
		return
	}

	log.Printf("Message stored with ID: %v", createdMsg.ID)

	// Publier le message sur Redis pour les autres clients
	messageChannel := "messages:" + msg.Channel
	payload, err := json.Marshal(msg)
	if err != nil {
		log.Printf("Failed to marshal message for Redis publish: %v", err)
		return
	}
	log.Printf("Attempting to publish message to Redis channel %s: %s", messageChannel, string(payload))
	if err := s.redisClient.Publish(ctx, messageChannel, payload).Err(); err != nil {
		log.Printf("Failed to publish message to Redis: %v", err)
	} else {
		log.Printf("Successfully published message to Redis channel %s", messageChannel)
	}
}

func (s *chatServer) publishPresence(ctx context.Context, channel, user string, isOnline bool) {
	if s.onlineUsers[channel] == nil {
		s.onlineUsers[channel] = make(map[string]bool)
	}
	s.onlineUsers[channel][user] = isOnline

	pubsubChannel := "presence:" + channel
	event := &pb.PresenceEvent{Channel: channel, User: user, IsOnline: isOnline}
	payload, err := json.Marshal(event)
	if err != nil {
		log.Printf("Failed to marshal presence event: %v", err)
		return
	}

	if err := s.redisClient.Publish(ctx, pubsubChannel, payload).Err(); err != nil {
		log.Printf("Failed to publish presence event: %v", err)
	}

	s.broadcastUserList(ctx, channel)
}

func (s *chatServer) publishTypingEvent(ctx context.Context, channel, user string, isTyping bool) {
	pubsubChannel := "typing:" + channel
	event := &pb.TypingEvent{Channel: channel, User: user, IsTyping: isTyping}
	payload, err := json.Marshal(event)
	if err != nil {
		log.Printf("Failed to marshal typing event: %v", err)
		return
	}

	if err := s.redisClient.Publish(ctx, pubsubChannel, payload).Err(); err != nil {
		log.Printf("Failed to publish typing event: %v", err)
	}
}

func (s *chatServer) handleDirectMessage(ctx context.Context, dm *pb.DirectMessage) {
	log.Printf("Received direct message from %s to %s: %s", dm.Sender, dm.Recipient, dm.Message)

	// Publish the direct message to the recipient's Redis channel
	dmChannel := "dm:" + dm.Recipient
	payload, err := json.Marshal(dm)
	if err != nil {
		log.Printf("Failed to marshal direct message: %v", err)
		return
	}

	if err := s.redisClient.Publish(ctx, dmChannel, payload).Err(); err != nil {
		log.Printf("Failed to publish direct message to Redis: %v", err)
	}
}

func (s *chatServer) broadcastUserList(ctx context.Context, channel string) {
	users := []string{}
	for user, online := range s.onlineUsers[channel] {
		if online {
			users = append(users, user)
		}
	}

	userListEvent := &pb.UserListEvent{Channel: channel, Users: users}
	payload, err := json.Marshal(userListEvent)
	if err != nil {
		log.Printf("Failed to marshal user list event: %v", err)
		return
	}

	if err := s.redisClient.Publish(ctx, "user_list:"+channel, payload).Err(); err != nil {
		log.Printf("Failed to publish user list event: %v", err)
	}
}

// subscribeToSurrealDBMessages utilise Redis pour les messages en temps réel au lieu des LIVE queries
func (s *chatServer) subscribeToSurrealDBMessages(stream pb.Chat_ChatServer, channel string) {
	ctx := stream.Context()

	// Utiliser Redis pour les messages en temps réel (plus fiable que les LIVE queries pour l'instant)
	messageChannel := "messages:" + channel
	pubsub := s.redisClient.Subscribe(ctx, messageChannel)
	defer pubsub.Close()

	log.Printf("Subscribed to Redis channel: %s", messageChannel)

	for {
		select {
		case <-ctx.Done():
			// Le client s'est déconnecté
			return
		case msg := <-pubsub.Channel():
			if msg == nil {
				continue
			}

			// Décoder le message
			var chatMsg pb.ChatMessage
			log.Printf("Received message from Redis channel %s: %s", msg.Channel, msg.Payload)
			if err := json.Unmarshal([]byte(msg.Payload), &chatMsg); err != nil {
				log.Printf("Failed to unmarshal message from Redis: %v", err)
				continue
			}

			// Envoyer le message au client via le stream gRPC
			log.Printf("Sending message to client: %+v", chatMsg)
			if err := stream.Send(&pb.ServerEvent{
				Event: &pb.ServerEvent_ChatMessage{ChatMessage: &chatMsg},
			}); err != nil {
				log.Printf("Error sending message to client: %v", err)
				return
			}
		}
	}
}

func (s *chatServer) subscribeToPresenceAndTyping(stream pb.Chat_ChatServer, channel string) {
	ctx := stream.Context()

	// S'abonner aux événements de présence, de frappe et de liste d'utilisateurs
	presenceChannel := "presence:" + channel
	typingChannel := "typing:" + channel
	userListChannel := "user_list:" + channel

	pubsub := s.redisClient.Subscribe(ctx, presenceChannel, typingChannel, userListChannel)
	defer pubsub.Close()

	for {
		select {
		case <-ctx.Done():
			return
		case msg := <-pubsub.Channel():
			if msg == nil {
				continue
			}

			// Essayer de décoder comme événement de présence
			var presenceEvent pb.PresenceEvent
			if json.Unmarshal([]byte(msg.Payload), &presenceEvent) == nil && presenceEvent.User != "" {
				if err := stream.Send(&pb.ServerEvent{
					Event: &pb.ServerEvent_PresenceEvent{PresenceEvent: &presenceEvent},
				}); err != nil {
					log.Printf("Error sending presence event: %v", err)
					return
				}
				continue
			}

			// Essayer de décoder comme événement de frappe
			var typingEvent pb.TypingEvent
			if json.Unmarshal([]byte(msg.Payload), &typingEvent) == nil && typingEvent.User != "" {
				if err := stream.Send(&pb.ServerEvent{
					Event: &pb.ServerEvent_TypingEvent{TypingEvent: &typingEvent},
				}); err != nil {
					log.Printf("Error sending typing event: %v", err)
					return
				}
				continue
			}

			// Essayer de décoder comme événement de liste d'utilisateurs
			var userListEvent pb.UserListEvent
			if json.Unmarshal([]byte(msg.Payload), &userListEvent) == nil && len(userListEvent.Users) > 0 {
				if err := stream.Send(&pb.ServerEvent{
					Event: &pb.ServerEvent_UserListEvent{UserListEvent: &userListEvent},
				}); err != nil {
					log.Printf("Error sending user list event: %v", err)
					return
				}
			}
		}
	}
}

func (s *chatServer) subscribeToDirectMessages(stream pb.Chat_ChatServer, user string) {
	ctx := stream.Context()

	dmChannel := "dm:" + user
	pubsub := s.redisClient.Subscribe(ctx, dmChannel)
	defer pubsub.Close()

	log.Printf("Subscribed to Redis DM channel: %s for user %s", dmChannel, user)

	for {
		select {
		case <-ctx.Done():
			return
		case msg := <-pubsub.Channel():
			if msg == nil {
				continue
			}

			var dm pb.DirectMessage
			if err := json.Unmarshal([]byte(msg.Payload), &dm); err != nil {
				log.Printf("Failed to unmarshal direct message from Redis: %v", err)
				continue
			}

			log.Printf("Sending direct message to client %s: %+v", user, dm)
			if err := stream.Send(&pb.ServerEvent{
				Event: &pb.ServerEvent_DirectMessage{DirectMessage: &dm},
			}); err != nil {
				log.Printf("Error sending direct message to client %s: %v", user, err)
				return
			}
		}
	}
}

func main() {
	lis, err := net.Listen("tcp", port)
	if err != nil {
		log.Fatalf("failed to listen: %v", err)
	}

	creds, err := credentials.NewServerTLSFromFile("server.crt", "server.key")
	if err != nil {
		log.Fatalf("Failed to generate credentials: %v", err)
	}

	grpcServer := grpc.NewServer(
		grpc.Creds(creds),
		grpc.UnaryInterceptor(AuthInterceptor),
		grpc.StreamInterceptor(AuthStreamInterceptor),
	)
	chatServerInstance := newServer()

	// Fermer proprement les connexions à l'arrêt
	defer func() {
		if err := chatServerInstance.surrealDB.Close(); err != nil {
			log.Printf("Error closing SurrealDB: %v", err)
		}
		if err := chatServerInstance.redisClient.Close(); err != nil {
			log.Printf("Error closing Redis: %v", err)
		}
	}()

	pb.RegisterChatServer(grpcServer, chatServerInstance)
	pb.RegisterAuthServer(grpcServer, newAuthServer(chatServerInstance.surrealDB))
	log.Printf("🚀 Chat Server listening at %v", lis.Addr())

	if err := grpcServer.Serve(lis); err != nil {
		log.Fatalf("failed to serve: %v", err)
	}
}
