package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

const listenAddr = ":9100"
const maxLayers = 8

type Layer struct {
	Type         string             `json:"type"`
	Effect       string             `json:"effect"`
	Name         string             `json:"name"`
	Params       map[string]float64 `json:"params"`
	StringParams map[string]string  `json:"stringParams,omitempty"`
	BlendMode    string             `json:"blendMode"`
	Opacity      float64            `json:"opacity"`
	Visible      bool               `json:"visible"`
}

type State struct {
	Layers  []Layer `json:"layers"`
	Version int     `json:"version"`
}

type Command struct {
	Cmd      string   `json:"cmd"`
	Args     []string `json:"args"`
	MsgID    string   `json:"msgId,omitempty"`
	UserName string   `json:"userName,omitempty"`
	UserID   string   `json:"userId,omitempty"`
}

const allowlistPath = "/opt/owncast/layer-server/allowlist.json"

func loadAllowlist() map[string]bool {
	data, err := os.ReadFile(allowlistPath)
	if err != nil {
		return nil
	}
	var names []string
	if err := json.Unmarshal(data, &names); err != nil {
		return nil
	}
	m := make(map[string]bool, len(names))
	for _, n := range names {
		m[strings.ToLower(n)] = true
	}
	return m
}

func isAllowed(cmd Command) bool {
	allowed := loadAllowlist()
	if allowed == nil {
		return true
	}
	if len(allowed) == 0 {
		return true
	}
	if cmd.UserName == "" && cmd.UserID == "" {
		return true
	}
	if cmd.UserName != "" && allowed[strings.ToLower(cmd.UserName)] {
		return true
	}
	if cmd.UserID != "" && allowed[strings.ToLower(cmd.UserID)] {
		return true
	}
	return false
}

var effectDefaults = map[string]map[string]float64{
	"passthrough": {},
	"crt":         {"scanlineIntensity": 0.5, "curvature": 0.3},
	"chromatic":   {"amount": 2.0, "angle": 0.0},
	"colorgrade":  {"hue": 0, "saturation": 1, "brightness": 0, "contrast": 1},
	"glitch":      {"intensity": 0.5, "blockSize": 16},
	"vhs":         {"tracking": 0.5, "noise": 0.3},
	"pixelate":    {"pixelSize": 4},
	"invert":      {"amount": 1.0},
}

var presets = map[string][]Layer{
	"crt": {
		{Type: "shader", Effect: "crt", Name: "crt",
			Params:    map[string]float64{"scanlineIntensity": 0.5, "curvature": 0.3},
			BlendMode: "normal", Opacity: 1.0, Visible: true},
	},
	"vaporwave": {
		{Type: "shader", Effect: "colorgrade", Name: "colorgrade",
			Params:    map[string]float64{"hue": 30, "saturation": 1.4, "brightness": 0.02, "contrast": 1.15},
			BlendMode: "normal", Opacity: 1.0, Visible: true},
		{Type: "shader", Effect: "chromatic", Name: "chromatic",
			Params:    map[string]float64{"amount": 3.0, "angle": 0.785},
			BlendMode: "normal", Opacity: 0.8, Visible: true},
	},
	"glitchcore": {
		{Type: "shader", Effect: "glitch", Name: "glitch",
			Params:    map[string]float64{"intensity": 0.6, "blockSize": 20},
			BlendMode: "normal", Opacity: 1.0, Visible: true},
		{Type: "shader", Effect: "pixelate", Name: "pixelate",
			Params:    map[string]float64{"pixelSize": 3},
			BlendMode: "normal", Opacity: 0.5, Visible: true},
	},
	"vhs_tape": {
		{Type: "shader", Effect: "vhs", Name: "vhs",
			Params:    map[string]float64{"tracking": 0.7, "noise": 0.4},
			BlendMode: "normal", Opacity: 1.0, Visible: true},
	},
	"nightvision": {
		{Type: "shader", Effect: "colorgrade", Name: "colorgrade",
			Params:    map[string]float64{"hue": 100, "saturation": 0.5, "brightness": 0.1, "contrast": 1.4},
			BlendMode: "normal", Opacity: 1.0, Visible: true},
	},
	"clean": {},
}

type server struct {
	mu      sync.RWMutex
	state   State
	clients map[chan []byte]struct{}
	seen    map[string]time.Time
}

func newServer() *server {
	s := &server{
		state:   State{Layers: []Layer{}, Version: 0},
		clients: make(map[chan []byte]struct{}),
		seen:    make(map[string]time.Time),
	}
	go s.cleanSeen()
	return s
}

func (s *server) cleanSeen() {
	for {
		time.Sleep(60 * time.Second)
		s.mu.Lock()
		cutoff := time.Now().Add(-5 * time.Minute)
		for id, t := range s.seen {
			if t.Before(cutoff) {
				delete(s.seen, id)
			}
		}
		s.mu.Unlock()
	}
}

func (s *server) handleState(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
	w.Header().Set("Access-Control-Allow-Headers", "Content-Type")

	if r.Method == "OPTIONS" {
		return
	}

	if r.Method == "GET" {
		s.mu.RLock()
		data, _ := json.Marshal(s.state)
		s.mu.RUnlock()
		w.Header().Set("Content-Type", "application/json")
		w.Write(data)
		return
	}

	if r.Method == "POST" {
		var cmd Command
		if err := json.NewDecoder(r.Body).Decode(&cmd); err != nil {
			http.Error(w, "bad request", 400)
			return
		}

		if !isAllowed(cmd) {
			log.Printf("Denied %q from user %q (%s)", cmd.Cmd, cmd.UserName, cmd.UserID)
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]string{"error": "not allowed"})
			return
		}

		s.mu.Lock()
		if cmd.MsgID != "" {
			if _, dup := s.seen[cmd.MsgID]; dup {
				data, _ := json.Marshal(s.state)
				s.mu.Unlock()
				w.Header().Set("Content-Type", "application/json")
				w.Write(data)
				return
			}
			s.seen[cmd.MsgID] = time.Now()
		}

		s.applyCommand(cmd.Cmd, cmd.Args)
		s.state.Version++
		data, _ := json.Marshal(s.state)
		s.broadcast(data)
		s.mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		w.Write(data)
		return
	}

	http.Error(w, "method not allowed", 405)
}

func (s *server) handleEvents(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming not supported", 500)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("Access-Control-Allow-Origin", "*")

	ch := make(chan []byte, 8)
	s.mu.Lock()
	s.clients[ch] = struct{}{}
	data, _ := json.Marshal(s.state)
	s.mu.Unlock()

	fmt.Fprintf(w, "data: %s\n\n", data)
	flusher.Flush()

	defer func() {
		s.mu.Lock()
		delete(s.clients, ch)
		s.mu.Unlock()
	}()

	for {
		select {
		case msg := <-ch:
			fmt.Fprintf(w, "data: %s\n\n", msg)
			flusher.Flush()
		case <-r.Context().Done():
			return
		}
	}
}

func (s *server) broadcast(data []byte) {
	for ch := range s.clients {
		select {
		case ch <- data:
		default:
		}
	}
}

func copyParams(src map[string]float64) map[string]float64 {
	dst := make(map[string]float64, len(src))
	for k, v := range src {
		dst[k] = v
	}
	return dst
}

func (s *server) applyCommand(cmd string, args []string) {
	switch cmd {
	case "layer":
		if len(args) < 1 {
			return
		}
		switch args[0] {
		case "add":
			if len(args) >= 3 && args[1] == "shader" {
				effect := args[2]
				defaults, ok := effectDefaults[effect]
				if !ok || len(s.state.Layers) >= maxLayers {
					return
				}
				name := effect
				if len(args) >= 4 {
					name = args[3]
				}
				s.state.Layers = append(s.state.Layers, Layer{
					Type: "shader", Effect: effect, Name: name,
					Params: copyParams(defaults), BlendMode: "normal", Opacity: 1.0, Visible: true,
				})
			}
			if args[1] == "text" && len(args) >= 3 {
				text := args[2]
				name := "text"
				if len(args) >= 4 {
					name = args[3]
				}
				// If more text args exist, join them
				if len(args) >= 5 {
					text = strings.Join(args[2:len(args)-1], " ")
					name = args[len(args)-1]
				}
				s.state.Layers = append(s.state.Layers, Layer{
					Type: "text", Effect: "text", Name: name,
					Params: map[string]float64{
						"x": 16, "y": 16,
						"fontSize": 28,
					},
					StringParams: map[string]string{
						"text":    text,
						"color":   "#ffffff",
						"font":    "monospace",
						"bgColor": "rgba(0,0,0,0.5)",
					},
					BlendMode: "normal", Opacity: 1.0, Visible: true,
				})
			}
		case "remove", "rm":
			if len(args) >= 2 {
				idx, err := strconv.Atoi(args[1])
				if err == nil && idx >= 0 && idx < len(s.state.Layers) {
					s.state.Layers = append(s.state.Layers[:idx], s.state.Layers[idx+1:]...)
				}
			}
		}

	case "fx":
		if len(args) >= 3 {
			val, err := strconv.ParseFloat(args[2], 64)
			if err != nil {
				return
			}
			for i := range s.state.Layers {
				if s.state.Layers[i].Name == args[0] || s.state.Layers[i].Effect == args[0] {
					if s.state.Layers[i].Params == nil {
						s.state.Layers[i].Params = make(map[string]float64)
					}
					s.state.Layers[i].Params[args[1]] = val
					break
				}
			}
		}

	case "blend":
		if len(args) >= 2 {
			idx, err := strconv.Atoi(args[0])
			if err != nil || idx < 0 || idx >= len(s.state.Layers) {
				return
			}
			s.state.Layers[idx].BlendMode = args[1]
		}

	case "opacity":
		if len(args) >= 2 {
			idx, err := strconv.Atoi(args[0])
			if err != nil || idx < 0 || idx >= len(s.state.Layers) {
				return
			}
			val, err := strconv.ParseFloat(args[1], 64)
			if err != nil {
				return
			}
			if val < 0 {
				val = 0
			}
			if val > 1 {
				val = 1
			}
			s.state.Layers[idx].Opacity = val
		}

	case "preset":
		if len(args) >= 1 {
			layers, ok := presets[args[0]]
			if !ok {
				return
			}
			newLayers := make([]Layer, len(layers))
			for i, l := range layers {
				newLayers[i] = Layer{
					Type: l.Type, Effect: l.Effect, Name: l.Name,
					Params: copyParams(l.Params), BlendMode: l.BlendMode,
					Opacity: l.Opacity, Visible: true,
				}
			}
			s.state.Layers = newLayers
		}

	case "layers":
		if len(args) >= 1 {
			vis := args[0] == "on"
			for i := range s.state.Layers {
				s.state.Layers[i].Visible = vis
			}
		}

	case "text":
		if len(args) >= 2 {
			for i := range s.state.Layers {
				if s.state.Layers[i].Name == args[0] || s.state.Layers[i].Effect == args[0] {
					if s.state.Layers[i].StringParams == nil {
						s.state.Layers[i].StringParams = make(map[string]string)
					}
					s.state.Layers[i].StringParams["text"] = strings.Join(args[1:], " ")
					break
				}
			}
		}

	case "clear":
		s.state.Layers = []Layer{}
	}
}

func main() {
	s := newServer()

	mux := http.NewServeMux()
	mux.HandleFunc("/layers-api/state", s.handleState)
	mux.HandleFunc("/layers-api/events", s.handleEvents)

	log.Printf("Layer state server on %s", listenAddr)
	log.Fatal(http.ListenAndServe(listenAddr, mux))
}
