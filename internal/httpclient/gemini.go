package httpclient

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"mime"
	"net/http"
)

const (
	geminiEndpoint         = "https://generativelanguage.googleapis.com/v1beta/models/gemini-3.5-flash-lite:generateContent"
	geminiMaxRequestBytes  = 64 << 10
	geminiMaxResponseBytes = 64 << 10
)

// GenerateGeminiContent posts once to the fixed Gemini endpoint through the
// shared DNS, transport and rate policy. Redirects are refused because the
// request carries both credentials and article text.
func (c *Client) GenerateGeminiContent(ctx context.Context, apiKey string, payload []byte) ([]byte, error) {
	if c == nil || c.http == nil {
		return nil, errors.New("Gemini HTTP client is not initialized")
	}
	if apiKey == "" {
		return nil, errors.New("Gemini API key is missing")
	}
	if len(payload) > geminiMaxRequestBytes {
		return nil, errors.New("Gemini request exceeds the size limit")
	}
	ctx, cancel := context.WithTimeout(ctx, c.config.RequestTimeout)
	defer cancel()
	u, err := c.ValidateURL(ctx, geminiEndpoint)
	if err != nil {
		return nil, geminiRequestError(ctx, "validate Gemini endpoint", err)
	}
	ctx = context.WithValue(ctx, resourceContextKey{}, resourceGemini)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u.String(), bytes.NewReader(payload))
	if err != nil {
		return nil, errors.New("create Gemini request")
	}
	req.GetBody = nil // Do not replay the POST if a connection fails.
	req.Header.Set("X-Goog-Api-Key", apiKey)
	req.Header.Set("User-Agent", c.config.UserAgent)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, geminiRequestError(ctx, "send Gemini request", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, &StatusError{URL: geminiEndpoint, StatusCode: resp.StatusCode}
	}
	mediaType, _, err := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" {
		return nil, fmt.Errorf("Gemini response: %w", ErrInvalidMIME)
	}
	if resp.ContentLength > geminiMaxResponseBytes {
		return nil, fmt.Errorf("Gemini response: %w", ErrTooLarge)
	}
	body, err := readLimited(resp.Body, geminiMaxResponseBytes)
	if err != nil {
		if errors.Is(err, ErrTooLarge) {
			return nil, fmt.Errorf("Gemini response: %w", ErrTooLarge)
		}
		return nil, geminiRequestError(ctx, "read Gemini response", err)
	}
	return body, nil
}

// The underlying error can include an untrusted response header or redirect
// target. Keep only the context error and the operation name.
func geminiRequestError(ctx context.Context, operation string, err error) error {
	if ctxErr := ctx.Err(); ctxErr != nil {
		return fmt.Errorf("%s: %w", operation, ctxErr)
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return fmt.Errorf("%s: %w", operation, context.DeadlineExceeded)
	}
	return errors.New(operation)
}
