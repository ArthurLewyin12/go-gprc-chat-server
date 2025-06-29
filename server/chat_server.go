package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"time"

	pb "grpc_golang/proto"

	"github.com/redis/go-redis/v9"
	"github.com/surrealdb/surrealdb.go"
	"github.com/surrealdb/surrealdb.go/pkg/models"
	"google.golang.org/grpc"
)

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

type chatServer struct {
	pb.UnimplementedChatServer
	redisClient *redis.Client
	surrealDB   *surrealdb.DB
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

	return &chatServer{redisClient: rdb, surrealDB: db}
}

func (s *chatServer) Chat(stream pb.Chat_ChatServer) error {
	ctx := stream.Context()

	// Recevoir le premier message pour identifier l'utilisateur et le canal
	initEvent, err := stream.Recv()
	if err != nil {
		return err
	}

	var channel string
	var user string

	switch event := initEvent.Event.(type) {
	case *pb.ClientEvent_ChatMessage:
		channel = event.ChatMessage.Channel
		user = event.ChatMessage.User
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

	

	// Recevoir les messages du client
	for {
		clientEvent, err := stream.Recv()
		if err != nil {
			log.Printf("Client disconnected: %v", err)
			return nil // Déconnexion normale
		}

		// Vérifier le type d'événement reçu
					switch event := clientEvent.Event.(type) {
			case *pb.ClientEvent_ChatMessage:
				s.storeAndPublishChatMessage(ctx, event.ChatMessage)
			case *pb.ClientEvent_TypingEvent:
				s.publishTypingEvent(ctx, event.TypingEvent.Channel, event.TypingEvent.User, event.TypingEvent.IsTyping)
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

	// S'abonner aux événements de présence et de frappe
	presenceChannel := "presence:" + channel
	typingChannel := "typing:" + channel

	pubsub := s.redisClient.Subscribe(ctx, presenceChannel, typingChannel)
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
			}
		}
	}
}

func main() {
	lis, err := net.Listen("tcp", port)
	if err != nil {
		log.Fatalf("failed to listen: %v", err)
	}

	grpcServer := grpc.NewServer()
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
	log.Printf("🚀 Chat Server listening at %v", lis.Addr())

	if err := grpcServer.Serve(lis); err != nil {
		log.Fatalf("failed to serve: %v", err)
	}
}
