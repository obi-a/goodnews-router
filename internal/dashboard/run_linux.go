//go:build linux

package dashboard

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"
)

type Options struct {
	Directory     string
	Socket        string
	Input, Output *os.File
}

func Run(ctx context.Context, options Options) (err error) {
	var client *Client
	directory := options.Directory
	if options.Socket != "" {
		client = NewClient(options.Socket)
		defer client.Close()
		info, err := client.Status(ctx)
		if err != nil {
			return fmt.Errorf("attach dashboard: %w", err)
		}
		directory = info.ArtifactDirectory
	}
	store, err := OpenStore(directory)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, store.Close()) }()
	if options.Input == nil {
		options.Input = os.Stdin
	}
	if options.Output == nil {
		options.Output = os.Stdout
	}
	terminal, err := openTerminal(options.Input, options.Output)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, terminal.Close()) }()
	ctx, cancel := context.WithCancel(ctx)
	var workers sync.WaitGroup
	defer func() { cancel(); workers.Wait() }()
	keys := make(chan string, 64)
	workers.Add(1)
	go func() { defer workers.Done(); readKeys(ctx, options.Input, keys) }()
	updates := make(chan LiveSnapshot, 1)
	if client != nil {
		workers.Add(1)
		go func() {
			defer workers.Done()
			ticker := time.NewTicker(time.Second)
			defer ticker.Stop()
			for {
				snapshot := client.Snapshot(ctx)
				select {
				case updates <- snapshot:
				case <-ctx.Done():
					return
				}
				select {
				case <-ticker.C:
				case <-ctx.Done():
					return
				}
			}
		}()
	}
	resize := make(chan os.Signal, 1)
	signal.Notify(resize, syscall.SIGWINCH)
	defer signal.Stop(resize)
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	model := NewModel()
	if client != nil {
		model.Connection = "connecting"
	}
	draw := func() error {
		if err := store.Poll(1024); err != nil {
			return err
		}
		model.Sync(store.Count())
		width, height := terminal.Size()
		return terminal.Draw(model.Lines(store, width, height))
	}
	if err := draw(); err != nil {
		return err
	}
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		case <-resize:
			terminal.lastFrame = ""
		case snapshot := <-updates:
			model.Observe(snapshot, store.Info.RunID)
		case key := <-keys:
			_, height := terminal.Size()
			switch model.Key(key, store.Count(), height) {
			case "quit":
				return nil
			case "reconnect":
				if client == nil {
					model.Message = "Saved-run mode; reopen with --config FILE to attach live."
					break
				}
				if model.Live == nil || model.Live.Err != nil {
					model.Message = "Waiting for the router to become available."
					break
				}
				if model.Live.Info.RunID == store.Info.RunID {
					model.Message = "Already attached to this run."
					break
				}
				next, openErr := OpenStore(model.Live.Info.ArtifactDirectory)
				if openErr != nil {
					model.Message = openErr.Error()
					break
				}
				if closeErr := store.Close(); closeErr != nil {
					_ = next.Close()
					return closeErr
				}
				store = next
				live := model.Live
				model = NewModel()
				model.Connection, model.Live = "connected", live
			}
		}
		if err := draw(); err != nil {
			return err
		}
	}
}
