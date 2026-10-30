package main

import (
	"bytes"
	"crypto/sha1"
	"encoding/binary"
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/jackpal/bencode-go"
)

type LocalSwarmDemo struct {
	mu           sync.Mutex
	running      bool
	trackerLn    net.Listener
	peerLns      []net.Listener
	TorrentBytes []byte
	TorrentFile  *TorrentFile
	Payload      []byte
	TrackerURL   string
	SeederAddrs  []string
}

func NewLocalSwarmDemo() (*LocalSwarmDemo, error) {
	demo := &LocalSwarmDemo{}
	if err := demo.initSwarm(); err != nil {
		return nil, err
	}
	return demo, nil
}

func (d *LocalSwarmDemo) initSwarm() error {
	d.mu.Lock()
	defer d.mu.Unlock()

	payload := buildDemoPayload()
	pieceLength := 32768

	var piecesConcat bytes.Buffer
	for offset := 0; offset < len(payload); offset += pieceLength {
		end := offset + pieceLength
		if end > len(payload) {
			end = len(payload)
		}
		sum := sha1.Sum(payload[offset:end])
		piecesConcat.Write(sum[:])
	}

	numPieces := (len(payload) + pieceLength - 1) / pieceLength

	for i := 0; i < 3; i++ {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			return err
		}
		d.peerLns = append(d.peerLns, ln)
		d.SeederAddrs = append(d.SeederAddrs, ln.Addr().String())
	}

	trackerLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return err
	}
	d.trackerLn = trackerLn
	d.TrackerURL = fmt.Sprintf("http://%s/announce", trackerLn.Addr().String())

	bto := bencodeTorrent{
		Announce: d.TrackerURL,
		Info: bencodeInfo{
			Pieces:      piecesConcat.String(),
			PieceLength: pieceLength,
			Length:      len(payload),
			Name:        "BitTorrent-Protocol-Specification-Demo.txt",
		},
	}

	var torrentBuf bytes.Buffer
	if err := bencode.Marshal(&torrentBuf, bto); err != nil {
		return err
	}

	tf, err := parseTorrentBytes(torrentBuf.Bytes())
	if err != nil {
		return err
	}

	d.Payload = payload
	d.TorrentBytes = torrentBuf.Bytes()
	d.TorrentFile = tf
	d.running = true

	go d.serveTracker()
	for i, ln := range d.peerLns {
		go d.serveSeeder(ln, i, numPieces)
	}

	return nil
}

func (d *LocalSwarmDemo) serveTracker() {
	mux := http.NewServeMux()
	mux.HandleFunc("/announce", func(w http.ResponseWriter, r *http.Request) {
		var compactPeers bytes.Buffer
		for _, ln := range d.peerLns {
			tcpAddr := ln.Addr().(*net.TCPAddr)
			ip4 := tcpAddr.IP.To4()
			if ip4 == nil {
				ip4 = net.IPv4(127, 0, 0, 1).To4()
			}
			compactPeers.Write(ip4)
			var portBytes [2]byte
			binary.BigEndian.PutUint16(portBytes[:], uint16(tcpAddr.Port))
			compactPeers.Write(portBytes[:])
		}

		resp := trackerResponse{
			Interval: 60,
			Peers:    compactPeers.String(),
		}
		w.Header().Set("Content-Type", "text/plain")
		_ = bencode.Marshal(w, resp)
	})

	srv := &http.Server{Handler: mux}
	_ = srv.Serve(d.trackerLn)
}

func (d *LocalSwarmDemo) serveSeeder(ln net.Listener, seederIdx int, numPieces int) {
	for {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		go d.handlePeerConnection(conn, seederIdx, numPieces)
	}
}

func (d *LocalSwarmDemo) handlePeerConnection(conn net.Conn, seederIdx int, numPieces int) {
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(60 * time.Second))

	hs, err := ReadHandshake(conn)
	if err != nil {
		return
	}
	if !bytes.Equal(hs.InfoHash[:], d.TorrentFile.InfoHash[:]) {
		return
	}

	var seederPeerID [20]byte
	copy(seederPeerID[:], fmt.Sprintf("-SD000%d-LOCAL-SEEDER", seederIdx+1))
	replyHs := Handshake{
		Pstr:     "BitTorrent protocol",
		InfoHash: d.TorrentFile.InfoHash,
		PeerID:   seederPeerID,
	}
	if _, err := conn.Write(replyHs.Serialize()); err != nil {
		return
	}

	bf := make(Bitfield, (numPieces+7)/8)
	for i := 0; i < numPieces; i++ {
		bf.SetPiece(i)
	}
	bfMsg := &Message{ID: MsgBitfield, Payload: bf}
	if _, err := conn.Write(bfMsg.Serialize()); err != nil {
		return
	}

	for {
		msg, err := ReadMessage(conn)
		if err != nil {
			return
		}
		if msg == nil {
			continue
		}
		switch msg.ID {
		case MsgInterested:
			unchoke := &Message{ID: MsgUnchoke}
			_, _ = conn.Write(unchoke.Serialize())
		case MsgRequest:
			if len(msg.Payload) != 12 {
				return
			}
			index := int(binary.BigEndian.Uint32(msg.Payload[0:4]))
			begin := int(binary.BigEndian.Uint32(msg.Payload[4:8]))
			length := int(binary.BigEndian.Uint32(msg.Payload[8:12]))

			pieceStart, pieceEnd := d.TorrentFile.pieceBounds(index)
			absStart := int(pieceStart) + begin
			absEnd := absStart + length
			if absStart < 0 || int64(absEnd) > pieceEnd || absEnd > len(d.Payload) {
				return
			}

			time.Sleep(18 * time.Millisecond)

			respPayload := make([]byte, 8+length)
			binary.BigEndian.PutUint32(respPayload[0:4], uint32(index))
			binary.BigEndian.PutUint32(respPayload[4:8], uint32(begin))
			copy(respPayload[8:], d.Payload[absStart:absEnd])

			pieceMsg := &Message{ID: MsgPiece, Payload: respPayload}
			if _, err := conn.Write(pieceMsg.Serialize()); err != nil {
				return
			}
		}
	}
}

func (d *LocalSwarmDemo) Close() {
	d.mu.Lock()
	defer d.mu.Unlock()
	if !d.running {
		return
	}
	d.running = false
	if d.trackerLn != nil {
		_ = d.trackerLn.Close()
	}
	for _, ln := range d.peerLns {
		_ = ln.Close()
	}
}

func buildDemoPayload() []byte {
	var b strings.Builder
	b.WriteString("================================================================================\n")
	b.WriteString("               BITTORRENT PROTOCOL (BEP 0003) VERIFIED PAYLOAD                  \n")
	b.WriteString("================================================================================\n\n")
	b.WriteString("This file was downloaded over the BitTorrent Peer Wire Protocol via TCP/IP\n")
	b.WriteString("from a multi-peer swarm, reassembled from 16 KiB pipelined blocks, and\n")
	b.WriteString("cryptographically verified against SHA-1 piece hashes.\n\n")

	for i := 1; i <= 1600; i++ {
		fmt.Fprintf(&b, "[Block Record #%04d] SHA-1 verified stream segment | BitTorrent Peer Wire Protocol v1.0 | 16 KiB pipelining active\n", i)
	}
	return []byte(b.String())
}
