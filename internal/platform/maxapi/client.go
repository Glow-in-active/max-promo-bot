package maxapi

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"promo/internal/app"
	"promo/internal/domain"
)

const defaultBaseURL = "https://platform-api2.max.ru"

type Config struct {
	Token      string
	BaseURL    string
	BannerURL  string
	CAFile     string
	HTTPClient *http.Client
}

type Client struct {
	token     string
	baseURL   string
	bannerURL string
	http      *http.Client
	requests  *pacer
	sends     *recipientPacer
	answers   *recipientPacer
}

func New(config Config) (*Client, error) {
	if strings.TrimSpace(config.Token) == "" {
		return nil, configurationError("MAX token is required")
	}
	if config.BaseURL == "" {
		config.BaseURL = defaultBaseURL
	}
	if err := validateURLs(config); err != nil {
		return nil, err
	}

	httpClient, err := newHTTPClient(config)
	if err != nil {
		return nil, err
	}

	requests := newPacer(time.Second/30 + time.Nanosecond)
	sends := newRecipientPacer(500 * time.Millisecond)
	answers := newRecipientPacer(500 * time.Millisecond)
	sends.parent, answers.parent = requests, requests

	return &Client{
		token:     config.Token,
		baseURL:   strings.TrimRight(config.BaseURL, "/"),
		bannerURL: config.BannerURL,
		http:      httpClient,
		requests:  requests,
		sends:     sends,
		answers:   answers,
	}, nil
}

func (c *Client) Subscription(ctx context.Context, channelID, userID int64) (app.Subscription, error) {
	query := url.Values{"user_ids": {strconv.FormatInt(userID, 10)}}
	endpoint := fmt.Sprintf("/chats/%d/members?%s", channelID, query.Encode())
	var response struct {
		Members *[]struct {
			UserID int64 `json:"user_id"`
		} `json:"members"`
	}
	if err := c.request(ctx, http.MethodGet, endpoint, nil, &response, false, nil); err != nil {
		return app.SubscriptionUnknown, err
	}
	if response.Members == nil {
		return app.SubscriptionUnknown, errors.New("MAX membership response has no members field")
	}
	for _, member := range *response.Members {
		if member.UserID == userID {
			return app.SubscriptionYes, nil
		}
	}
	return app.SubscriptionNo, nil
}

type attachment struct {
	Type    string `json:"type"`
	Payload any    `json:"payload"`
}

type button struct {
	Type    string `json:"type"`
	Text    string `json:"text"`
	Payload string `json:"payload,omitempty"`
	URL     string `json:"url,omitempty"`
}

func (c *Client) Send(ctx context.Context, message domain.Message) error {
	body := struct {
		Text        string       `json:"text"`
		Attachments []attachment `json:"attachments,omitempty"`
	}{Text: message.Text}

	if message.Banner && c.bannerURL != "" {
		body.Attachments = append(body.Attachments, attachment{
			Type:    "image",
			Payload: map[string]string{"url": c.bannerURL},
		})
	}
	if len(message.Buttons) > 0 {
		rows := make([][]button, 0, len(message.Buttons))
		for _, item := range message.Buttons {
			b := button{Type: "callback", Text: item.Text, Payload: item.Payload}
			if item.URL != "" {
				b.Type = "link"
				b.Payload = ""
				b.URL = item.URL
			}
			rows = append(rows, []button{b})
		}
		body.Attachments = append(body.Attachments, attachment{
			Type:    "inline_keyboard",
			Payload: map[string]any{"buttons": rows},
		})
	}

	var response struct {
		Message *struct {
			Body struct {
				MID string `json:"mid"`
			} `json:"body"`
		} `json:"message"`
	}
	if err := c.request(ctx, http.MethodPost,
		"/messages?user_id="+strconv.FormatInt(message.UserID, 10),
		body, &response, true, &recipientRequest{c.sends, message.UserID}); err != nil {
		return err
	}
	if response.Message == nil || strings.TrimSpace(response.Message.Body.MID) == "" {
		return &Error{Status: http.StatusOK, Ambiguous: true}
	}
	return nil
}

func (c *Client) AnswerCallback(ctx context.Context, userID int64, callbackID string) error {
	if strings.TrimSpace(callbackID) == "" {
		return errors.New("MAX callback ID is required")
	}

	query := url.Values{"callback_id": {callbackID}}
	var response struct {
		Success *bool `json:"success"`
	}
	if err := c.request(ctx, http.MethodPost, "/answers?"+query.Encode(),
		struct{}{}, &response, false, &recipientRequest{c.answers, userID}); err != nil {
		return err
	}
	if response.Success == nil || !*response.Success {
		return errors.New("MAX callback acknowledgement was rejected")
	}
	return nil
}
