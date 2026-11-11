//go:build linux

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strconv"
	"time"
)

func main() {
	if err := command(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "badnews:", err)
		os.Exit(1)
	}
}

// Flags precede the command, as in badnews --socket PATH link right loss 25.
func command(args []string) error {
	fs := flag.NewFlagSet("badnews", flag.ContinueOnError)
	fs.Usage = func() {
		fmt.Fprintln(fs.Output(), "usage: badnews [--socket PATH] show|clear|link NAME up|down|delay MS|loss PERCENT|corruption PERCENT")
		fs.PrintDefaults()
	}
	socket := fs.String("socket", "/run/goodnews-lab-stage2/r1.sock", "router management Unix socket")
	if err := fs.Parse(args); err != nil {
		if err == flag.ErrHelp {
			return nil
		}
		return err
	}
	body, method, err := parseCommand(fs.Args())
	if err != nil {
		return err
	}
	dialer := net.Dialer{Timeout: 2 * time.Second}
	transport := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return dialer.DialContext(ctx, "unix", *socket)
	}}
	defer transport.CloseIdleConnections()
	client := http.Client{Transport: transport, Timeout: 5 * time.Second}
	request, err := http.NewRequest(method, "http://goodnews/v1/impairments", bytes.NewReader(body))
	if err != nil {
		return err
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := client.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	data, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil {
		return err
	}
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("%s: %s", response.Status, bytes.TrimSpace(data))
	}
	_, err = os.Stdout.Write(data)
	return err
}

func parseCommand(args []string) ([]byte, string, error) {
	usage := fmt.Errorf("usage: badnews [--socket PATH] show|clear|link NAME up|down|delay MS|loss PERCENT|corruption PERCENT (cost arrives in Stage 4)")
	if len(args) == 1 && args[0] == "show" {
		return nil, http.MethodGet, nil
	}
	command := struct {
		Interface string  `json:"interface,omitempty"`
		Setting   string  `json:"setting"`
		Value     float64 `json:"value,omitempty"`
	}{}
	if len(args) == 1 && args[0] == "clear" {
		command.Setting = "clear"
	} else {
		if len(args) < 3 || args[0] != "link" {
			return nil, "", usage
		}
		command.Interface, command.Setting = args[1], args[2]
		switch command.Setting {
		case "up", "down":
			if len(args) != 3 {
				return nil, "", usage
			}
		case "delay", "loss", "corruption":
			if len(args) != 4 {
				return nil, "", usage
			}
			value, err := strconv.ParseFloat(args[3], 64)
			if err != nil {
				return nil, "", err
			}
			command.Value = value
		default:
			return nil, "", usage
		}
	}
	body, err := json.Marshal(command)
	return body, http.MethodPost, err
}
