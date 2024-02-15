package main

// Code originally developed by sungo (https://sungo.io)
// Distributed under the terms of the 0BSD license https://opensource.org/licenses/0BSD

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/alecthomas/kong"
	"github.com/mattn/go-isatty"
	"github.com/mattn/go-mastodon"
	toml "github.com/pelletier/go-toml"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
)

const (
	ourName         = "tootfyre"
	ourVersion      = "0.0.1"
	ourURL          = "https://git.sr.ht/~sungo/tootfyre.git"
	timeMax         = -(30 * (24 * (60 * (60 * time.Second))))
	defaultCount    = "10"
	paginationLimit = 200
)

type (
	Cmd struct {
		Config            string `kong:"required,name='config',help='path to config file',type='existingfile'"`
		Slow              bool   `kong:"name='slow',default=true,negatable,help='delete stuff at a slow pace to be nice to your instance and the fediverse in general (default on)'"`
		ExcludeReplies    bool   `kong:"name='exclude-replies',default=true,negatable,help='exclude replies from filter (default true)'"`
		ExcludePinned     bool   `kong:"name='exclude-pinned',default=true,negatable,help='exclude toots that are pinned to the profile'"`
		ExcludeBookmarked bool   `kong:"name='exclude-bookmarked',default=true,negatable,help='exclude toots that are bookmarked'"`
		ExcludePublic     bool   `kong:"name='exclude-public',default=false,negatable,help='exclude toots with a visibility of public'"`
		ExcludeBoosts     bool   `kong:"name='exclude-boosts',default=false,negatable,help='exclude boosted'"`
		ExcludeDirect     bool   `kong:"name='exclude-dms',default=true,negatable,help='exclude DMs (default on)'"`
		Count             int    `kong:"name='count',default='${defaultCount}',help='the number of toots to act on in this run'"`
		DryRun            bool   `kong:"name='dry-run',short='n',default=false,help='do not do the thing just log about the thing'"`
		Quiet             bool   `kong:"name='quiet',default=false,help='only log about errors and the stuff we deleted'"`
		BurnItAll         bool   `kong:"name='burn-it-all',default=false,help='ignore all exclusions, set no time limit, watch the world burn. slowly'"`
	}
	Config struct {
		Server       string
		ClientID     string
		ClientSecret string
		AccessToken  string
	}
)

func main() {
	ctx := kong.Parse(&Cmd{}, kong.Vars{
		"defaultCount": defaultCount,
	})
	err := ctx.Run()
	ctx.FatalIfErrorf(err)
}

func (cmd *Cmd) LoadConfig(path string) (Config, error) {
	config := Config{}

	if _, err := os.Stat(path); os.IsNotExist(err) {
		return config, err
	} else if err != nil {
		return config, err
	}

	file, err := os.Open(path)
	if err != nil {
		return config, err
	}
	defer file.Close()

	if err := toml.NewDecoder(file).Decode(&config); err != nil {
		return config, err
	}

	switch {
	case config.Server == "":
		return config, errors.New("'server' is required in config")
	case config.AccessToken == "":
		return config, errors.New("'accesstoken' is required in config. get this from headers in an existing UI client")
	}

	return config, nil
}

func (cmd *Cmd) WriteConfig(config Config) error {
	file, err := os.Create(cmd.Config)
	if err != nil {
		return err
	}
	defer file.Close()
	enc := toml.NewEncoder(file)
	if err := enc.Encode(config); err != nil {
		return err
	}

	return nil
}

func (cmd *Cmd) Rest(secs int) {
	if cmd.Slow {
		log.Debug().Int("seconds", secs).Msg("slow mode engaged. resting")
		time.Sleep(time.Duration(secs) * time.Second)
	}
}

func (cmd *Cmd) Run() error {
	if isatty.IsTerminal(os.Stdout.Fd()) {
		log.Logger = log.Output(zerolog.ConsoleWriter{Out: os.Stderr})
	}
	if cmd.Quiet {
		zerolog.SetGlobalLevel(zerolog.InfoLevel)
	} else {
		zerolog.SetGlobalLevel(zerolog.DebugLevel)
	}

	var (
		endTime     = time.Now().Add(timeMax)
		ctx, cancel = context.WithCancel(context.Background())
	)
	defer cancel()

	config, err := cmd.LoadConfig(cmd.Config)
	if err != nil {
		return err
	}

	if (config.ClientID == "") || (config.ClientSecret == "") {
		log.Debug().Msg("getting app credentials")

		app, err := mastodon.RegisterApp(ctx, &mastodon.AppConfig{
			Server:     config.Server,
			ClientName: ourName,
			Scopes:     "read write",
			Website:    ourURL,
		})
		if err != nil {
			return err
		}
		config.ClientID = app.ClientID
		config.ClientSecret = app.ClientSecret

		cmd.WriteConfig(config)
	}

	c := mastodon.NewClient(&mastodon.Config{
		Server:       config.Server,
		ClientID:     config.ClientID,
		ClientSecret: config.ClientSecret,
		AccessToken:  config.AccessToken,
	})
	c.UserAgent = fmt.Sprintf("%s/%s", ourName, ourVersion)

	log.Debug().Msg("getting account info")
	account, err := c.GetAccountCurrentUser(ctx)
	if err != nil {
		return err
	}

	var (
		pg       mastodon.Pagination
		toDelete = make([]*mastodon.Status, 0)
	)

	pg.Limit = int64(paginationLimit)
	log.Debug().Int("max_toots", cmd.Count).Time("before", endTime).Msg("starting run")

LOOP:
	for {
		log.Debug().Msgf("Polling for toots before ID %s, max of %d", pg.MaxID, pg.Limit)
		statuses, err := c.GetAccountStatuses(ctx, account.ID, &pg)
		if err != nil {
			return err
		}

		log.Debug().Int("count", len(statuses)).Msg("found statuses to consider")

		for id := range statuses {
			status := statuses[id]
			logger := log.With().
				Interface("id", status.ID).
				Str("url", status.URL).
				Time("created", status.CreatedAt).
				Str("content", status.Content).
				Bool("is_reply", status.InReplyToID != nil).
				Bool("is_boost", status.Reblog != nil).
				Str("visibility", status.Visibility).
				Bool("pinned", status.Pinned == true).
				Bool("bookmarked", status.Bookmarked == true).
				Bool("favstarred", status.Favourited == true).
				Logger()

			if !cmd.BurnItAll {
				if !status.CreatedAt.Before(endTime) {
					logger.Debug().Msg("skipping for being too young")
					continue
				}
				switch {
				case cmd.ExcludePinned && status.Pinned == true:
					logger.Debug().Msg("skipping due to pinned")
					continue
				case cmd.ExcludePublic && status.Visibility == mastodon.VisibilityPublic:
					logger.Debug().Msg("skipping due to being public")
					continue
				case cmd.ExcludeBookmarked && status.Bookmarked == true:
					logger.Debug().Msg("skipping due to being bookmarked")
					continue
				case cmd.ExcludeBoosts && status.Reblog != nil:
					logger.Debug().Msg("skipping due to being a boost")
					continue
				case cmd.ExcludeReplies && status.InReplyToID != nil:
					logger.Debug().Msg("skipping due to being a reply")
					continue
				case cmd.ExcludeDirect && status.Visibility == mastodon.VisibilityDirectMessage:
					logger.Debug().Msg("skipping due to being a DM")
					continue
				}
			}

			toDelete = append(toDelete, status)
			if len(toDelete) >= cmd.Count {
				break LOOP
			}
		}

		if pg.MaxID == "" {
			break LOOP
		}

		if pg.MinID == "" {
			break LOOP
		}

		pg.SinceID = ""
		pg.MinID = ""
		pg.Limit = paginationLimit

		cmd.Rest(5)
	}
	log.Info().Msgf("Found %d statuses to delete", len(toDelete))

	for idx := len(toDelete) - 1; idx >= 0; idx-- {
		status := toDelete[idx]
		logger := log.With().
			Interface("id", status.ID).
			Str("url", status.URL).
			Time("created", status.CreatedAt).
			Str("content", status.Content).
			Bool("is_reply", status.InReplyToID != nil).
			Bool("is_boost", status.Reblog != nil).
			Str("visibility", status.Visibility).
			Bool("pinned", status.Pinned == true).
			Bool("bookmarked", status.Bookmarked == true).
			Bool("favstarred", status.Favourited == true).
			Logger()

		if cmd.DryRun {
			logger.Warn().Msg("dry run: would delete status otherwise")
		} else {
			logger.Info().Msg("deleting status")
			if err := c.DeleteStatus(ctx, status.ID); err != nil {
				logger.Error().Err(err).Msg("error when deleting")
			}

			cmd.Rest(15)
		}
	}

	return nil
}
