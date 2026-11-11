package main

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestCommandParsing(t *testing.T) {
	for _, args := range []string{"show", "clear", "link right up", "link right down", "link right delay 250", "link right loss 12.5", "link right corruption 100"} {
		body, method, err := parseCommand(strings.Fields(args))
		if err != nil || method == "" || method == "POST" && !json.Valid(body) {
			t.Fatalf("%s: %s %v", args, body, err)
		}
	}
	for _, args := range []string{"", "show extra", "clear extra", "link", "link right cost 60", "link right delay", "link right loss nope", "link right down 1", "link right loss NaN"} {
		if _, _, err := parseCommand(strings.Fields(args)); err == nil {
			t.Fatalf("accepted %s", args)
		}
	}
}
