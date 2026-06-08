package main

import (
	"bytes"
	"crypto/rand"
	"crypto/sha1"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"

	"github.com/jackpal/bencode-go"
)

type bencodeInfo struct {
	Pieces       string `bencode:"pieces"`
	PiecesLength int    `bencode:"piece length"`
	Length       int    `bencode:"length"`
	Name         string `bencode:"name"`
}

type bencodeTorrent struct {
	Announce string      `bencode:"announce"`
	Info     bencodeInfo `bencode:"info"`
}

type TorrentFile struct {
	Announce    string
	InfoHash    [20]byte
	PieceHashes [][20]byte
	PieceLength int
	Length      int
	Name        string
}

type trackerResponse struct {
	Interval int    `bencode:"interval"`
	Peers    string `bencode:"peers"`
}

type Peer struct {
	IP   net.IP
	Port uint16
}

func Open(r io.Reader) (*bencodeTorrent, error) {
	bto := bencodeTorrent{}
	err := bencode.Unmarshal(r, &bto)
	if err != nil {
		return nil, err
	}
	return &bto, nil
}

func (t TorrentFile) buildTrackerUrl(peerID [20]byte, port uint16) (string, error) {
	base, err := url.Parse(t.Announce)
	if err != nil {
		return "", err
	}
	params := url.Values{
		"info_hash":  []string{string(t.InfoHash[:])},
		"peer_id":    []string{string(peerID[:])},
		"port":       []string{strconv.Itoa(int(port))},
		"uploaded":   []string{"0"},
		"downloaded": []string{"0"},
		"compact":    []string{"1"},
		"left":       []string{strconv.Itoa(t.Length)},
	}
	base.RawQuery = params.Encode()
	return base.String(), nil
}

func splitPiecesHash(pieces string) ([][20]byte, error) {
	hashLen := len([]byte(pieces))

	if hashLen%20 != 0 {
		return nil, errors.New("hash len of pieces is not a multiple of 20")
	}
	numHashes := hashLen / 20
	hashes := make([][20]byte, numHashes)

	for i := 0; i < numHashes; i++ {
		copy(hashes[i][:], []byte(pieces)[i*20:(i+1)*20])
	}
	return hashes, nil
}

func (bto *bencodeTorrent) toTorrentFile() (*TorrentFile, error) {
	if bto.Info.Pieces == "" {
		return nil, errors.New("pieces field is empty")
	}

	var buf bytes.Buffer
	err := bencode.Marshal(&buf, bto.Info)
	if err != nil {
		return nil, err
	}

	var infoHash [20]byte
	hasher := sha1.New()
	hasher.Write(buf.Bytes())
	copy(infoHash[:], hasher.Sum(nil))

	hashes, err := splitPiecesHash(bto.Info.Pieces)
	if err != nil {
		return nil, err
	}

	return &TorrentFile{
		Announce:    bto.Announce,
		Length:      bto.Info.Length,
		PieceHashes: hashes,
		PieceLength: bto.Info.PiecesLength,
		InfoHash:    infoHash,
		Name:        bto.Info.Name,
	}, nil
}

func parsePeers(peersBin string) ([]Peer, error) {
	peerSize := 6
	if len(peersBin)%peerSize != 0 {
		return nil, errors.New("malformed peers string")
	}

	numPeers := len(peersBin) / peerSize
	peers := make([]Peer, numPeers)

	for i := 0; i < numPeers; i++ {
		offset := i * peerSize
		peers[i].IP = net.IP([]byte(peersBin[offset : offset+4]))
		peers[i].Port = binary.BigEndian.Uint16([]byte(peersBin[offset+4 : offset+6]))
	}
	return peers, nil
}

func (t *TorrentFile) requestPeers(peerID [20]byte, port uint16) ([]Peer, error) {
	urlStr, err := t.buildTrackerUrl(peerID, port)
	if err != nil {
		return nil, err
	}

	resp, err := http.Get(urlStr)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var trackerResp trackerResponse
	err = bencode.Unmarshal(resp.Body, &trackerResp)
	if err != nil {
		return nil, err
	}

	return parsePeers(trackerResp.Peers)
}

func generatePeerID() ([20]byte, error) {
	var peerID [20]byte
	_, err := rand.Read(peerID[:])
	return peerID, err
}

func main() {
	file, err := os.Open("data.torrent")
	if err != nil {
		panic("Error of opening torrent file: " + err.Error())
	}
	defer file.Close()

	bto, err := Open(file)
	if err != nil {
		panic("Error of parsing torrent file: " + err.Error())
	}

	torrentFile, err := bto.toTorrentFile()
	if err != nil {
		panic("Error. Can't convert bencode to torrent: " + err.Error())
	}

	peerID, err := generatePeerID()
	if err != nil {
		panic("Can't generate peer id: " + err.Error())
	}

	peers, err := torrentFile.requestPeers(peerID, 6881)
	if err != nil {
		panic("Can't get peers from tracker: " + err.Error())
	}

	for _, peer := range peers {
		fmt.Printf("%s:%d\n", peer.IP.String(), peer.Port)
	}
}