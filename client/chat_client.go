package main

import (
	"bufio"
	"context"
	"fmt"
	"log"
	"os"

	pb "grpc_golang/proto"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

const (
	address = "localhost:50051"
)

func main() {
	conn, err := grpc.NewClient(address, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		log.Fatalf("did not connect: %v", err)
	}
	defer conn.Close()

	client := pb.NewChatClient(conn)

	scanner := bufio.NewScanner(os.Stdin)
	fmt.Print("Enter your name: ")
	scanner.Scan()
	user := scanner.Text()

	fmt.Print("Enter channel to join: ")
	scanner.Scan()
	channel := scanner.Text()

	stream, err := client.Chat(context.Background())
	if err != nil {
		log.Fatalf("Error creating stream: %v", err)
	}

	// Envoyer le message d'initialisation
	joinMsg := &pb.ChatMessage{User: user, Channel: channel, Message: "has joined"}
	initialEvent := &pb.ClientEvent{Event: &pb.ClientEvent_ChatMessage{ChatMessage: joinMsg}}
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
				if presence.User == user {
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
			}
		}
	}()

	// Boucle pour envoyer les messages de l'utilisateur
	fmt.Printf("Joined channel %s. You can now start sending messages.\n", channel)
	for scanner.Scan() {
		msg := scanner.Text()
		if msg == "" {
			continue
		}
		chatMsg := &pb.ChatMessage{User: user, Channel: channel, Message: msg}
		clientEvent := &pb.ClientEvent{Event: &pb.ClientEvent_ChatMessage{ChatMessage: chatMsg}}
		if err := stream.Send(clientEvent); err != nil {
			log.Printf("Failed to send message: %v", err)
		}
	}
}
