#!/bin/bash

# Check if config file exists, create it if not
if [ ! -f "./config.json" ]; then
    echo "Config file not found, creating default config.json"
    cat > ./config.json << EOF
{
  "listen_addr": ":8080",
  "database_path": "./data/pfpcache.db",
  "media_cache_path": "./data/media_cache",
  "upstream_relays": [
    "wss://damus.io",
    "wss://primal.net",
    "wss://nos.lol"
  ],
  "max_concurrent": 20,
  "cache_expiration_days": 7
}
EOF
fi

# Create data directories if they don't exist
mkdir -p ./data/media_cache

# Build the pfpcache relay
echo "Building pfpcache relay..."
go build -o pfpcache-relay main.go

# Check if build was successful
if [ $? -ne 0 ]; then
    echo "Build failed. Please check the errors above."
    exit 1
fi

# Run the relay
echo "Starting pfpcache relay on http://localhost:8080"
echo "Press Ctrl+C to stop"
./pfpcache-relay
