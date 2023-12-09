package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/alecthomas/kong"
	"github.com/mattn/go-mastodon"
	toml "github.com/pelletier/go-toml"
)

const (
	ourName      = "tootfyre"
	ourVersion   = "0.0.1"
	ourURL       = "https://git.sr.ht/~sungo/tootfyre.git"
	timeMax      = -(30 * (24 * (60 * (60 * time.Second))))
	defaultCount = "10"
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
		Count             int    `kong:"name='count',default='${defaultCount}',help='the number of toots to act on in this run'"`
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
		fmt.Printf("slow mode engaged. resting for %d seconds\n", secs)
		time.Sleep(time.Duration(secs) * time.Second)
	}
}

func (cmd *Cmd) Run() error {
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

	account, err := c.GetAccountCurrentUser(ctx)
	if err != nil {
		return err
	}

	var (
		pg    mastodon.Pagination
		found int
	)
	fmt.Printf("Starting run. Will delete a max of %d toots from before %s\n", cmd.Count, endTime)

	for {
		pg.Limit = 100
		fmt.Printf("Polling for toots before ID %s, max of %d\n", pg.MaxID, pg.Limit)
		statuses, err := c.GetAccountStatuses(ctx, account.ID, &pg)
		if err != nil {
			return err
		}

		if pg.MaxID == "" {
			return nil
		}

		fmt.Printf("==> Found %d statuses to consider\n", len(statuses))

		for id := range statuses {
			var (
				deleted bool
				status  = statuses[id]
			)
			if status.CreatedAt.Before(endTime) {
				switch {
				case cmd.ExcludePinned && status.Pinned == true:
					continue
				case cmd.ExcludePublic && status.Visibility == "public":
					continue
				case cmd.ExcludeBookmarked && status.Bookmarked == true:
					continue
				case cmd.ExcludeBoosts && status.Reblog != nil:
					continue
				case cmd.ExcludeReplies && status.InReplyToID != nil:
					continue
				}
				found++
				fmt.Printf("==> Deleting [ %s // %s ] %s - %s\n", status.ID, status.URL, status.CreatedAt, status.Content)
				if err := c.DeleteStatus(ctx, status.ID); err != nil {
					return err
				}
				deleted = true

			}
			if found >= cmd.Count {
				return nil
			}

			if deleted {
				cmd.Rest(2)
			}
		}
		pg.SinceID = ""
		pg.MinID = ""
		pg.Limit = 100

		cmd.Rest(5)
	}

	return nil
}
