package maxapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const responseLimit = 1 << 20

type recipientRequest struct {
	limiter *recipientPacer
	userID  int64
}

func (c *Client) request(ctx context.Context, method, endpoint string, body, output any, sending bool, recipient *recipientRequest) error {
	req, err := c.newRequest(ctx, method, endpoint, body)
	if err != nil {
		return err
	}

	if recipient != nil {
		err = recipient.limiter.wait(ctx, recipient.userID)
	} else {
		err = c.requests.wait(ctx)
	}
	if err != nil {
		return err
	}

	response, err := c.http.Do(req)
	if err != nil {
		return requestError(ctx, err, sending)
	}
	defer response.Body.Close()

	return readResponse(response, output, sending)
}

func (c *Client) newRequest(ctx context.Context, method, endpoint string, body any) (*http.Request, error) {
	var input io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("encode MAX request: %w", err)
		}
		input = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+endpoint, input)
	if err != nil {
		return nil, errors.New("construct MAX request")
	}

	req.Header.Set("Authorization", c.token)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return req, nil
}

func readResponse(response *http.Response, output any, sending bool) error {
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4096))
		return &Error{
			Status:     response.StatusCode,
			RetryAfter: retryAfter(response.Header.Get("Retry-After")),
			Ambiguous:  sending && response.StatusCode >= 500,
		}
	}

	data, err := io.ReadAll(io.LimitReader(response.Body, responseLimit+1))
	if err != nil || len(data) > responseLimit {
		return &Error{Status: response.StatusCode, Ambiguous: sending}
	}
	if err := json.Unmarshal(data, output); err != nil {
		return &Error{Status: response.StatusCode, Ambiguous: sending}
	}
	return nil
}

func retryAfter(value string) time.Duration {
	value = strings.TrimSpace(value)
	const maxDuration = time.Duration(1<<63 - 1)
	if value != "" && strings.IndexFunc(value, func(r rune) bool { return r < '0' || r > '9' }) == -1 {
		seconds, err := strconv.ParseUint(value, 10, 64)
		// Saturate only at the duration representation limit to prevent overflow.
		if err != nil || seconds > uint64(maxDuration/time.Second) {
			return maxDuration
		}
		return time.Duration(seconds) * time.Second
	}
	if at, err := http.ParseTime(value); err == nil {
		if wait := time.Until(at); wait > 0 {
			return wait
		}
	}
	return 0
}
