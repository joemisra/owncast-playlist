.PHONY: build run
build:
	go mod tidy
	go build -o playlist-streamer .
run: build
	./playlist-streamer -run
