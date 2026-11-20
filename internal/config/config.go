// Package config loads the bot configuration from a JSON file and
// environment variables.
//
// The LLM part uses the same format as opencode.json: a "provider" block and
// a "model" field written "<provider>/<model>". Strings may reference
// environment variables with {env:NAME}, as in opencode.
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strings"

	"github.com/littlejo/llm-chess-bot/internal/llm"
)

// Config is the whole bot configuration.
type Config struct {
	Lichess    Lichess                       `json:"lichess"`
	Provider   map[string]llm.ProviderConfig `json:"provider"`
	Model      string                        `json:"model"`
	Challenges Challenges                    `json:"challenges"`
	Bot        Bot                           `json:"bot"`
	// Openings is the default opening plan. When OpeningsFile is set, that
	// file is read again at the start of every game and takes precedence, so
	// the instruction can be changed between games without a restart.
	Openings     Openings `json:"openings"`
	OpeningsFile string   `json:"openings_file"`
	Log          Log      `json:"log"`
}

// Lichess connection settings.
type Lichess struct {
	Token     string `json:"token"`
	BaseURL   string `json:"base_url"`
	UserAgent string `json:"user_agent"`
}

// Challenges decides which incoming challenges are accepted.
type Challenges struct {
	Variants            []string `json:"variants"`              // e.g. ["standard"]
	MinInitialSeconds   int      `json:"min_initial_seconds"`   // reject faster games
	MinIncrementSeconds int      `json:"min_increment_seconds"` // reject smaller increments
	AllowCorrespondence bool     `json:"allow_correspondence"`  // correspondence and unlimited
	AllowRated          bool     `json:"allow_rated"`
	AllowCasual         bool     `json:"allow_casual"`
	AllowBots           bool     `json:"allow_bots"`
}

// Bot settings for the games themselves.
type Bot struct {
	MaxGames        int    `json:"max_games"`         // games played at the same time
	Persona         string `json:"persona"`           // personality given to the LLM
	MaxThinkSeconds int    `json:"max_think_seconds"` // max time per LLM move request
	ChatMemory      int    `json:"chat_memory"`       // transcript entries kept in prompts
	MoveRetries     int    `json:"move_retries"`      // LLM retries after an illegal move
	Greet           bool   `json:"greet"`             // greet and announce the opening
}

// Openings holds one plan per color.
type Openings struct {
	White Opening `json:"white"`
	Black Opening `json:"black"`
}

// Opening is an instruction given to the LLM before a game. With neither
// Name nor Moves, the LLM chooses an opening suited to the opponent.
type Opening struct {
	Name         string `json:"name"`         // e.g. "Italian Game"
	Moves        string `json:"moves"`        // variation in SAN, e.g. "1. e4 e5 2. Nf3 Nc6 3. Bc4"
	Instructions string `json:"instructions"` // free text, e.g. "play aggressively against lower rated players"
}

// Auto reports whether the LLM has to pick the opening itself.
func (o Opening) Auto() bool { return o.Name == "" && o.Moves == "" }

// Log settings.
type Log struct {
	Level  string `json:"level"`  // debug, info, warn, error
	Format string `json:"format"` // json or text
}

// Default returns the configuration used for missing fields.
func Default() Config {
	return Config{
		Lichess: Lichess{BaseURL: "https://lichess.org", UserAgent: "llm-chess-bot/0.1"},
		Challenges: Challenges{
			Variants:          []string{"standard"},
			MinInitialSeconds: 300,
			AllowCasual:       true,
		},
		Bot: Bot{
			MaxGames:        1,
			Persona:         "You are a friendly, talkative chess player who enjoys explaining ideas.",
			MaxThinkSeconds: 60,
			ChatMemory:      40,
			MoveRetries:     2,
			Greet:           true,
		},
		Log: Log{Level: "info", Format: "json"},
	}
}

// Load reads the configuration file (optional when path is empty) on top of
// the defaults, then applies environment variable overrides:
// LICHESS_TOKEN, LICHESS_BASE_URL, LLM_MODEL, LLM_BASE_URL, LLM_API_KEY,
// LOG_LEVEL.
func Load(path string) (Config, error) {
	cfg := Default()
	if path != "" {
		data, err := os.ReadFile(path)
		if err != nil {
			return cfg, err
		}
		if err := json.Unmarshal(expandEnv(data), &cfg); err != nil {
			return cfg, fmt.Errorf("parsing %s: %w", path, err)
		}
	}
	applyEnv(&cfg)
	return cfg, cfg.Validate()
}

// LoadOpenings reads an openings file ({"white": {...}, "black": {...}}).
func LoadOpenings(path string) (Openings, error) {
	var o Openings
	data, err := os.ReadFile(path)
	if err != nil {
		return o, err
	}
	if err := json.Unmarshal(expandEnv(data), &o); err != nil {
		return o, fmt.Errorf("parsing %s: %w", path, err)
	}
	return o, nil
}

var envRef = regexp.MustCompile(`\{env:([A-Za-z_][A-Za-z0-9_]*)\}`)

// expandEnv replaces {env:NAME} with the JSON-escaped value of $NAME.
func expandEnv(data []byte) []byte {
	return envRef.ReplaceAllFunc(data, func(m []byte) []byte {
		name := envRef.FindSubmatch(m)[1]
		quoted, _ := json.Marshal(os.Getenv(string(name)))
		return quoted[1 : len(quoted)-1]
	})
}

func applyEnv(cfg *Config) {
	set := func(dst *string, env string) {
		if v := os.Getenv(env); v != "" {
			*dst = v
		}
	}
	set(&cfg.Lichess.Token, "LICHESS_TOKEN")
	set(&cfg.Lichess.BaseURL, "LICHESS_BASE_URL")
	set(&cfg.Model, "LLM_MODEL")
	set(&cfg.Log.Level, "LOG_LEVEL")

	baseURL, apiKey := os.Getenv("LLM_BASE_URL"), os.Getenv("LLM_API_KEY")
	if baseURL == "" && apiKey == "" {
		return
	}
	if cfg.Model == "" {
		return
	}
	name, _, _ := strings.Cut(cfg.Model, "/")
	if cfg.Provider == nil {
		cfg.Provider = map[string]llm.ProviderConfig{}
	}
	p := cfg.Provider[name]
	if baseURL != "" {
		p.Options.BaseURL = baseURL
	}
	if apiKey != "" {
		p.Options.APIKey = apiKey
	}
	cfg.Provider[name] = p
}

// LLM returns the provider name, its configuration and the model name from
// the "<provider>/<model>" model field.
func (c Config) LLM() (string, llm.ProviderConfig, string, error) {
	name, model, ok := strings.Cut(c.Model, "/")
	if !ok || name == "" || model == "" {
		return "", llm.ProviderConfig{}, "", fmt.Errorf("model %q must be written <provider>/<model>, e.g. litellm/gpt-4o", c.Model)
	}
	p, ok := c.Provider[name]
	if !ok {
		return "", llm.ProviderConfig{}, "", fmt.Errorf("model %q: provider %q is not configured", c.Model, name)
	}
	return name, p, model, nil
}

// Validate checks the configuration.
func (c Config) Validate() error {
	var errs []error
	if c.Lichess.Token == "" {
		errs = append(errs, errors.New("lichess.token (or LICHESS_TOKEN) is required"))
	}
	if _, _, _, err := c.LLM(); err != nil {
		errs = append(errs, err)
	}
	if c.Bot.MaxGames < 1 {
		errs = append(errs, errors.New("bot.max_games must be >= 1"))
	}
	if c.Bot.MaxThinkSeconds < 1 {
		errs = append(errs, errors.New("bot.max_think_seconds must be >= 1"))
	}
	if !c.Challenges.AllowRated && !c.Challenges.AllowCasual {
		errs = append(errs, errors.New("challenges: at least one of allow_rated/allow_casual must be true"))
	}
	return errors.Join(errs...)
}
