package discord

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/bwmarrin/discordgo"
)

type Publisher interface {
	Publish(context.Context, string, string, []*discordgo.MessageEmbed) (string, error)
}

type Client struct {
	Session *discordgo.Session
}

func NewClient(token string) (*Client, error) {
	session, err := discordgo.New("Bot " + token)
	if err != nil {
		return nil, fmt.Errorf("create Discord session: %w", err)
	}
	session.Identify.Intents = discordgo.IntentsNone
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	session.Client = &http.Client{Transport: transport, Timeout: 20 * time.Second}
	dialer := *session.Dialer
	dialer.Proxy = nil
	session.Dialer = &dialer
	session.StateEnabled = false
	return &Client{Session: session}, nil
}

func (c *Client) Publish(ctx context.Context, channelID, nonce string, embeds []*discordgo.MessageEmbed) (string, error) {
	if c == nil || c.Session == nil {
		return "", errors.New("Discord client is not initialized")
	}
	if len(nonce) == 0 || len(nonce) > 25 {
		return "", errors.New("Discord nonce must contain 1 to 25 characters")
	}
	payload := struct {
		Embeds          []*discordgo.MessageEmbed         `json:"embeds"`
		AllowedMentions *discordgo.MessageAllowedMentions `json:"allowed_mentions"`
		Nonce           string                            `json:"nonce"`
		EnforceNonce    bool                              `json:"enforce_nonce"`
	}{
		Embeds:          embeds,
		AllowedMentions: &discordgo.MessageAllowedMentions{Parse: []discordgo.AllowedMentionType{}},
		Nonce:           nonce,
		EnforceNonce:    true,
	}
	endpoint := discordgo.EndpointChannelMessages(channelID)
	body, err := c.Session.RequestWithBucketID(http.MethodPost, endpoint, payload, endpoint, discordgo.WithContext(ctx))
	if err != nil {
		return "", fmt.Errorf("create Discord message: %w", err)
	}
	var message discordgo.Message
	if err := json.Unmarshal(body, &message); err != nil {
		return "", fmt.Errorf("decode Discord message: %w", err)
	}
	if message.ID == "" {
		return "", errors.New("Discord returned an empty message ID")
	}
	return message.ID, nil
}

func Nonce(channelID, localDate, runID string, batchIndex int) string {
	sum := sha256.Sum256([]byte(fmt.Sprintf("%s\x00%s\x00%s\x00%d", channelID, localDate, runID, batchIndex)))
	return hex.EncodeToString(sum[:12])
}

func IsAmbiguous(err error) bool {
	var restErr *discordgo.RESTError
	if errors.As(err, &restErr) && restErr.Response != nil {
		return restErr.Response.StatusCode < 400 || restErr.Response.StatusCode >= 500
	}
	return true
}
