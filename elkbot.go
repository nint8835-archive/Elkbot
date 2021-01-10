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
	elasticsearch "github.com/elastic/go-elasticsearch/v8"
	"github.com/elastic/go-elasticsearch/v8/esapi"
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
	// TODO: Handle channels with > 100 messages
	messages, err := session.ChannelMessages(args.ChannelID, 100, "", "", "")
	if err != nil {
		log.Error().Err(err).Msg("Failed to retrieve message history")
		session.ChannelMessageSend(message.ChannelID, fmt.Sprintf("```\nfailed to retrieve message history: %s\n```", err.Error()))
		return
	}

	for _, historyMessage := range messages {
		documentBody := map[string]interface{}{
			"content":   historyMessage.Content,
			"author_id": historyMessage.Author.ID,
			"timestamp": historyMessage.Timestamp,
		}

		reqBody, _ := json.Marshal(documentBody)

		req := esapi.IndexRequest{
			Index:      "messages",
			DocumentID: historyMessage.ID,
			Body:       bytes.NewReader(reqBody),
			Refresh:    "true",
		}

		resp, err := req.Do(context.Background(), esClient)
		if err != nil {
			log.Error().Err(err).Msg("Error submitting message to Elasticsearch")
		}
		defer resp.Body.Close()

		if resp.IsError() {
			log.Error().Str("status", resp.Status()).Msg("Error submitting message to Elasticsearch")
		}
	}

	session.ChannelMessageSend(message.ChannelID, fmt.Sprintf("```go\n%#v\n```", args))
}
