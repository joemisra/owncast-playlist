package main

import (
	"context"
	"flag"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"playlist-streamer/config"
	"playlist-streamer/playlist"
	"playlist-streamer/worker"
	"github.com/robfig/cron/v3"
)

func main() {
	configPath := flag.String("config", "config.yaml", "Path to config file")
	playlistName := flag.String("playlist", "", "Playlist name or file (default: first in playlists dir)")
	runNow := flag.Bool("run", false, "Start streaming immediately (no schedule)")
	daemon := flag.String("daemon", "", "Run with cron: 'schedule' uses playlist schedule, 'continuous' runs 24/7")
	flag.Parse()

	cfg, err := config.Load(*configPath)
	if err != nil {
		// Try config.local.yaml if config.yaml doesn't exist
		if os.IsNotExist(err) && *configPath == "config.yaml" {
			cfg, err = config.Load("config.local.yaml")
		}
		if err != nil {
			log.Fatalf("load config: %v", err)
		}
	}

	plPath := cfg.Playlists
	if _, err := os.Stat(plPath); err != nil {
		// Resolve relative to binary
		exe, _ := os.Executable()
		plPath = filepath.Join(filepath.Dir(exe), "playlists")
	}
	pf, err := playlist.LoadPlaylistFromDir(plPath, *playlistName)
	if err != nil {
		log.Fatalf("load playlist: %v", err)
	}
	pl := pf.First()
	if pl == nil {
		log.Fatal("no playlist found")
	}

	w := worker.New(cfg)

	switch {
	case *runNow:
		runStream(w, pl)
	case *daemon == "continuous":
		runStream(w, pl)
	case *daemon == "schedule":
		runScheduled(w, pl)
	default:
		log.Print("Usage: playlist-streamer -run | -daemon=schedule | -daemon=continuous")
		log.Fatal("  -run: stream now | -daemon=schedule: use playlist cron | -daemon=continuous: stream 24/7")
	}
}

func runStream(w *worker.StreamWorker, pl *playlist.Playlist) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sig
		cancel()
	}()
	log.Printf("Starting stream for playlist %q (%d videos)", pl.Name, len(pl.Videos))
	if err := w.StreamPlaylist(ctx, pl); err != nil && err != context.Canceled {
		log.Printf("stream error: %v", err)
	}
	log.Println("Stream stopped")
}

func runScheduled(w *worker.StreamWorker, pl *playlist.Playlist) {
	if pl.Schedule == "" {
		log.Fatal("playlist has no schedule; use -run for immediate stream or set schedule in playlist yaml")
	}
	c := cron.New()
	_, err := c.AddFunc(pl.Schedule, func() {
		go runStream(w, pl)
	})
	if err != nil {
		log.Fatalf("invalid cron schedule %q: %v", pl.Schedule, err)
	}
	c.Start()
	log.Printf("Scheduler running with cron %q (Ctrl+C to stop)", pl.Schedule)
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	<-sig
	c.Stop()
	log.Println("Scheduler stopped")
}
