package main

import (
	"bufio"
	"context"
	"fmt"
	"log"
	"os"
	"strings"
	"crypto/tls"
	"crypto/x509"
	"io/ioutil"

	pb "grpc_golang/proto"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
)

const (
	address = "localhost:50051"
)

func main() {
	// Load the CA certificate
	caCert, err := ioutil.ReadFile("server/server.crt")
	if err != nil {
		log.Fatalf("Failed to read CA certificate: %v", err)
	}
	caCertPool := x509.NewCertPool()
	caCertPool.AppendCertsFromPEM(caCert)

	// Create TLS credentials
	creds := credentials.NewTLS(&tls.Config{
		RootCAs: caCertPool,
		ServerName: "localhost", // This must match the CN in your server.crt
		InsecureSkipVerify: true, // WARNING: Do not use in production!
	})
	conn, err := grpc.NewClient(address, grpc.WithTransportCredentials(creds))
	if err != nil {
		log.Fatalf("did not connect: %v", err)
	}
	defer conn.Close()

	// Auth Client
	authClient := pb.NewAuthClient(conn)

	scanner := bufio.NewScanner(os.Stdin)
	fmt.Print("Do you want to (L)ogin or (R)egister? ")
	scanner.Scan()
	choice := strings.ToLower(scanner.Text())

	var username, password string

	if choice == "r" {
		fmt.Print("Enter desired username: ")
		scanner.Scan()
		username = scanner.Text()

		fmt.Print("Enter desired password: ")
		scanner.Scan()
		password = scanner.Text()

		regRes, err := authClient.Register(context.Background(), &pb.RegisterRequest{Username: username, Password: password})
		if err != nil {
			log.Fatalf("Registration failed: %v", err)
		}

		if !regRes.GetSuccess() {
			log.Fatalf("Registration error: %s", regRes.GetError())
		}

		log.Printf("Registration successful for %s. Please login.", username)
		// After successful registration, fall through to login
	}

	fmt.Print("Enter your username: ")
	scanner.Scan()
	username = scanner.Text()

	fmt.Print("Enter your password: ")
	scanner.Scan()
	password = scanner.Text()

	loginRes, err := authClient.Login(context.Background(), &pb.LoginRequest{Username: username, Password: password})
	if err != nil {
		log.Fatalf("Login failed: %v", err)
	}

	if loginRes.GetError() != "" {
		log.Fatalf("Login error: %s", loginRes.GetError())
	}

	authToken := loginRes.GetToken()
	log.Printf("Successfully logged in. Token: %s", authToken)

	// Chat Client
	chatClient := pb.NewChatClient(conn)

	fmt.Print("Enter channel to join: ")
	scanner.Scan()
	channel := scanner.Text()

	stream, err := chatClient.Chat(context.Background())
	if err != nil {
		log.Fatalf("Error creating stream: %v", err)
	}

	// Envoyer le message d'initialisation avec le token
	joinMsg := &pb.ChatMessage{User: username, Channel: channel, Message: "has joined"}
	initialEvent := &pb.ClientEvent{AuthToken: authToken, Event: &pb.ClientEvent_ChatMessage{ChatMessage: joinMsg}}
	if err := stream.Send(initialEvent); err != nil {
		log.Fatalf("Failed to join channel: %v", err)
	}

	// Goroutine pour recevoir les événements du serveur
	go func() {
		for {
			serverEvent, err := stream.Recv()
			if err != nil {
				log.Fatalf("Failed to receive an event: %v", err)
			}

			switch e := serverEvent.Event.(type) {
			case *pb.ServerEvent_ChatMessage:
				msg := e.ChatMessage
				fmt.Printf("[%s] %s: %s\n", msg.Channel, msg.User, msg.Message)
			case *pb.ServerEvent_PresenceEvent:
				presence := e.PresenceEvent
				status := "left"
				if presence.IsOnline {
					status = "joined"
				}
				// Ne pas afficher sa propre notification de join
				if presence.User == username {
					continue
				}
				fmt.Printf("*** %s has %s the channel %s ***\n", presence.User, status, presence.Channel)
			case *pb.ServerEvent_TypingEvent:
				typing := e.TypingEvent
				if typing.IsTyping {
					fmt.Printf("*** %s is typing in %s ***\n", typing.User, typing.Channel)
				} else {
					fmt.Printf("*** %s stopped typing in %s ***\n", typing.User, typing.Channel)
				}
			case *pb.ServerEvent_UserListEvent:
				userList := e.UserListEvent
				fmt.Printf("*** Online users in %s: %v ***\n", userList.Channel, userList.Users)
			case *pb.ServerEvent_DirectMessage:
				dm := e.DirectMessage
				fmt.Printf("[DM from %s to %s]: %s\n", dm.Sender, dm.Recipient, dm.Message)
			}
		}
	}()

	// Boucle pour envoyer les messages de l'utilisateur
	fmt.Printf("Joined channel %s. You can now start sending messages. Type /dm <recipient> <message> for direct messages.\n", channel)
	for scanner.Scan() {
		input := scanner.Text()
		if input == "" {
			continue
		}

		if len(input) > 3 && input[:3] == "/dm" {
			parts := strings.SplitN(input[4:], " ", 2)
			if len(parts) < 2 {
				fmt.Println("Usage: /dm <recipient> <message>")
				continue
			}
			recipient := parts[0]
			dmMessage := parts[1]
			dm := &pb.DirectMessage{Sender: username, Recipient: recipient, Message: dmMessage, Channel: channel}
			clientEvent := &pb.ClientEvent{AuthToken: authToken, Event: &pb.ClientEvent_DirectMessage{DirectMessage: dm}}
			if err := stream.Send(clientEvent); err != nil {
				log.Printf("Failed to send direct message: %v", err)
			}
		} else {
			msg := input
			chatMsg := &pb.ChatMessage{User: username, Channel: channel, Message: msg}
			clientEvent := &pb.ClientEvent{AuthToken: authToken, Event: &pb.ClientEvent_ChatMessage{ChatMessage: chatMsg}}
			if err := stream.Send(clientEvent); err != nil {
				log.Printf("Failed to send message: %v", err)
			}
		}
	}
}

