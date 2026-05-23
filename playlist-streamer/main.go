package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"playlist-streamer/api"
	"playlist-streamer/config"
	"playlist-streamer/playlist"
	"playlist-streamer/tui"
	"playlist-streamer/worker"

	"github.com/robfig/cron/v3"
)

func main() {
	configPath := flag.String("config", "config.yaml", "Path to config file")
	playlistName := flag.String("playlist", "", "Playlist name or file (default: first in playlists dir)")
	runNow := flag.Bool("run", false, "Start streaming immediately (no schedule)")
	daemon := flag.String("daemon", "", "Run with cron: 'schedule' uses playlist schedule, 'continuous' runs 24/7")
	tuiMode := flag.Bool("tui", false, "Full studio: playlist editor + live RTMP stream (same screen)")
	tuiEditOnly := flag.Bool("tui-edit", false, "Playlist editor only (no streaming)")
	apiAddr := flag.String("api", "", "HTTP API listen address, e.g. :9090 (also starts streaming)")
	flag.Parse()

	if *configPath == "config.yaml" {
		if _, err := os.Stat("config.local.yaml"); err == nil {
			*configPath = "config.local.yaml"
		}
	}
	cfg, err := config.Load(*configPath)
	if err != nil {
		log.Fatalf("load config: %v", err)
	}

	plPath := cfg.Playlists
	if _, err := os.Stat(plPath); err != nil {
		exe, _ := os.Executable()
		plPath = filepath.Join(filepath.Dir(exe), "playlists")
	}

	if *tuiEditOnly {
		if err := tui.RunEditor(plPath, *configPath); err != nil {
			log.Fatalf("tui: %v", err)
		}
		return
	}
	if *tuiMode {
		pf, playlistPath, err := playlist.LoadPlaylistFromDirWithPath(plPath, *playlistName)
		if err != nil {
			log.Fatalf("load playlist: %v", err)
		}
		if pf.First() == nil {
			log.Fatal("no playlist in file")
		}
		w := worker.New(cfg)
		if err := tui.RunStudio(cfg, *configPath, plPath, pf, playlistPath, w); err != nil {
			log.Fatalf("studio: %v", err)
		}
		return
	}

	// Load the playlist file we'll use
	pf, err := playlist.LoadPlaylistFromDir(plPath, *playlistName)
	if err != nil {
		log.Fatalf("load playlist: %v", err)
	}
	pl := pf.First()
	if pl == nil {
		log.Fatal("no playlist found")
	}

	w := worker.New(cfg)

	// ── API mode: stream + HTTP API ─────────────────────────────
	if *apiAddr != "" {
		// Use the API address as a flag value (can also be set in config)
		if *apiAddr == "" || *apiAddr == "true" {
			*apiAddr = ":9090"
		}
		runAPIMode(w, pl, cfg, plPath, *apiAddr, *daemon == "continuous")
		return
	}

	switch {
	case *runNow, *daemon == "continuous":
		runInteractive(w, pl, cfg)
	case *daemon == "schedule":
		runScheduled(w, pl)
	default:
		log.Print("Usage: playlist-streamer -run | -daemon=schedule | -daemon=continuous | -tui | -tui-edit | -api=:9090")
		log.Fatal("  -run: stream now | -daemon=schedule: cron | -daemon=continuous: 24/7 | -tui: studio (edit + stream) | -tui-edit: editor only | -api: stream + HTTP API")
	}
}

// runAPIMode starts streaming and the HTTP API server, then waits for SIGINT/SIGTERM.
func runAPIMode(w *worker.StreamWorker, pl *playlist.Playlist, cfg *config.Config, plPath, addr string, continuous bool) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Create API server first so the stream loop can check ShouldContinue()
	apiSrv := api.New(cfg, w, plPath)

	// Start streaming in background
	streamDone := make(chan struct{})
	go func() {
		defer close(streamDone)
		if continuous {
			log.Printf("Starting continuous stream for playlist %q (%d videos)", pl.Name, len(pl.Videos))
		} else {
			log.Printf("Starting stream for playlist %q (%d videos)", pl.Name, len(pl.Videos))
		}

		for {
			if ctx.Err() != nil {
				return
			}
			if err := w.StreamPlaylist(ctx, pl); err != nil && err != context.Canceled {
				log.Printf("stream error: %v", err)
			}
			if ctx.Err() != nil {
				return
			}
			if !continuous || !apiSrv.ShouldContinue() {
				log.Println("Stream stopped — not restarting")
				return
			}
			log.Println("Playlist ended, starting over (continuous mode)...")
		}
	}()
	apiErr := make(chan error, 1)
	go func() {
		apiErr <- apiSrv.Listen(addr)
	}()

	log.Printf("[api] Playlist streamer API running on %s", addr)
	log.Println("Available endpoints:")
	log.Println("  GET  /api/status")
	log.Println("  POST /api/control/play  /api/control/pause  /api/control/skip")
	log.Println("  POST /api/control/subs  { \"enabled\": true }")
	log.Println("  GET  /api/playlist")
	log.Println("  POST /api/playlist     (replace entire playlist)")
	log.Println("  POST /api/playlist/add { \"url\": \"...youtube playlist...\" }")
	log.Println("  POST /api/playlist/video { \"url\": \"...\", \"provider\": \"youtube\" }")
	log.Println("  POST /api/playlist/remove { \"index\": 3 }")
	log.Println("  POST /api/playlist/load { \"file\": \"default.yaml\" }")
	log.Println("  POST /api/playlist/save")
	log.Println("Ctrl+C to stop")

	// Wait for signal
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	select {
	case <-sig:
		fmt.Println("\nShutting down...")
	case err := <-apiErr:
		if err != nil {
			log.Printf("API server error: %v", err)
		}
	}

	// Graceful shutdown
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer shutdownCancel()
	apiSrv.Shutdown(shutdownCtx)
	w.Send(worker.CmdStop)
	cancel()
	select {
	case <-streamDone:
	case <-time.After(5 * time.Second):
		log.Println("Timed out waiting for stream to stop")
	}
}

func runInteractive(w *worker.StreamWorker, pl *playlist.Playlist, cfg *config.Config) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	streamDone := make(chan struct{})

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sig
		fmt.Println("\nShutting down...")
		cancel()
		select {
		case <-streamDone:
		case <-time.After(5 * time.Second):
			log.Println("Timed out waiting for stream cleanup")
		}
		os.Exit(0)
	}()
	go func() {
		defer close(streamDone)
		log.Printf("Starting stream for playlist %q (%d videos)", pl.Name, len(pl.Videos))
		if err := w.StreamPlaylist(ctx, pl); err != nil && err != context.Canceled {
			log.Printf("stream error: %v", err)
		}
		log.Println("Stream finished")
	}()

	printHelp()
	scanner := bufio.NewScanner(os.Stdin)
	fmt.Print("> ")
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		parts := strings.Fields(line)
		if len(parts) == 0 {
			fmt.Print("> ")
			continue
		}
		cmd := strings.ToLower(parts[0])

		switch cmd {
		case "play", "resume":
			w.Send(worker.CmdPlay)
			fmt.Println("Sent play/resume")

		case "pause":
			w.Send(worker.CmdPause)
			fmt.Println("Paused")

		case "stop", "quit", "exit":
			w.Send(worker.CmdStop)
			fmt.Println("Stopping...")
			cancel()
			select {
			case <-streamDone:
			case <-time.After(5 * time.Second):
				log.Println("Timed out waiting for stream to stop, forcing exit")
			}
			return

		case "skip", "next":
			w.Send(worker.CmdSkip)
			fmt.Println("Skipping to next video")

		case "subs", "subtitles", "sub":
			if len(parts) < 2 {
				if w.SubtitlesEnabled() {
					w.Send(worker.CmdSubsOff)
					fmt.Println("Subtitles OFF (video will restart without subs)")
				} else {
					w.Send(worker.CmdSubsOn)
					fmt.Println("Subtitles ON (video will restart with burned-in subs)")
				}
			} else {
				switch strings.ToLower(parts[1]) {
				case "on", "enable", "yes":
					w.Send(worker.CmdSubsOn)
					fmt.Println("Subtitles ON (video will restart with burned-in subs)")
				case "off", "disable", "no":
					w.Send(worker.CmdSubsOff)
					fmt.Println("Subtitles OFF (video will restart without subs)")
				default:
					fmt.Println("Usage: subs [on|off] (no argument toggles)")
				}
			}

		case "status":
			if w.IsPlaying() {
				state := "playing"
				if w.IsPaused() {
					state = "paused"
				}
				subsState := "off"
				if w.SubtitlesEnabled() {
					subsState = "on"
				}
				pl := w.CurrentPlaylist()
				total := 0
				if pl != nil {
					total = len(pl.Videos)
				}
				fmt.Printf("State: %s | Subs: %s | Video %d/%d | URL: %s\n", state, subsState, w.CurrentIndex()+1, total, w.CurrentURL())
			} else {
				fmt.Println("Not currently streaming")
			}

		case "list", "ls":
			currentPl := w.CurrentPlaylist()
			if currentPl == nil {
				fmt.Println("No playlist loaded")
			} else {
				fmt.Printf("Playlist: %s (%d videos)\n", currentPl.Name, len(currentPl.Videos))
				for i, v := range currentPl.Videos {
					marker := "  "
					if i == w.CurrentIndex() && w.IsPlaying() {
						marker = "> "
					}
					fmt.Printf("%s%d. [%s] %s\n", marker, i+1, v.Provider, v.URL)
				}
			}

		case "playlist":
			if len(parts) < 2 {
				fmt.Println("Usage: playlist <filename>")
				break
			}
			newPf, err := playlist.LoadPlaylistFromDir(cfg.Playlists, parts[1])
			if err != nil {
				fmt.Printf("Error loading playlist: %v\n", err)
				break
			}
			newPl := newPf.First()
			if newPl == nil {
				fmt.Println("No playlist found in that file")
				break
			}
			w.SetPlaylist(newPl)
			fmt.Printf("Switched to playlist %q (%d videos) - takes effect after current video\n", newPl.Name, len(newPl.Videos))

		case "help", "?":
			printHelp()

		default:
			fmt.Printf("Unknown command: %s (type 'help' for commands)\n", cmd)
		}
		fmt.Print("> ")
	}
}

func printHelp() {
	fmt.Println(`
Commands:
  play / resume   Resume playback
  pause           Pause after current download/stream finishes
  stop / quit     Stop streaming and exit
  skip / next     Skip to next video
  subs [on|off]   Toggle or set subtitle burn-in (restarts current video)
  status          Show current state
  list / ls       Show playlist with current position
  playlist <file> Switch to a different playlist file
  help / ?        Show this help`)
}

func runScheduled(w *worker.StreamWorker, pl *playlist.Playlist) {
	if pl.Schedule == "" {
		log.Fatal("playlist has no schedule; use -run for immediate stream or set schedule in playlist yaml")
	}
	c := cron.New()
	_, err := c.AddFunc(pl.Schedule, func() {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		log.Printf("Starting scheduled stream for playlist %q", pl.Name)
		if err := w.StreamPlaylist(ctx, pl); err != nil && err != context.Canceled {
			log.Printf("stream error: %v", err)
		}
	})
	if err != nil {
		log.Fatalf("invalid cron schedule %q: %v", pl.Schedule, err)
	}
	c.Start()
	log.Printf("Scheduler running with cron %q (Ctrl+C to stop)", pl.Schedule)
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	<-sigCh
	c.Stop()
	log.Println("Scheduler stopped")
}
