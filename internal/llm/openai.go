package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// OpenAICompatible calls the /chat/completions endpoint of an OpenAI
// compatible API, such as a LiteLLM proxy.
type OpenAICompatible struct {
	BaseURL string
	APIKey  string
	Model   string
	Headers map[string]string
	HTTP    *http.Client
	// Retries is the number of extra attempts after a 429 or 5xx answer.
	Retries int
}

// NewOpenAICompatible returns a client for baseURL (e.g. http://localhost:4000/v1).
func NewOpenAICompatible(baseURL, apiKey, model string, headers map[string]string) *OpenAICompatible {
	return &OpenAICompatible{
		BaseURL: strings.TrimRight(baseURL, "/"),
		APIKey:  apiKey,
		Model:   model,
		Headers: headers,
		HTTP:    &http.Client{Timeout: 3 * time.Minute},
		Retries: 1,
	}
}

type chatRequest struct {
	Model    string    `json:"model"`
	Messages []Message `json:"messages"`
}

type chatResponse struct {
	Choices []struct {
		Message Message `json:"message"`
	} `json:"choices"`
}

type statusError struct {
	status int
	body   string
}

func (e *statusError) Error() string {
	return fmt.Sprintf("llm: HTTP %d: %s", e.status, e.body)
}

// Complete implements Provider.
func (c *OpenAICompatible) Complete(ctx context.Context, msgs []Message) (string, error) {
	body, err := json.Marshal(chatRequest{Model: c.Model, Messages: msgs})
	if err != nil {
		return "", err
	}
	var lastErr error
	for attempt := 0; attempt <= c.Retries; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return "", ctx.Err()
			case <-time.After(time.Duration(attempt) * time.Second):
			}
		}
		var out string
		out, lastErr = c.once(ctx, body)
		if lastErr == nil {
			return out, nil
		}
		var se *statusError
		if ctx.Err() != nil || (errors.As(lastErr, &se) && se.status != http.StatusTooManyRequests && se.status < 500) {
			break
		}
	}
	return "", lastErr
}

func (c *OpenAICompatible) once(ctx context.Context, body []byte) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	if c.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.APIKey)
	}
	for k, v := range c.Headers {
		req.Header.Set(k, v)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return "", err
	}
	if resp.StatusCode/100 != 2 {
		return "", &statusError{status: resp.StatusCode, body: truncate(string(data), 500)}
	}
	var cr chatResponse
	if err := json.Unmarshal(data, &cr); err != nil {
		return "", fmt.Errorf("llm: decoding response: %w", err)
	}
	if len(cr.Choices) == 0 {
		return "", errors.New("llm: empty response (no choices)")
	}
	return cr.Choices[0].Message.Content, nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
