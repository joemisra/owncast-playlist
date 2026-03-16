#!/bin/zsh
echo 'playlists:
  - name: "Watch Later"
    videos:' > watch-later.yaml

yt-dlp --cookies-from-browser chrome --flat-playlist --print "%(url)s" "https://www.youtube.com/playlist?list=WL" | while read url; do
  echo "      - url: \"$url\"
        provider: youtube" >> watch-later.yaml
done
