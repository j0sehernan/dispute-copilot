// Package psp is the HTTP client for the merchant/PSP systems the agent
// pulls evidence from. In this project they are served by cmd/mockpsp.
package psp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

var ErrNotFound = errors.New("record not found")

type Client struct {
	baseURL string
	http    *http.Client
}

func New(baseURL string, timeout time.Duration) *Client {
	return &Client{
		baseURL: strings.TrimRight(baseURL, "/"),
		http:    &http.Client{Timeout: timeout},
	}
}

func (c *Client) Transaction(ctx context.Context, id string) (json.RawMessage, error) {
	return c.get(ctx, "/v1/transactions/"+url.PathEscape(id))
}

func (c *Client) Order(ctx context.Context, id string) (json.RawMessage, error) {
	return c.get(ctx, "/v1/orders/"+url.PathEscape(id))
}

func (c *Client) Shipment(ctx context.Context, orderID string) (json.RawMessage, error) {
	return c.get(ctx, "/v1/orders/"+url.PathEscape(orderID)+"/shipment")
}

func (c *Client) CustomerHistory(ctx context.Context, customerID string) (json.RawMessage, error) {
	return c.get(ctx, "/v1/customers/"+url.PathEscape(customerID)+"/history")
}

func (c *Client) get(ctx context.Context, path string) (json.RawMessage, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+path, nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("psp: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("psp: read body: %w", err)
	}
	switch {
	case resp.StatusCode == http.StatusNotFound:
		return nil, ErrNotFound
	case resp.StatusCode != http.StatusOK:
		return nil, fmt.Errorf("psp: %s returned %d", path, resp.StatusCode)
	}
	if !json.Valid(body) {
		return nil, fmt.Errorf("psp: %s returned invalid json", path)
	}
	return body, nil
}
