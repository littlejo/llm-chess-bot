// Package llm defines the minimal interface the bot needs from a language
// model, and its implementations.
package llm

import (
	"context"
	"fmt"
)

// Message is one chat message sent to the model.
type Message struct {
	Role    string `json:"role"` // system, user or assistant
	Content string `json:"content"`
}

// Provider returns the model's answer to a conversation.
type Provider interface {
	Complete(ctx context.Context, msgs []Message) (string, error)
}

// ProviderConfig mirrors an entry of the "provider" block of opencode.json:
//
//	"litellm": {
//	  "npm": "@ai-sdk/openai-compatible",
//	  "options": {"baseURL": "http://localhost:4000/v1", "apiKey": "{env:LITELLM_API_KEY}"},
//	  "models": {"gpt-4o": {}}
//	}
type ProviderConfig struct {
	NPM     string          `json:"npm"`
	Name    string          `json:"name"`
	Options ProviderOptions `json:"options"`
	// Models lists the models of the provider; their settings are ignored.
	Models map[string]any `json:"models"`
}

// ProviderOptions are the connection settings of a provider.
type ProviderOptions struct {
	BaseURL string            `json:"baseURL"`
	APIKey  string            `json:"apiKey"`
	Headers map[string]string `json:"headers"`
}

// New builds a provider. Only OpenAI-compatible APIs (LiteLLM, the default of
// opencode for custom providers) are implemented; other kinds can be added
// here without touching the bot.
func New(name string, cfg ProviderConfig, model string) (Provider, error) {
	switch cfg.NPM {
	case "", "@ai-sdk/openai-compatible", "@ai-sdk/openai":
		if cfg.Options.BaseURL == "" {
			return nil, fmt.Errorf("provider %q: options.baseURL is required", name)
		}
		return NewOpenAICompatible(cfg.Options.BaseURL, cfg.Options.APIKey, model, cfg.Options.Headers), nil
	default:
		return nil, fmt.Errorf("provider %q: npm package %q is not supported (use @ai-sdk/openai-compatible)", name, cfg.NPM)
	}
}
