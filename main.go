package main

import (
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/drdeee/whatsapp-trello-bridge/platforms"
	serv "github.com/drdeee/whatsapp-trello-bridge/server"
	store "github.com/drdeee/whatsapp-trello-bridge/store"
	"github.com/joho/godotenv"
)

func main() {
	err := godotenv.Load()
	if err != nil {
		fmt.Println("Error loading .env file")
	}

	store := store.RequestStore{}
	store.Init()

	trelloClient := platforms.TrelloClient{}
	trelloClient.Init()

	whatsappClient := platforms.WhatsAppClient{}

	// start server
	var server = serv.Server{}
	next := make(chan bool, 1)
	go server.Start(next, &trelloClient, &whatsappClient, &store)
	<-next

	// check webhooks
	trelloClient.CheckTrelloWebhooks()

	// init whatsapp
	whatsappClient.Init(&trelloClient, &store)

	c := make(chan os.Signal, 1)
	signal.Notify(c, os.Interrupt, syscall.SIGTERM)
	<-c
	whatsappClient.Client.Disconnect()
}
