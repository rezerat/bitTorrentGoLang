package main

import (
	"crypto/sha1"
	"errors"
	"io"
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

func Open(r io.Reader) (*bencodeTorrent, error) {
	bto := bencodeTorrent{}
	err := bencode.Unmarshal(r, &bto)
	if err != nil {
		return nil, err
	}
	return &bto, nil
}

type TorrentFile struct {
    Announce    string
    InfoHash    [20]byte
    PieceHashes [][20]byte
    PieceLength int
    Length      int
    Name        string
}

func (t TorrentFile) buildTrackerUrl(peerID [20]byte, port uint16) (string, error) {
	base, err := url.Parse(t.Announce)
	if err != nil {
		return "", err
	}
	params := url.Values {
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

	if hashLen % 20 != 0 {
		return nil, errors.New("Hash len of pieces is too small")
	}
	numHashes := hashLen / 20
	hashes := make([][20]byte, numHashes)

	for i := 0; i < numHashes; i++ {
		copy(hashes[i][:], []byte(pieces)[i * 20:(i + 1) * 20])
	}
	return hashes, nil
}

func (bto *bencodeTorrent) toTorrentFile() (*TorrentFile, error) {
	var infoHash [20]byte
	if bto.Info.Pieces == "" {
        return nil, errors.New("pieces field is empty")
    }
	hasher := sha1.New()
	hasher.Write([]byte(bto.Info.Name))
	copy(infoHash[:], hasher.Sum(nil))

	hashes, err := splitPiecesHash(bto.Info.Pieces)
	return &TorrentFile{
		Announce: bto.Announce,
		Length: bto.Info.Length,
		PieceHashes: hashes,
		PieceLength: bto.Info.PiecesLength,
		InfoHash: infoHash}, err
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
	torrentFile, err := (bto).toTorrentFile()
	if err != nil {
		panic("Error. Can't convert bencode to torrent! " + err.Error())
	}
	
}
