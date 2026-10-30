package main

import (
	"bytes"
	"crypto/rand"
	"crypto/sha1"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"math/bits"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackpal/bencode-go"
)

type bencodeFile struct {
	Length int      `bencode:"length"`
	Path   []string `bencode:"path"`
}

type bencodeInfo struct {
	Pieces      string        `bencode:"pieces"`
	PieceLength int           `bencode:"piece length"`
	Length      int           `bencode:"length"`
	Name        string        `bencode:"name"`
	Files       []bencodeFile `bencode:"files"`
}

type bencodeTorrent struct {
	Announce     string      `bencode:"announce"`
	AnnounceList [][]string  `bencode:"announce-list"`
	Info         bencodeInfo `bencode:"info"`
}

type FileEntry struct {
	Path   string `json:"path"`
	Length int64  `json:"length"`
	Offset int64  `json:"offset"`
}

type TorrentFile struct {
	Announce    string
	Trackers    []string
	InfoHash    [20]byte
	PieceHashes [][20]byte
	PieceLength int
	Length      int64
	Name        string
	Files       []FileEntry
}

type trackerResponse struct {
	FailureReason string `bencode:"failure reason"`
	Interval      int    `bencode:"interval"`
	Peers         string `bencode:"peers"`
}

type Peer struct {
	IP   net.IP
	Port uint16
}

func (p Peer) String() string {
	return net.JoinHostPort(p.IP.String(), strconv.Itoa(int(p.Port)))
}

func Open(r io.Reader) (*bencodeTorrent, error) {
	bto := bencodeTorrent{}
	if err := bencode.Unmarshal(r, &bto); err != nil {
		return nil, err
	}
	return &bto, nil
}

func parseTorrentBytes(data []byte) (*TorrentFile, error) {
	bto, err := Open(bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	tf, err := bto.toTorrentFile()
	if err != nil {
		return nil, err
	}
	// Hash the exact raw "4:info" bencode slice to preserve custom keys (e.g. private, source, files)
	if rawHash, ok := extractRawInfoHash(data); ok {
		tf.InfoHash = rawHash
	}
	return tf, nil
}

func extractRawInfoHash(data []byte) ([20]byte, bool) {
	var zero [20]byte
	idx := bytes.Index(data, []byte("4:infod"))
	if idx == -1 {
		return zero, false
	}
	start := idx + 6
	end, ok := skipBencodeValue(data, start)
	if !ok || end > len(data) {
		return zero, false
	}
	return sha1.Sum(data[start:end]), true
}

func skipBencodeValue(data []byte, pos int) (int, bool) {
	if pos >= len(data) {
		return 0, false
	}
	switch data[pos] {
	case 'i':
		end := bytes.IndexByte(data[pos:], 'e')
		if end == -1 {
			return 0, false
		}
		return pos + end + 1, true
	case 'l', 'd':
		pos++
		for pos < len(data) && data[pos] != 'e' {
			var ok bool
			pos, ok = skipBencodeValue(data, pos)
			if !ok {
				return 0, false
			}
		}
		if pos >= len(data) {
			return 0, false
		}
		return pos + 1, true
	default:
		if data[pos] < '0' || data[pos] > '9' {
			return 0, false
		}
		colon := bytes.IndexByte(data[pos:], ':')
		if colon == -1 {
			return 0, false
		}
		length, err := strconv.Atoi(string(data[pos : pos+colon]))
		if err != nil || length < 0 {
			return 0, false
		}
		next := pos + colon + 1 + length
		if next > len(data) {
			return 0, false
		}
		return next, true
	}
}

func splitPiecesHash(pieces string) ([][20]byte, error) {
	buf := []byte(pieces)
	if len(buf)%20 != 0 {
		return nil, errors.New("hash len of pieces is not a multiple of 20")
	}
	numHashes := len(buf) / 20
	hashes := make([][20]byte, numHashes)
	for i := 0; i < numHashes; i++ {
		copy(hashes[i][:], buf[i*20:(i+1)*20])
	}
	return hashes, nil
}

func (bto *bencodeTorrent) toTorrentFile() (*TorrentFile, error) {
	if bto.Info.Pieces == "" {
		return nil, errors.New("pieces field is empty")
	}

	type singleFileInfo struct {
		Pieces      string `bencode:"pieces"`
		PieceLength int    `bencode:"piece length"`
		Length      int    `bencode:"length"`
		Name        string `bencode:"name"`
	}

	var buf bytes.Buffer
	err := bencode.Marshal(&buf, singleFileInfo{
		Pieces:      bto.Info.Pieces,
		PieceLength: bto.Info.PieceLength,
		Length:      bto.Info.Length,
		Name:        bto.Info.Name,
	})
	if err != nil {
		return nil, err
	}
	infoHash := sha1.Sum(buf.Bytes())

	hashes, err := splitPiecesHash(bto.Info.Pieces)
	if err != nil {
		return nil, err
	}

	var files []FileEntry
	totalLength := int64(bto.Info.Length)
	if len(bto.Info.Files) > 0 {
		totalLength = 0
		for _, f := range bto.Info.Files {
			cleanParts := make([]string, 0, len(f.Path))
			for _, part := range f.Path {
				if part != "" && part != "." && part != ".." {
					cleanParts = append(cleanParts, part)
				}
			}
			relPath := filepath.Join(cleanParts...)
			fLen := int64(f.Length)
			files = append(files, FileEntry{
				Path:   relPath,
				Length: fLen,
				Offset: totalLength,
			})
			totalLength += fLen
		}
	} else {
		files = []FileEntry{{
			Path:   bto.Info.Name,
			Length: int64(bto.Info.Length),
			Offset: 0,
		}}
	}

	trackers := make([]string, 0)
	seen := make(map[string]bool)
	addTracker := func(u string) {
		u = strings.TrimSpace(u)
		if u != "" && !seen[u] {
			seen[u] = true
			trackers = append(trackers, u)
		}
	}
	addTracker(bto.Announce)
	for _, tier := range bto.AnnounceList {
		for _, u := range tier {
			addTracker(u)
		}
	}

	return &TorrentFile{
		Announce:    bto.Announce,
		Trackers:    trackers,
		Length:      totalLength,
		PieceHashes: hashes,
		PieceLength: bto.Info.PieceLength,
		InfoHash:    infoHash,
		Name:        bto.Info.Name,
		Files:       files,
	}, nil
}

func (t *TorrentFile) buildTrackerUrl(announce string, peerID [20]byte, port uint16) (string, error) {
	base, err := url.Parse(announce)
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
		"left":       []string{strconv.FormatInt(t.Length, 10)},
	}
	base.RawQuery = params.Encode()
	return base.String(), nil
}

func parsePeers(peersBin string) ([]Peer, error) {
	const peerSize = 6
	if len(peersBin)%peerSize != 0 {
		return nil, errors.New("malformed peers binary string")
	}
	numPeers := len(peersBin) / peerSize
	peers := make([]Peer, 0, numPeers)
	for i := 0; i < numPeers; i++ {
		offset := i * peerSize
		ip := net.IP([]byte(peersBin[offset : offset+4]))
		port := binary.BigEndian.Uint16([]byte(peersBin[offset+4 : offset+6]))
		if port == 0 {
			continue
		}
		peers = append(peers, Peer{IP: ip, Port: port})
	}
	return peers, nil
}

// Queries all trackers in parallel so dead or slow trackers do not block peer discovery
func (t *TorrentFile) requestPeers(peerID [20]byte, port uint16, onPeers func(tracker string, peers []Peer, err error)) ([]Peer, error) {
	trackers := t.Trackers
	if len(trackers) == 0 && t.Announce != "" {
		trackers = []string{t.Announce}
	}

	type trResult struct {
		tracker string
		peers   []Peer
		err     error
	}

	resCh := make(chan trResult, len(trackers))
	var wg sync.WaitGroup

	for _, tr := range trackers {
		wg.Add(1)
		go func(announceURL string) {
			defer wg.Done()
			var peers []Peer
			var err error
			switch {
			case strings.HasPrefix(announceURL, "udp://"):
				peers, err = t.requestUDPTracker(announceURL, peerID, port)
			case strings.HasPrefix(announceURL, "http://"), strings.HasPrefix(announceURL, "https://"):
				peers, err = t.requestHTTPTracker(announceURL, peerID, port)
			default:
				return
			}
			resCh <- trResult{tracker: announceURL, peers: peers, err: err}
		}(tr)
	}

	go func() {
		wg.Wait()
		close(resCh)
	}()

	var allPeers []Peer
	seen := make(map[string]bool)
	var lastErr error

	for res := range resCh {
		if onPeers != nil {
			onPeers(res.tracker, res.peers, res.err)
		}
		if res.err != nil {
			lastErr = res.err
			continue
		}
		for _, p := range res.peers {
			key := p.String()
			if !seen[key] {
				seen[key] = true
				allPeers = append(allPeers, p)
			}
		}
	}

	if len(allPeers) == 0 && lastErr != nil {
		return nil, lastErr
	}
	return allPeers, nil
}

func (t *TorrentFile) requestHTTPTracker(announce string, peerID [20]byte, port uint16) ([]Peer, error) {
	urlStr, err := t.buildTrackerUrl(announce, peerID, port)
	if err != nil {
		return nil, err
	}
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Get(urlStr)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}

	var trackerResp trackerResponse
	if err := bencode.Unmarshal(bytes.NewReader(body), &trackerResp); err != nil {
		if peers, ok := extractCompactPeersFromRawBencode(body); ok {
			return peers, nil
		}
		return nil, err
	}
	if trackerResp.FailureReason != "" {
		return nil, errors.New(trackerResp.FailureReason)
	}
	return parsePeers(trackerResp.Peers)
}

func extractCompactPeersFromRawBencode(data []byte) ([]Peer, bool) {
	idx := bytes.Index(data, []byte("5:peers"))
	if idx == -1 {
		return nil, false
	}
	pos := idx + 7
	if pos >= len(data) || data[pos] < '0' || data[pos] > '9' {
		return nil, false
	}
	colon := bytes.IndexByte(data[pos:], ':')
	if colon == -1 {
		return nil, false
	}
	length, err := strconv.Atoi(string(data[pos : pos+colon]))
	if err != nil || length <= 0 {
		return nil, false
	}
	start := pos + colon + 1
	if start+length > len(data) {
		return nil, false
	}
	peers, err := parsePeers(string(data[start : start+length]))
	return peers, err == nil
}

// Implements BEP 15 UDP Tracker Protocol (connect + announce handshake)
func (t *TorrentFile) requestUDPTracker(announce string, peerID [20]byte, port uint16) ([]Peer, error) {
	u, err := url.Parse(announce)
	if err != nil {
		return nil, err
	}
	host := u.Host
	if !strings.Contains(host, ":") {
		host += ":80"
	}
	addr, err := net.ResolveUDPAddr("udp4", host)
	if err != nil {
		return nil, err
	}
	conn, err := net.DialUDP("udp4", nil, addr)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(4 * time.Second))

	var txIDBytes [4]byte
	_, _ = rand.Read(txIDBytes[:])
	txID := binary.BigEndian.Uint32(txIDBytes[:])

	req := make([]byte, 16)
	binary.BigEndian.PutUint64(req[0:8], 0x41727101980) // BEP 15 protocol magic constant
	binary.BigEndian.PutUint32(req[8:12], 0)
	binary.BigEndian.PutUint32(req[12:16], txID)

	if _, err := conn.Write(req); err != nil {
		return nil, err
	}

	resp := make([]byte, 16)
	n, err := conn.Read(resp)
	if err != nil || n < 16 {
		return nil, errors.New("udp tracker connect timeout")
	}
	if binary.BigEndian.Uint32(resp[0:4]) != 0 || binary.BigEndian.Uint32(resp[4:8]) != txID {
		return nil, errors.New("invalid udp tracker connect response")
	}
	connID := binary.BigEndian.Uint64(resp[8:16])

	_, _ = rand.Read(txIDBytes[:])
	txID = binary.BigEndian.Uint32(txIDBytes[:])

	ann := make([]byte, 98)
	binary.BigEndian.PutUint64(ann[0:8], connID)
	binary.BigEndian.PutUint32(ann[8:12], 1)
	binary.BigEndian.PutUint32(ann[12:16], txID)
	copy(ann[16:36], t.InfoHash[:])
	copy(ann[36:56], peerID[:])
	binary.BigEndian.PutUint64(ann[56:64], 0)
	binary.BigEndian.PutUint64(ann[64:72], uint64(t.Length))
	binary.BigEndian.PutUint64(ann[72:80], 0)
	binary.BigEndian.PutUint32(ann[80:84], 0)
	binary.BigEndian.PutUint32(ann[84:88], 0)
	binary.BigEndian.PutUint32(ann[88:92], txID)
	binary.BigEndian.PutUint32(ann[92:96], 200)
	binary.BigEndian.PutUint16(ann[96:98], port)

	if _, err := conn.Write(ann); err != nil {
		return nil, err
	}

	annResp := make([]byte, 4096)
	n, err = conn.Read(annResp)
	if err != nil || n < 20 {
		return nil, errors.New("udp tracker announce timeout")
	}
	if binary.BigEndian.Uint32(annResp[0:4]) != 1 || binary.BigEndian.Uint32(annResp[4:8]) != txID {
		return nil, errors.New("invalid udp tracker announce response")
	}

	return parsePeers(string(annResp[20:n]))
}

func generatePeerID() ([20]byte, error) {
	var peerID [20]byte
	copy(peerID[:8], "-GT0001-")
	_, err := rand.Read(peerID[8:])
	return peerID, err
}

func (t *TorrentFile) pieceBounds(index int) (begin int64, end int64) {
	begin = int64(index) * int64(t.PieceLength)
	end = begin + int64(t.PieceLength)
	if end > t.Length {
		end = t.Length
	}
	return begin, end
}

func (t *TorrentFile) calculatePieceSize(index int) int {
	begin, end := t.pieceBounds(index)
	return int(end - begin)
}

func (t *TorrentFile) InfoHashHex() string {
	return hex.EncodeToString(t.InfoHash[:])
}

type Bitfield []byte

func (bf Bitfield) HasPiece(index int) bool {
	byteIndex := index / 8
	offset := index % 8
	if byteIndex < 0 || byteIndex >= len(bf) {
		return false
	}
	return bf[byteIndex]>>uint(7-offset)&1 != 0
}

func (bf Bitfield) SetPiece(index int) {
	byteIndex := index / 8
	offset := index % 8
	if byteIndex < 0 || byteIndex >= len(bf) {
		return
	}
	bf[byteIndex] |= 1 << uint(7-offset)
}

func (bf Bitfield) Count() int {
	total := 0
	for _, b := range bf {
		total += bits.OnesCount8(b)
	}
	return total
}

type messageID uint8

const (
	MsgChoke         messageID = 0
	MsgUnchoke       messageID = 1
	MsgInterested    messageID = 2
	MsgNotInterested messageID = 3
	MsgHave          messageID = 4
	MsgBitfield      messageID = 5
	MsgRequest       messageID = 6
	MsgPiece         messageID = 7
	MsgCancel        messageID = 8
)

type Message struct {
	ID      messageID
	Payload []byte
}

func (m *Message) Serialize() []byte {
	if m == nil {
		return make([]byte, 4)
	}
	length := uint32(len(m.Payload) + 1)
	buf := make([]byte, 4+length)
	binary.BigEndian.PutUint32(buf[0:4], length)
	buf[4] = byte(m.ID)
	copy(buf[5:], m.Payload)
	return buf
}

func ReadMessage(r io.Reader) (*Message, error) {
	var lengthBuf [4]byte
	if _, err := io.ReadFull(r, lengthBuf[:]); err != nil {
		return nil, err
	}
	length := binary.BigEndian.Uint32(lengthBuf[:])
	if length == 0 {
		return nil, nil // keep-alive
	}
	if length > 2*1024*1024 {
		return nil, errors.New("message frame too large")
	}
	messageBuf := make([]byte, length)
	if _, err := io.ReadFull(r, messageBuf); err != nil {
		return nil, err
	}
	return &Message{
		ID:      messageID(messageBuf[0]),
		Payload: messageBuf[1:],
	}, nil
}

func FormatRequest(index, begin, length int) *Message {
	payload := make([]byte, 12)
	binary.BigEndian.PutUint32(payload[0:4], uint32(index))
	binary.BigEndian.PutUint32(payload[4:8], uint32(begin))
	binary.BigEndian.PutUint32(payload[8:12], uint32(length))
	return &Message{ID: MsgRequest, Payload: payload}
}

func FormatHave(index int) *Message {
	payload := make([]byte, 4)
	binary.BigEndian.PutUint32(payload, uint32(index))
	return &Message{ID: MsgHave, Payload: payload}
}

func ParseHave(msg *Message) (int, error) {
	if msg.ID != MsgHave || len(msg.Payload) != 4 {
		return 0, errors.New("invalid HAVE message")
	}
	return int(binary.BigEndian.Uint32(msg.Payload)), nil
}

func ParsePiece(index int, buf []byte, msg *Message) (int, error) {
	if msg.ID != MsgPiece || len(msg.Payload) < 8 {
		return 0, errors.New("invalid PIECE message")
	}
	parsedIndex := int(binary.BigEndian.Uint32(msg.Payload[0:4]))
	if parsedIndex != index {
		return 0, fmt.Errorf("expected piece %d, got %d", index, parsedIndex)
	}
	begin := int(binary.BigEndian.Uint32(msg.Payload[4:8]))
	if begin < 0 || begin >= len(buf) {
		return 0, errors.New("block offset out of bounds")
	}
	data := msg.Payload[8:]
	if begin+len(data) > len(buf) {
		return 0, errors.New("block data exceeds piece size")
	}
	copy(buf[begin:], data)
	return len(data), nil
}

type Handshake struct {
	Pstr     string
	InfoHash [20]byte
	PeerID   [20]byte
}

func (h *Handshake) Serialize() []byte {
	buf := make([]byte, len(h.Pstr)+49)
	buf[0] = byte(len(h.Pstr))
	curr := 1
	curr += copy(buf[curr:], h.Pstr)
	curr += copy(buf[curr:], make([]byte, 8))
	curr += copy(buf[curr:], h.InfoHash[:])
	curr += copy(buf[curr:], h.PeerID[:])
	return buf
}

func ReadHandshake(r io.Reader) (*Handshake, error) {
	var lengthBuf [1]byte
	if _, err := io.ReadFull(r, lengthBuf[:]); err != nil {
		return nil, err
	}
	pstrlen := int(lengthBuf[0])
	if pstrlen == 0 {
		return nil, errors.New("pstrlen cannot be 0")
	}
	handshakeBuf := make([]byte, 48+pstrlen)
	if _, err := io.ReadFull(r, handshakeBuf); err != nil {
		return nil, err
	}
	var infoHash, peerID [20]byte
	copy(infoHash[:], handshakeBuf[pstrlen+8:pstrlen+28])
	copy(peerID[:], handshakeBuf[pstrlen+28:])
	return &Handshake{
		Pstr:     string(handshakeBuf[0:pstrlen]),
		InfoHash: infoHash,
		PeerID:   peerID,
	}, nil
}

type PeerClient struct {
	Conn     net.Conn
	Choked   bool
	Bitfield Bitfield
	peer     Peer
	infoHash [20]byte
	peerID   [20]byte
}

func NewPeerClient(peer Peer, peerID, infoHash [20]byte, numPieces int) (*PeerClient, error) {
	conn, err := net.DialTimeout("tcp", peer.String(), 5*time.Second)
	if err != nil {
		return nil, err
	}

	if err := completeHandshake(conn, infoHash, peerID); err != nil {
		conn.Close()
		return nil, err
	}

	bf := make(Bitfield, (numPieces+7)/8)
	client := &PeerClient{
		Conn:     conn,
		Choked:   true,
		Bitfield: bf,
		peer:     peer,
		infoHash: infoHash,
		peerID:   peerID,
	}

	_ = conn.SetDeadline(time.Now().Add(4 * time.Second))
	msg, err := ReadMessage(conn)
	if err == nil && msg != nil {
		switch msg.ID {
		case MsgBitfield:
			client.Bitfield = msg.Payload
		case MsgHave:
			if idx, err := ParseHave(msg); err == nil {
				client.Bitfield.SetPiece(idx)
			}
		case MsgUnchoke:
			client.Choked = false
		}
	}
	_ = conn.SetDeadline(time.Time{})

	return client, nil
}

func completeHandshake(conn net.Conn, infoHash, peerID [20]byte) error {
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	defer conn.SetDeadline(time.Time{})

	req := Handshake{
		Pstr:     "BitTorrent protocol",
		InfoHash: infoHash,
		PeerID:   peerID,
	}
	if _, err := conn.Write(req.Serialize()); err != nil {
		return err
	}
	res, err := ReadHandshake(conn)
	if err != nil {
		return err
	}
	if !bytes.Equal(res.InfoHash[:], infoHash[:]) {
		return errors.New("handshake infohash mismatch")
	}
	return nil
}

func (c *PeerClient) SendUnchoke() error {
	msg := Message{ID: MsgUnchoke}
	_, err := c.Conn.Write(msg.Serialize())
	return err
}

func (c *PeerClient) SendInterested() error {
	msg := Message{ID: MsgInterested}
	_, err := c.Conn.Write(msg.Serialize())
	return err
}

func (c *PeerClient) SendHave(index int) error {
	msg := FormatHave(index)
	_, err := c.Conn.Write(msg.Serialize())
	return err
}

func (c *PeerClient) SendRequest(index, begin, length int) error {
	req := FormatRequest(index, begin, length)
	_, err := c.Conn.Write(req.Serialize())
	return err
}

const (
	MaxBlockSize = 16384
	MaxBacklog   = 5
)

type pieceWork struct {
	index  int
	hash   [20]byte
	length int
}

type pieceResult struct {
	index int
	buf   []byte
}

type pieceProgress struct {
	index      int
	client     *PeerClient
	buf        []byte
	downloaded int
	requested  int
	backlog    int
	onBytes    func(int)
}

func (state *pieceProgress) readMessage() error {
	msg, err := ReadMessage(state.client.Conn)
	if err != nil {
		return err
	}
	if msg == nil {
		return nil
	}
	switch msg.ID {
	case MsgUnchoke:
		state.client.Choked = false
	case MsgChoke:
		state.client.Choked = true
	case MsgHave:
		index, err := ParseHave(msg)
		if err != nil {
			return err
		}
		state.client.Bitfield.SetPiece(index)
	case MsgPiece:
		n, err := ParsePiece(state.index, state.buf, msg)
		if err != nil {
			return err
		}
		state.downloaded += n
		state.backlog--
		if state.onBytes != nil {
			state.onBytes(n)
		}
	}
	return nil
}

// Pipelines up to MaxBacklog concurrent 16 KiB block requests per peer to saturate link bandwidth
func attemptDownloadPiece(c *PeerClient, pw *pieceWork, onBytes func(int)) ([]byte, error) {
	state := pieceProgress{
		index:   pw.index,
		client:  c,
		buf:     make([]byte, pw.length),
		onBytes: onBytes,
	}

	_ = c.Conn.SetDeadline(time.Now().Add(30 * time.Second))
	defer c.Conn.SetDeadline(time.Time{})

	for state.downloaded < pw.length {
		if !state.client.Choked {
			for state.backlog < MaxBacklog && state.requested < pw.length {
				blockSize := MaxBlockSize
				if pw.length-state.requested < blockSize {
					blockSize = pw.length - state.requested
				}
				if err := c.SendRequest(pw.index, state.requested, blockSize); err != nil {
					return nil, err
				}
				state.backlog++
				state.requested += blockSize
			}
		}
		if err := state.readMessage(); err != nil {
			return nil, err
		}
	}
	return state.buf, nil
}

func checkIntegrity(pw *pieceWork, buf []byte) error {
	hash := sha1.Sum(buf)
	if !bytes.Equal(hash[:], pw.hash[:]) {
		return fmt.Errorf("piece %d failed integrity check", pw.index)
	}
	return nil
}

type DownloadSession struct {
	ID            string
	Torrent       *TorrentFile
	OutputDir     string
	Status        string
	ErrorMsg      string
	Peers         []Peer
	ActivePeers   int32
	Downloaded    int64
	SpeedBps      int64
	CompletedPcs  int32
	PieceStatuses []uint8 // 0: pending, 1: downloading, 2: verified
	Logs          []string
	StartedAt     time.Time

	mu     sync.RWMutex
	stopCh chan struct{}
	paused atomic.Bool
}

func NewDownloadSession(tf *TorrentFile, outputDir string) *DownloadSession {
	return &DownloadSession{
		ID:            tf.InfoHashHex(),
		Torrent:       tf,
		OutputDir:     outputDir,
		Status:        "Initializing",
		PieceStatuses: make([]uint8, len(tf.PieceHashes)),
		StartedAt:     time.Now(),
		stopCh:        make(chan struct{}),
	}
}

func (s *DownloadSession) AddLog(format string, args ...any) {
	msg := fmt.Sprintf("[%s] %s", time.Now().Format("15:04:05"), fmt.Sprintf(format, args...))
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Logs = append(s.Logs, msg)
	if len(s.Logs) > 200 {
		s.Logs = s.Logs[len(s.Logs)-200:]
	}
}

func (s *DownloadSession) SetPieceStatus(index int, status uint8) {
	s.mu.Lock()
	if index >= 0 && index < len(s.PieceStatuses) {
		s.PieceStatuses[index] = status
	}
	s.mu.Unlock()
}

func (s *DownloadSession) Stop() {
	s.mu.Lock()
	defer s.mu.Unlock()
	select {
	case <-s.stopCh:
	default:
		close(s.stopCh)
	}
	if s.Status != "Completed" {
		s.Status = "Stopped"
	}
	atomic.StoreInt64(&s.SpeedBps, 0)
}

func (s *DownloadSession) IsStopped() bool {
	select {
	case <-s.stopCh:
		return true
	default:
		return false
	}
}

func (s *DownloadSession) TogglePause() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.Status == "Completed" || s.Status == "Stopped" || s.Status == "Error" {
		return s.Status
	}
	if s.paused.Load() {
		s.paused.Store(false)
		if atomic.LoadInt32(&s.ActivePeers) > 0 {
			s.Status = "Downloading"
		} else {
			s.Status = "Connecting to Peers"
		}
	} else {
		s.paused.Store(true)
		s.Status = "Paused"
		atomic.StoreInt64(&s.SpeedBps, 0)
	}
	return s.Status
}

func (s *DownloadSession) Start(customPeers []Peer) {
	go s.run(customPeers)
}

func (s *DownloadSession) run(customPeers []Peer) {
	peerID, err := generatePeerID()
	if err != nil {
		s.fail("peer ID generation failed: %v", err)
		return
	}

	var recentBytes atomic.Int64
	speedTicker := time.NewTicker(1 * time.Second)
	defer speedTicker.Stop()

	go func() {
		for {
			select {
			case <-s.stopCh:
				return
			case <-speedTicker.C:
				b := recentBytes.Swap(0)
				atomic.StoreInt64(&s.SpeedBps, b)
			}
		}
	}()

	workQueue := make(chan *pieceWork, len(s.Torrent.PieceHashes))
	results := make(chan *pieceResult, 64)

	for index, hash := range s.Torrent.PieceHashes {
		length := s.Torrent.calculatePieceSize(index)
		workQueue <- &pieceWork{index, hash, length}
	}

	var startedPeers sync.Map
	spawnPeer := func(p Peer) {
		if s.IsStopped() {
			return
		}
		key := p.String()
		if _, loaded := startedPeers.LoadOrStore(key, true); loaded {
			return
		}
		s.mu.Lock()
		s.Peers = append(s.Peers, p)
		if !s.paused.Load() && (s.Status == "Contacting Trackers" || s.Status == "Initializing") {
			s.Status = "Connecting to Peers"
		}
		s.mu.Unlock()

		go s.startDownloadWorker(p, peerID, workQueue, results, func(n int) {
			recentBytes.Add(int64(n))
		})
	}

	s.mu.Lock()
	s.Status = "Contacting Trackers"
	s.mu.Unlock()

	numTrackers := len(s.Torrent.Trackers)
	if numTrackers == 0 && s.Torrent.Announce != "" {
		numTrackers = 1
	}
	s.AddLog("Querying %d tracker(s) in parallel for %s (%d pieces, %d bytes)", numTrackers, s.Torrent.Name, len(s.Torrent.PieceHashes), s.Torrent.Length)

	for _, cp := range customPeers {
		spawnPeer(cp)
	}

	// Stream peers from both Trackers (HTTP/UDP) and Mainline Kademlia DHT (BEP 0005) concurrently
	go func() {
		var wg sync.WaitGroup

		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = s.Torrent.requestPeers(peerID, 6881, func(tracker string, trPeers []Peer, trErr error) {
				if trErr != nil {
					return
				}
				if len(trPeers) > 0 {
					s.AddLog("Tracker %s returned %d peers", tracker, len(trPeers))
					for _, p := range trPeers {
						spawnPeer(p)
					}
				}
			})
		}()

		wg.Add(1)
		go func() {
			defer wg.Done()
			s.AddLog("Searching Mainline DHT (BEP 0005) for active swarm peers...")
			found := discoverDHTPeers(s.Torrent.InfoHash, 10*time.Second, func(p Peer) {
				spawnPeer(p)
			})
			if found > 0 {
				s.AddLog("DHT search discovered %d peers", found)
			} else {
				s.AddLog("DHT search finished (0 peers announced for this info_hash)")
			}
		}()

		wg.Wait()

		// Allow ongoing 5s TCP dial attempts to complete before evaluating if any peer succeeded
		time.Sleep(5500 * time.Millisecond)

		s.mu.Lock()
		defer s.mu.Unlock()
		if s.IsStopped() || s.paused.Load() || s.Status == "Completed" || s.Status == "Stopped" || s.Status == "Paused" {
			return
		}
		if atomic.LoadInt32(&s.ActivePeers) == 0 && atomic.LoadInt32(&s.CompletedPcs) == 0 {
			totalDiscovered := len(s.Peers)
			if totalDiscovered == 0 {
				s.Status = "Stalled (0 Seeds)"
				s.AddLog("No active seeders found on trackers or DHT for this torrent.")
			} else {
				s.Status = "Stalled (Peers Unreachable)"
				s.AddLog("Discovered %d peer(s), but none accepted incoming TCP BitTorrent handshakes (dead swarm or behind NAT).", totalDiscovered)
			}
		}
	}()

	donePieces := 0
	totalPieces := len(s.Torrent.PieceHashes)

	for donePieces < totalPieces {
		select {
		case <-s.stopCh:
			s.AddLog("Download stopped by user.")
			return
		case res := <-results:
			if err := s.writePieceToDisk(res.index, res.buf); err != nil {
				s.fail("disk write error on piece #%d: %v", res.index, err)
				return
			}
			donePieces++
			atomic.StoreInt32(&s.CompletedPcs, int32(donePieces))
			atomic.AddInt64(&s.Downloaded, int64(len(res.buf)))
			s.SetPieceStatus(res.index, 2)
			percent := float64(donePieces) / float64(totalPieces) * 100
			s.AddLog("Verified & wrote piece #%d (%d/%d, %.1f%%)", res.index, donePieces, totalPieces, percent)
		}
	}

	close(workQueue)

	atomic.StoreInt64(&s.SpeedBps, 0)
	s.mu.Lock()
	s.Status = "Completed"
	s.mu.Unlock()
	s.AddLog("Download completed! Saved to %s", s.OutputDir)
}

// Writes each verified piece directly to its target file offsets on disk using pwrite (WriteAt)
// so multi-gigabyte torrents never allocate the entire file in RAM.
func (s *DownloadSession) writePieceToDisk(pieceIndex int, pieceBuf []byte) error {
	if err := os.MkdirAll(s.OutputDir, 0755); err != nil {
		return err
	}
	pieceBegin, pieceEnd := s.Torrent.pieceBounds(pieceIndex)

	if len(s.Torrent.Files) <= 1 {
		outPath := filepath.Join(s.OutputDir, filepath.Base(s.Torrent.Name))
		f, err := os.OpenFile(outPath, os.O_CREATE|os.O_WRONLY, 0644)
		if err != nil {
			return err
		}
		defer f.Close()
		_, err = f.WriteAt(pieceBuf, pieceBegin)
		return err
	}

	baseDir := filepath.Join(s.OutputDir, filepath.Base(s.Torrent.Name))
	for _, fe := range s.Torrent.Files {
		fileStart := fe.Offset
		fileEnd := fe.Offset + fe.Length
		if pieceEnd <= fileStart || pieceBegin >= fileEnd {
			continue
		}
		overlapStart := maxInt64(pieceBegin, fileStart)
		overlapEnd := minInt64(pieceEnd, fileEnd)

		bufOffset := overlapStart - pieceBegin
		bufLen := overlapEnd - overlapStart
		fileOffset := overlapStart - fileStart

		fullPath := filepath.Join(baseDir, fe.Path)
		if err := os.MkdirAll(filepath.Dir(fullPath), 0755); err != nil {
			return err
		}
		f, err := os.OpenFile(fullPath, os.O_CREATE|os.O_WRONLY, 0644)
		if err != nil {
			return err
		}
		_, writeErr := f.WriteAt(pieceBuf[bufOffset:bufOffset+bufLen], fileOffset)
		_ = f.Close()
		if writeErr != nil {
			return writeErr
		}
	}
	return nil
}

func maxInt64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}

func minInt64(a, b int64) int64 {
	if a < b {
		return a
	}
	return b
}

func (s *DownloadSession) fail(format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	s.mu.Lock()
	s.Status = "Error"
	s.ErrorMsg = msg
	s.mu.Unlock()
	s.AddLog("ERROR: %s", msg)
}

func (s *DownloadSession) startDownloadWorker(peer Peer, peerID [20]byte, workQueue chan *pieceWork, results chan *pieceResult, onBytes func(int)) {
	numPieces := len(s.Torrent.PieceHashes)
	c, err := NewPeerClient(peer, peerID, s.Torrent.InfoHash, numPieces)
	if err != nil {
		s.AddLog("Peer %s unreachable: %v", peer.String(), err)
		return
	}
	defer c.Conn.Close()

	atomic.AddInt32(&s.ActivePeers, 1)
	defer atomic.AddInt32(&s.ActivePeers, -1)

	// Close peer socket immediately if the session is stopped while blocked on network I/O
	go func() {
		<-s.stopCh
		_ = c.Conn.Close()
	}()

	s.mu.Lock()
	if !s.paused.Load() && !s.IsStopped() && (s.Status == "Connecting to Peers" || s.Status == "Contacting Trackers") {
		s.Status = "Downloading"
	}
	s.mu.Unlock()

	s.AddLog("Handshake completed with peer %s", peer.String())
	_ = c.SendUnchoke()
	_ = c.SendInterested()

	for {
		select {
		case <-s.stopCh:
			return
		case pw, ok := <-workQueue:
			if !ok {
				return
			}
			for s.paused.Load() {
				select {
				case <-s.stopCh:
					return
				case <-time.After(300 * time.Millisecond):
				}
			}
			if !c.Bitfield.HasPiece(pw.index) {
				workQueue <- pw
				time.Sleep(150 * time.Millisecond)
				continue
			}

			s.SetPieceStatus(pw.index, 1)
			buf, err := attemptDownloadPiece(c, pw, onBytes)
			if err != nil {
				s.SetPieceStatus(pw.index, 0)
				workQueue <- pw
				return
			}

			if err := checkIntegrity(pw, buf); err != nil {
				s.SetPieceStatus(pw.index, 0)
				s.AddLog("Hash mismatch on piece #%d from %s", pw.index, peer.String())
				workQueue <- pw
				continue
			}

			_ = c.SendHave(pw.index)
			results <- &pieceResult{pw.index, buf}
		}
	}
}

func (t *TorrentFile) DownloadToFile(outPath string) error {
	peerID, err := generatePeerID()
	if err != nil {
		return err
	}
	peers, err := t.requestPeers(peerID, 6881, nil)
	if err != nil {
		return err
	}
	if len(peers) == 0 {
		return errors.New("no peers available from tracker")
	}

	outFile, err := os.OpenFile(outPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0644)
	if err != nil {
		return err
	}
	defer outFile.Close()

	workQueue := make(chan *pieceWork, len(t.PieceHashes))
	results := make(chan *pieceResult, 64)
	for index, hash := range t.PieceHashes {
		workQueue <- &pieceWork{index, hash, t.calculatePieceSize(index)}
	}

	for _, peer := range peers {
		go func(p Peer) {
			c, err := NewPeerClient(p, peerID, t.InfoHash, len(t.PieceHashes))
			if err != nil {
				return
			}
			defer c.Conn.Close()
			_ = c.SendUnchoke()
			_ = c.SendInterested()
			for pw := range workQueue {
				if !c.Bitfield.HasPiece(pw.index) {
					workQueue <- pw
					continue
				}
				buf, err := attemptDownloadPiece(c, pw, nil)
				if err != nil {
					workQueue <- pw
					return
				}
				if err := checkIntegrity(pw, buf); err != nil {
					workQueue <- pw
					continue
				}
				_ = c.SendHave(pw.index)
				results <- &pieceResult{pw.index, buf}
			}
		}(peer)
	}

	for done := 0; done < len(t.PieceHashes); done++ {
		res := <-results
		begin, _ := t.pieceBounds(res.index)
		if _, err := outFile.WriteAt(res.buf, begin); err != nil {
			return err
		}
	}
	close(workQueue)
	return nil
}
