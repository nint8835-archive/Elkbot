package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/bwmarrin/discordgo"
	elasticsearch "github.com/elastic/go-elasticsearch/v7"
	"github.com/elastic/go-elasticsearch/v7/esapi"
	"github.com/joho/godotenv"
	"github.com/kelseyhightower/envconfig"
	"github.com/nint8835/parsley"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
)

// Config represents the config that Elkbot will use to run
type Config struct {
	Prefix   string        `default:"elk!"`
	Token    string        `required:"true"`
	LogLevel zerolog.Level `default:"1" split_words:"true"`
}

var session *discordgo.Session
var esClient *elasticsearch.Client

func _PaginateMessages(channelID string, callback func([]*discordgo.Message) error) error {
	messages, err := session.ChannelMessages(channelID, 100, "", "", "")
	if err != nil {
		return fmt.Errorf("error fetching messages from Discord: %w", err)
	}
	for len(messages) > 0 {
		err = callback(messages)
		if err != nil {
			return fmt.Errorf("error when processing messages: %w", err)
		}
		log.Debug().Int("count", len(messages)).Msg("Finished processing page")
		log.Debug().Str("before", messages[len(messages)-1].ID).Msg("Fetching next page of messages")
		messages, err = session.ChannelMessages(channelID, 100, messages[len(messages)-1].ID, "", "")
		if err != nil {
			return fmt.Errorf("error fetching messages from Discord: %w", err)
		}
	}

	return nil
}

func _InsertIndex(data map[string]interface{}, indexName string, documentID string) error {
	reqBody, _ := json.Marshal(data)

	req := esapi.IndexRequest{
		Index:      indexName,
		DocumentID: documentID,
		Body:       bytes.NewReader(reqBody),
		Refresh:    "true",
	}

	resp, err := req.Do(context.Background(), esClient)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.IsError() {
		return fmt.Errorf("got status code %s", resp.Status())
	}

	return nil
}

func _IngestAttachment(attachment *discordgo.MessageAttachment, message *discordgo.Message) error {
	documentBody := map[string]interface{}{
		"filename":   attachment.Filename,
		"height":     attachment.Height,
		"width":      attachment.Width,
		"size":       attachment.Size,
		"url":        attachment.URL,
		"proxy_url":  attachment.ProxyURL,
		"message_id": message.ID,
		"timestamp":  message.Timestamp,
	}

	err := _InsertIndex(documentBody, "attachments", attachment.ID)
	if err != nil {
		return fmt.Errorf("error ingesting attachment: %w", err)
	}

	return nil
}

func _IngestMessage(message *discordgo.Message) error {
	documentBody := map[string]interface{}{
		"content":    message.Content,
		"channel_id": message.ChannelID,
		"author_id":  message.Author.ID,
		"timestamp":  message.Timestamp,
	}

	err := _InsertIndex(documentBody, "messages", message.ID)
	if err != nil {
		if err != nil {
			return fmt.Errorf("error ingesting message: %w", err)
		}
	}

	for _, attachment := range message.Attachments {
		err = _IngestAttachment(attachment, message)
		if err != nil {
			return err
		}
	}

	return nil
}

func main() {
	err := godotenv.Load()
	if err != nil {
		fmt.Printf("Failed to load .env file: %s\n", err.Error())
	}

	var config Config
	err = envconfig.Process("elkbot", &config)
	if err != nil {
		panic(fmt.Errorf("error loading config: %w", err))
	}

	zerolog.SetGlobalLevel(config.LogLevel)
	log.Logger = log.Output(zerolog.ConsoleWriter{Out: os.Stderr})

	log.Debug().Msg("Creating Elasticsearch client")
	esClient, err = elasticsearch.NewDefaultClient()
	if err != nil {
		panic(fmt.Errorf("error creating Elasticsearch client: %w", err))
	}
	log.Debug().Msg("Elasticsearch client created")

	log.Debug().Msg("Creating Discord session")
	session, err = discordgo.New("Bot " + config.Token)
	if err != nil {
		panic(fmt.Errorf("error creating Discord session: %w", err))
	}
	session.Identify.Intents = discordgo.MakeIntent(discordgo.IntentsGuildMessages)
	log.Debug().Msg("Discord session created")

	log.Debug().Msg("Creating command parser")
	parser := parsley.New(config.Prefix)
	parser.RegisterHandler(session)
	log.Debug().Msg("Parser created")

	parser.NewCommand("ingest", "Ingest a backlog of messages from a certain channel.", _IngestHandler)

	log.Debug().Msg("Opening Discord connection")
	err = session.Open()
	if err != nil {
		log.Error().Err(err).Msg("Error opening connection")
		return
	}
	log.Debug().Msg("Discord connection open")

	log.Info().Msg("Elkbot is now running, press CTRL-C to exit.")
	sc := make(chan os.Signal, 1)
	signal.Notify(sc, syscall.SIGINT, syscall.SIGTERM, os.Interrupt, os.Kill)
	<-sc
	log.Info().Msg("Quitting Elkbot")

	err = session.Close()
	if err != nil {
		log.Error().Err(err).Msg("Error closing Discord connection")
	}
}

type _IngestArgs struct {
	ChannelID string `description:"ID of the channel to ingest logs from."`
}

func _IngestHandler(message *discordgo.MessageCreate, args _IngestArgs) {
	if message.Author.ID != "106162668032802816" {
		log.Warn().Str("author_id", message.Author.ID).Msg("User does not have access to this command")
		return
	}

	err := _PaginateMessages(args.ChannelID, func(messages []*discordgo.Message) error {
		for _, historyMessage := range messages {
			err := _IngestMessage(historyMessage)
			if err != nil {
				return err
			}
		}
		return nil
	})

	if err != nil {
		log.Error().Err(err).Msg("Error ingesting messages")
		session.ChannelMessageSend(message.ChannelID, fmt.Sprintf("```\n%s\n```", err.Error()))
	} else {
		session.ChannelMessageSend(message.ChannelID, "Channel messages successfully ingested.")
	}
}
