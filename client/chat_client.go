package main

import (
	"bufio"
	"context"
	"fmt"
	"log"
	"os"
	"strings"

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
			dm := &pb.DirectMessage{Sender: user, Recipient: recipient, Message: dmMessage, Channel: channel}
			clientEvent := &pb.ClientEvent{Event: &pb.ClientEvent_DirectMessage{DirectMessage: dm}}
			if err := stream.Send(clientEvent); err != nil {
				log.Printf("Failed to send direct message: %v", err)
			}
		} else {
			msg := input
			chatMsg := &pb.ChatMessage{User: user, Channel: channel, Message: msg}
			clientEvent := &pb.ClientEvent{Event: &pb.ClientEvent_ChatMessage{ChatMessage: chatMsg}}
			if err := stream.Send(clientEvent); err != nil {
				log.Printf("Failed to send message: %v", err)
			}
		}
	}
}
