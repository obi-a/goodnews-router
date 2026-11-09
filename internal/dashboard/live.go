package dashboard

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"time"

	goodnews "github.com/obi-a/goodnews-router"
)

type LiveSnapshot struct {
	Info     goodnews.RunInfo
	Counters goodnews.CounterSnapshot
	Table    goodnews.FIBSnapshot
	Err      error
}

type Client struct {
	client    *http.Client
	transport *http.Transport
}

func NewClient(socket string) *Client {
	dialer := net.Dialer{Timeout: time.Second}
	transport := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return dialer.DialContext(ctx, "unix", socket)
	}}
	return &Client{client: &http.Client{Transport: transport, Timeout: 2 * time.Second}, transport: transport}
}

func (c *Client) Close() { c.transport.CloseIdleConnections() }

func (c *Client) get(ctx context.Context, resource string, value any) error {
	request, err := http.NewRequestWithContext(ctx, "GET", "http://goodnews/v1/"+resource, nil)
	if err != nil {
		return err
	}
	response, err := c.client.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("management %s: %s (restart the rebuilt router, or use --run DIR for saved logs)", resource, response.Status)
	}
	return json.NewDecoder(io.LimitReader(response.Body, 8<<20)).Decode(value)
}

func (c *Client) Status(ctx context.Context) (goodnews.RunInfo, error) {
	var info goodnews.RunInfo
	if err := c.get(ctx, "status", &info); err != nil {
		return info, err
	}
	if info.Version != 1 || info.RunID == "" || info.ArtifactDirectory == "" {
		return info, fmt.Errorf("router has no usable run metadata; restart the rebuilt router")
	}
	return info, nil
}

func (c *Client) Snapshot(ctx context.Context) LiveSnapshot {
	var result LiveSnapshot
	result.Info, result.Err = c.Status(ctx)
	if result.Err != nil {
		return result
	}
	if result.Err = c.get(ctx, "counters", &result.Counters); result.Err != nil {
		return result
	}
	if result.Err = c.get(ctx, "routes", &result.Table); result.Err != nil {
		return result
	}
	// Do not combine data across a restart that occurred between the requests.
	info, err := c.Status(ctx)
	if err != nil {
		result.Err = err
	} else if info.RunID != result.Info.RunID {
		result.Err = fmt.Errorf("router restarted while reading its state; retrying")
	}
	return result
}
