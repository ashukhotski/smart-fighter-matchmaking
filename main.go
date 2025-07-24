package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net"
	"strings"
	"sync"
	"time"
)

const (
	tcpPort              = ":9999"
	udpPort              = ":9998"
	maxReconnectsPerIP   = 5
	reconnectWindow      = 5 * time.Second
	maxConnectionsPerIP  = 3
	totalConnLimit       = 500
)

type Player struct {
	tcpConn  net.Conn
	ip       string
	udpPort  int
	addr     *net.TCPAddr
}

var (
	waitingQueue          []*Player
	udpPorts              = make(map[string]int)       // IP → last known UDP port
	reconnectTimestamps   = make(map[string][]time.Time)
	connCounts            = make(map[string]int)
	activeConnections     = 0

	queueLock      sync.Mutex
	connCountsLock sync.Mutex
	udpLock        sync.Mutex
)

func main() {
	go udpListen() // Start UDP listener
	tcpListen()    // Run TCP matchmaking
}

func udpListen() {
	pc, err := net.ListenPacket("udp", udpPort)
	if err != nil {
		panic(err)
	}
	defer pc.Close()
	fmt.Println("📡 UDP listener on", udpPort)

	buf := make([]byte, 1024)
	for {
		n, addr, err := pc.ReadFrom(buf)
		if err != nil {
			fmt.Println("UDP read error:", err)
			continue
		}
		text := strings.TrimSpace(string(buf[:n]))
		if text == "hello" {
			ip := getIP(addr)
			port := getUDPPort(addr)

			udpLock.Lock()
			udpPorts[ip+fmt.Sprint(port)] = port
			udpLock.Unlock()

			fmt.Printf("👋 UDP hello from %s:%d\n", ip, port)
		}
	}
}

func tcpListen() {
	ln, err := net.Listen("tcp", tcpPort)
	if err != nil {
		panic(err)
	}
	fmt.Println("🧠 TCP matchmaking listening on", tcpPort)

	for {
		conn, err := ln.Accept()
		if err != nil {
			fmt.Println("TCP accept error:", err)
			continue
		}

		ip := getIP(conn.RemoteAddr())

		if !allowConnection(ip) {
			fmt.Println("🚫 Connection blocked:", ip)
			conn.Close()
			continue
		}

		incrementConnection(ip)
		go func() {
			handlePlayer(conn)
			decrementConnection(ip)
		}()
	}
}

func handlePlayer(conn net.Conn) {
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(60 * time.Second))

	addr, ok := conn.RemoteAddr().(*net.TCPAddr)
	if !ok {
		fmt.Println("⚠️ Cannot parse address")
		return
	}
	ip := addr.IP.String()

	reader := bufio.NewReader(conn)
	line, err := reader.ReadString('\n')
	if err != nil {
		fmt.Println("❌ Failed to read from client:", err)
		return
	}
	line = strings.TrimSpace(line)
	fmt.Println("📩 Received from client:", line)


	var msg map[string]interface{}
	if err := json.Unmarshal([]byte(line), &msg); err != nil {
		fmt.Println("❌ Invalid JSON from client:", line, err)
		return
	}
	udpPortFloat, ok := msg["udp_port"].(float64)
	if !ok {
		fmt.Println("❌ Missing or invalid UDP port in JSON")
		return
	}
	udpPort := int(udpPortFloat)

	udpLock.Lock()
	port, ok := udpPorts[ip+fmt.Sprint(udpPort)]
	udpLock.Unlock()

	if !ok {
		fmt.Println("⏳ Waiting for UDP hello from", ip)
		time.Sleep(1 * time.Second)
		return
	}

	player := &Player{
		tcpConn: conn,
		ip:      ip,
		udpPort: port,
		addr:    addr,
	}

	queueLock.Lock()
	if len(waitingQueue) == 0 {
		waitingQueue = append(waitingQueue, player)
		queueLock.Unlock()
		sendJSON(conn, map[string]any{"role": "host"})
		fmt.Printf("[HOST] %s waiting...\n", ip)
		waitForPeer(conn)
	} else {
		peer := waitingQueue[0]
		waitingQueue = waitingQueue[1:]
		queueLock.Unlock()

		sendJSON(player.tcpConn, map[string]any{
			"role":      "client",
			"host_ip":   peer.ip,
			"host_port": peer.udpPort,
		})
		sendJSON(peer.tcpConn, map[string]any{
			"peer_joined": true,
			"client_ip":   player.ip,
			"client_port": player.udpPort,
		})
		fmt.Printf("[PAIR] %s ⇄ %s\n", player.ip, peer.ip)
	}
}

func waitForPeer(conn net.Conn) {
	buf := make([]byte, 1)
	_, _ = conn.Read(buf)
}

func sendJSON(conn net.Conn, msg map[string]any) {
	data, err := json.Marshal(msg)
	if err != nil {
		fmt.Println("❌ JSON encode error:", err)
		return
	}
	writer := bufio.NewWriter(conn)
	writer.Write(data)
	writer.WriteByte('\n')
	writer.Flush()
	fmt.Println("📤 Sent:", string(data))
}

func getIP(addr net.Addr) string {
	switch v := addr.(type) {
	case *net.TCPAddr:
		return v.IP.String()
	case *net.UDPAddr:
		return v.IP.String()
	default:
		return ""
	}
}

func getUDPPort(addr net.Addr) int {
	switch v := addr.(type) {
	case *net.UDPAddr:
		return v.Port
	default:
		return 0
	}
}

// --- Security checks

func allowConnection(ip string) bool {
	now := time.Now()

	connCountsLock.Lock()
	defer connCountsLock.Unlock()

	var recent []time.Time
	for _, t := range reconnectTimestamps[ip] {
		if now.Sub(t) <= reconnectWindow {
			recent = append(recent, t)
		}
	}
	reconnectTimestamps[ip] = append(recent, now)

	if len(recent) >= maxReconnectsPerIP {
		fmt.Printf("⚠ Rate limited: %s (%d/%ds)\n", ip, len(recent), int(reconnectWindow.Seconds()))
		return false
	}

	if activeConnections >= totalConnLimit {
		return false
	}
	if connCounts[ip] >= maxConnectionsPerIP {
		return false
	}

	return true
}

func incrementConnection(ip string) {
	connCountsLock.Lock()
	defer connCountsLock.Unlock()
	connCounts[ip]++
	activeConnections++
}

func decrementConnection(ip string) {
	connCountsLock.Lock()
	defer connCountsLock.Unlock()
	if connCounts[ip] > 0 {
		connCounts[ip]--
	}
	if activeConnections > 0 {
		activeConnections--
	}
}
