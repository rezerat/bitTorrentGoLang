package main

import (
	"bytes"
	"path/filepath"
	"testing"
)

func TestBitfield(t *testing.T) {
	bf := make(Bitfield, 2)
	if bf.HasPiece(0) || bf.HasPiece(11) {
		t.Fatalf("expected empty bitfield")
	}
	bf.SetPiece(0)
	bf.SetPiece(11)
	if !bf.HasPiece(0) || !bf.HasPiece(11) {
		t.Fatalf("expected pieces 0 and 11 to be set")
	}
	if bf.Count() != 2 {
		t.Fatalf("expected count 2, got %d", bf.Count())
	}
}

func TestHandshakeSerialization(t *testing.T) {
	var infoHash, peerID [20]byte
	copy(infoHash[:], "12345678901234567890")
	copy(peerID[:], "-GT0001-abcdefghijkl")

	hs := Handshake{
		Pstr:     "BitTorrent protocol",
		InfoHash: infoHash,
		PeerID:   peerID,
	}

	raw := hs.Serialize()
	parsed, err := ReadHandshake(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if parsed.Pstr != hs.Pstr || parsed.InfoHash != hs.InfoHash || parsed.PeerID != hs.PeerID {
		t.Fatalf("handshake mismatch after roundtrip")
	}
}

func TestEndToEndSwarmDownload(t *testing.T) {
	demo, err := NewLocalSwarmDemo()
	if err != nil {
		t.Fatalf("failed to init local swarm demo: %v", err)
	}
	defer demo.Close()

	outFile := filepath.Join(t.TempDir(), demo.TorrentFile.Name)
	if err := demo.TorrentFile.DownloadToFile(outFile); err != nil {
		t.Fatalf("DownloadToFile failed: %v", err)
	}

	sess := NewDownloadSession(demo.TorrentFile, t.TempDir())
	sess.run(nil)
	if sess.Status != "Completed" {
		t.Fatalf("expected session status Completed, got %s (%s)", sess.Status, sess.ErrorMsg)
	}
}
