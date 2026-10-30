package main

import (
	"bytes"
	"crypto/rand"
	"encoding/binary"
	"net"
	"strconv"
	"sync"
	"time"

	"github.com/jackpal/bencode-go"
)

var dhtBootstrapNodes = []string{
	"router.bittorrent.com:6881",
	"dht.transmissionbt.com:6881",
	"router.utorrent.com:6881",
	"dht.aelitis.com:6881",
}

type dhtMsg struct {
	Y string         `bencode:"y"`
	R dhtRespPayload `bencode:"r"`
}

type dhtRespPayload struct {
	Nodes  string   `bencode:"nodes"`
	Values []string `bencode:"values"`
}

// Queries Mainline BitTorrent DHT (BEP 0005) via iterative Kademlia get_peers RPCs
func discoverDHTPeers(infoHash [20]byte, timeout time.Duration, onPeer func(Peer)) int {
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4zero, Port: 0})
	if err != nil {
		return 0
	}
	defer conn.Close()

	var nodeID [20]byte
	_, _ = rand.Read(nodeID[:])

	var mu sync.Mutex
	visitedNodes := make(map[string]bool)
	discoveredPeers := make(map[string]bool)
	nodeQueue := make([]*net.UDPAddr, 0, 256)

	enqueueNode := func(addr *net.UDPAddr) {
		if addr == nil || addr.Port <= 0 || addr.Port > 65535 || addr.IP.IsLoopback() || addr.IP.IsUnspecified() {
			return
		}
		key := addr.String()
		mu.Lock()
		if !visitedNodes[key] && len(visitedNodes) < 600 {
			visitedNodes[key] = true
			nodeQueue = append(nodeQueue, addr)
		}
		mu.Unlock()
	}

	for _, host := range dhtBootstrapNodes {
		if addr, err := net.ResolveUDPAddr("udp4", host); err == nil {
			enqueueNode(addr)
		}
	}

	packet := buildGetPeersPacket(nodeID, infoHash)
	done := make(chan struct{})
	defer close(done)

	go func() {
		buf := make([]byte, 4096)
		for {
			_ = conn.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
			n, _, err := conn.ReadFromUDP(buf)
			if err != nil {
				select {
				case <-done:
					return
				default:
					continue
				}
			}
			parseKRPCResponse(buf[:n], func(p Peer) {
				key := p.String()
				mu.Lock()
				isNew := !discoveredPeers[key]
				if isNew {
					discoveredPeers[key] = true
				}
				mu.Unlock()
				if isNew && onPeer != nil {
					onPeer(p)
				}
			}, enqueueNode)
		}
	}()

	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		mu.Lock()
		batch := nodeQueue
		nodeQueue = nil
		peerCount := len(discoveredPeers)
		mu.Unlock()

		if peerCount >= 80 {
			break
		}

		for _, addr := range batch {
			_, _ = conn.WriteToUDP(packet, addr)
		}
		time.Sleep(120 * time.Millisecond)
	}

	mu.Lock()
	defer mu.Unlock()
	return len(discoveredPeers)
}

func buildGetPeersPacket(nodeID, infoHash [20]byte) []byte {
	var b bytes.Buffer
	b.WriteString("d1:ad2:id20:")
	b.Write(nodeID[:])
	b.WriteString("9:info_hash20:")
	b.Write(infoHash[:])
	b.WriteString("e1:q9:get_peers1:t2:aa1:y1:qe")
	return b.Bytes()
}

func parseKRPCResponse(raw []byte, onPeer func(Peer), onNode func(*net.UDPAddr)) {
	var msg dhtMsg
	if err := bencode.Unmarshal(bytes.NewReader(raw), &msg); err == nil {
		for _, v := range msg.R.Values {
			if len(v) == 6 {
				ip := net.IP([]byte(v[0:4]))
				port := binary.BigEndian.Uint16([]byte(v[4:6]))
				if port > 0 {
					onPeer(Peer{IP: ip, Port: port})
				}
			}
		}
		parseCompactNodes([]byte(msg.R.Nodes), onNode)
		return
	}

	if idx := bytes.Index(raw, []byte("5:nodes")); idx != -1 {
		pos := idx + 7
		if colon := bytes.IndexByte(raw[pos:], ':'); colon != -1 {
			if length, err := strconv.Atoi(string(raw[pos : pos+colon])); err == nil {
				start := pos + colon + 1
				if start+length <= len(raw) {
					parseCompactNodes(raw[start:start+length], onNode)
				}
			}
		}
	}
	if idx := bytes.Index(raw, []byte("6:valuesl")); idx != -1 {
		pos := idx + 9
		for pos < len(raw) && raw[pos] != 'e' {
			if bytes.HasPrefix(raw[pos:], []byte("6:")) && pos+8 <= len(raw) {
				v := raw[pos+2 : pos+8]
				ip := net.IP([]byte{v[0], v[1], v[2], v[3]})
				port := binary.BigEndian.Uint16(v[4:6])
				if port > 0 {
					onPeer(Peer{IP: ip, Port: port})
				}
				pos += 8
			} else {
				break
			}
		}
	}
}

func parseCompactNodes(nodesBin []byte, onNode func(*net.UDPAddr)) {
	const nodeSize = 26
	count := len(nodesBin) / nodeSize
	for i := 0; i < count; i++ {
		off := i * nodeSize
		ip := net.IPv4(nodesBin[off+20], nodesBin[off+21], nodesBin[off+22], nodesBin[off+23])
		port := int(binary.BigEndian.Uint16(nodesBin[off+24 : off+26]))
		if port > 0 {
			onNode(&net.UDPAddr{IP: ip, Port: port})
		}
	}
}
