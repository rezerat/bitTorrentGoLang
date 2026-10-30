# PulseTorrent — Go BitTorrent Client & GUI

PulseTorrent is a lightweight, dependency-minimal BitTorrent client written in Go implementing the **BitTorrent Peer Wire Protocol (BEP 0003)**, **Mainline DHT (BEP 0005)**, and **UDP Tracker Protocol (BEP 0015)**, bundled with an embedded real-time graphical interface and an interactive local swarm testbed.

---

## Key Features

- **Full BitTorrent Peer Wire Protocol (BEP 0003)**:
  - 68-byte binary peer handshake (`\x13BitTorrent protocol`, reserved bytes, `info_hash`, `peer_id`).
  - Length-prefixed peer wire framing (`choke`, `unchoke`, `interested`, `not interested`, `have`, `bitfield`, `request`, `piece`, `cancel`, and `keep-alive`).
  - **Request Pipelining**: Maintains a sliding window of up to `5` concurrent `16 KiB` block requests (`MaxBacklog = 5`, `MaxBlockSize = 16384`) per unchoked peer to maximize link utilization.
  - **Cryptographic Piece Verification**: Every downloaded piece is verified against its expected 20-byte SHA-1 digest before being written to disk via `WriteAt` (`int64` offsets) and announced via `HAVE`.
  - **Fault-Tolerant Worker Pool**: Concurrent goroutine workers per peer; if a peer disconnects or fails SHA-1 verification, the piece is automatically returned to the work queue and reassigned.
- **Multi-Tracker, Mainline DHT & Multi-File Support**:
  - Queries **HTTP/HTTPS Trackers**, **UDP Trackers (BEP 0015)**, and **Mainline Kademlia DHT (BEP 0005)** in parallel.
  - Supports both single-file (`length`) and multi-file (`files`) `.torrent` metadata, extracting the exact raw Bencode `4:info` dictionary slice to preserve exact `info_hash` compatibility.
- **Embedded Graphical User Interface (GUI)**:
  - Zero external CGo/WebView dependencies — compiled into a single portable binary via `go:embed`.
  - Live **SHA-1 Piece Verification Map** visualizing piece states (`Pending`, `Pipelining 16 KiB Blocks`, `SHA-1 Verified`).
  - Live transfer telemetry (download speed, progress percentage, connected peers, file list, and peer wire event log).
  - Session lifecycle controls (`Pause`, `Resume`, `Stop`, `Remove`) and custom direct peer injection (`IP:Port`).
- **Built-in Local Swarm Testbed**:
  - One-click **"Run Live Swarm Demo"** spins up an in-process HTTP tracker and 3 concurrent TCP BitTorrent seeders on `127.0.0.1`, generates a valid `data.torrent` file, and performs a full multi-peer download over TCP loopback.

---

## Architecture Overview

| File | Responsibility |
| :--- | :--- |
| `bittorrent.go` | Core BitTorrent engine: Bencode parsing, raw `info_hash` extraction, parallel HTTP & UDP tracker clients, bitfield manipulation, TCP peer wire protocol, pipelined block downloader, SHA-1 piece integrity check, and `DownloadSession` worker pool. |
| `dht.go` | Mainline BitTorrent DHT (BEP 0005) Kademlia `get_peers` client for trackerless peer discovery. |
| `swarm_demo.go` | Self-contained local BitTorrent swarm (`LocalSwarmDemo`) providing a local HTTP tracker and 3 TCP seeders speaking the binary Peer Wire Protocol. |
| `server.go` | CLI entrypoint and HTTP/JSON backend server managing active `DownloadSession` instances and serving the embedded GUI. |
| `ui/index.html` | Single-page graphical dashboard with live piece map, peer table, file inspector, and protocol console. |
| `bittorrent_test.go` | Unit and end-to-end integration tests verifying bitfields, handshake serialization, and multi-seeder swarm downloads. |

---

## Getting Started

### Prerequisites

- **Go 1.22+** (tested with Go 1.25)

### 1. Build the Application

```bash
cd bitTorrentGoLang
go build -o pulsetorrent .
```

### 2. Run in Graphical Mode (Default)

```bash
./pulsetorrent
```

By default, the client starts the GUI server on `http://127.0.0.1:8080` and opens it in your default browser.
- Click **Run Live Swarm Demo** in the top-right corner to immediately watch a live multi-peer BitTorrent transfer and generate a working `data.torrent` file.
- Click **+ Add .torrent File** to upload any `.torrent` file from your computer, customize the destination directory, or specify additional `IP:Port` peers.

### 3. Run in Headless CLI Mode

You can also download any `.torrent` file directly from the terminal without opening the GUI:

```bash
./pulsetorrent -torrent path/to/file.torrent -out ./downloads/output.file
```

#### CLI Flags

| Flag | Default | Description |
| :--- | :--- | :--- |
| `-addr` | `127.0.0.1:8080` | Bind address for the graphical interface server. |
| `-no-browser` | `false` | Do not automatically launch the system browser on startup. |
| `-torrent` | `""` | Run in headless CLI mode and download the specified `.torrent` file. |
| `-out` | `./downloads` | Output file path (in CLI mode) or default downloads directory (in GUI mode). |

---

## REST API Reference

The GUI communicates with the Go backend via a lightweight JSON API:

- `GET /api/torrents` — Returns default output path and real-time state for all active/completed torrent sessions, including piece states, peer lists, and protocol logs.
- `POST /api/torrents/upload` — Multipart form upload accepting `torrent` (`.torrent` file), `outputDir` (string), and `customPeers` (comma-separated `IP:Port` list).
- `POST /api/torrents/demo` — Initializes a local tracker and 3 TCP seeders, writes `data.torrent`, and starts a live download session.
- `POST /api/torrents/action` — Controls an existing session (`{"id": "<infohash>", "action": "pause" | "stop" | "remove"}`).

---

## Running Tests

Run the unit and full end-to-end swarm integration tests with:

```bash
go test -v ./...
```
