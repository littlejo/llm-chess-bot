package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeFile(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

const opencodeStyle = `{
  "lichess": {"token": "{env:TEST_LICHESS_TOKEN}"},
  "provider": {
    "litellm": {
      "npm": "@ai-sdk/openai-compatible",
      "name": "LiteLLM",
      "options": {"baseURL": "http://localhost:4000/v1", "apiKey": "{env:TEST_LITELLM_KEY}"},
      "models": {"openai/gpt-4o": {"name": "GPT-4o"}}
    }
  },
  "model": "litellm/openai/gpt-4o",
  "openings": {"white": {"name": "Italian Game", "moves": "1. e4 e5 2. Nf3 Nc6 3. Bc4"}},
  "bot": {"max_games": 2}
}`

func TestLoadOpencodeStyle(t *testing.T) {
	t.Setenv("TEST_LICHESS_TOKEN", "lip_abc")
	t.Setenv("TEST_LITELLM_KEY", `sk-"quoted"`)
	cfg, err := Load(writeFile(t, opencodeStyle))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Lichess.Token != "lip_abc" {
		t.Errorf("token = %q", cfg.Lichess.Token)
	}
	name, p, model, err := cfg.LLM()
	if err != nil || name != "litellm" || model != "openai/gpt-4o" || p.Options.APIKey != `sk-"quoted"` {
		t.Errorf("LLM() = %q %+v %q %v", name, p, model, err)
	}
	// Defaults are kept for missing fields; file values override them.
	if cfg.Bot.MaxGames != 2 || cfg.Bot.MaxThinkSeconds != 60 || cfg.Challenges.MinInitialSeconds != 300 {
		t.Errorf("bot = %+v challenges = %+v", cfg.Bot, cfg.Challenges)
	}
	if cfg.Openings.White.Auto() || !cfg.Openings.Black.Auto() {
		t.Errorf("openings = %+v", cfg.Openings)
	}
}

func TestEnvOverrides(t *testing.T) {
	t.Setenv("TEST_LICHESS_TOKEN", "from-file")
	t.Setenv("LICHESS_TOKEN", "from-env")
	t.Setenv("LLM_MODEL", "proxy/claude")
	t.Setenv("LLM_BASE_URL", "http://proxy:4000")
	cfg, err := Load(writeFile(t, opencodeStyle))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Lichess.Token != "from-env" {
		t.Errorf("token = %q", cfg.Lichess.Token)
	}
	name, p, model, err := cfg.LLM()
	if err != nil || name != "proxy" || model != "claude" || p.Options.BaseURL != "http://proxy:4000" {
		t.Errorf("LLM() = %q %+v %q %v", name, p, model, err)
	}
}

func TestEnvOnlyConfig(t *testing.T) {
	t.Setenv("LICHESS_TOKEN", "tok")
	t.Setenv("LLM_MODEL", "litellm/gpt-4o")
	t.Setenv("LLM_BASE_URL", "http://localhost:4000/v1")
	if _, err := Load(""); err != nil {
		t.Fatal(err)
	}
}

func TestValidate(t *testing.T) {
	cfg := Default()
	cfg.Model = "gpt-4o"
	err := cfg.Validate()
	if err == nil || !strings.Contains(err.Error(), "lichess.token") || !strings.Contains(err.Error(), "<provider>/<model>") {
		t.Fatalf("err = %v", err)
	}
	cfg.Lichess.Token = "x"
	cfg.Model = "missing/gpt"
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "not configured") {
		t.Fatalf("err = %v", err)
	}
}

func TestLoadOpenings(t *testing.T) {
	path := writeFile(t, `{"black": {"name": "Sicilian Najdorf", "moves": "1. e4 c5 2. Nf3 d6", "instructions": "be sharp"}}`)
	o, err := LoadOpenings(path)
	if err != nil {
		t.Fatal(err)
	}
	if o.Black.Name != "Sicilian Najdorf" || !o.White.Auto() {
		t.Fatalf("openings = %+v", o)
	}
}
