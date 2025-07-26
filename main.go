package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"
)

const (
	tcpPort             = ":9999"
	udpPort             = ":9998"
	maxReconnectsPerIP  = 5
	reconnectWindow     = 5 * time.Second
	maxConnectionsPerIP = 3
	totalConnLimit      = 500
)

type Player struct {
	tcpConn net.Conn
	ip      string
	udpPort int
	addr    *net.TCPAddr
}

var (
	waitingQueue        []*Player
	udpPorts            = make(map[string]int) // IP → last known UDP port
	reconnectTimestamps = make(map[string][]time.Time)
	connCounts          = make(map[string]int)
	activeConnections   = 0

	queueLock      sync.Mutex
	connCountsLock sync.Mutex
	udpLock        sync.Mutex
)

func main() {
	ctx, cancel := context.WithCancel(context.Background())

	// Listen for system signals (e.g. Ctrl+C)
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM, syscall.SIGTSTP)

	// TCP and UDP listener references
	var tcpLn net.Listener
	var udpLn net.PacketConn
	var wg sync.WaitGroup

	go func() {
		<-sigChan
		fmt.Println("\n🛑 Shutting down gracefully...")
		cancel()
		if tcpLn != nil {
			tcpLn.Close()
		}
		if udpLn != nil {
			udpLn.Close()
		}
	}()

	wg.Add(2)
	go func() {
		defer wg.Done()
		udpLn = udpListen(ctx)
	}()
	go func() {
		defer wg.Done()
		tcpLn = tcpListen(ctx)
	}()

	wg.Wait()
	fmt.Println("✅ Clean shutdown complete.")
}

func udpListen(ctx context.Context) net.PacketConn {
	pc, err := net.ListenPacket("udp", udpPort)
	if err != nil {
		panic(err)
	}
	fmt.Println("📡 UDP listener on", udpPort)

	go func() {
		<-ctx.Done()
		fmt.Println("📴 Closing UDP listener")
		pc.Close()
	}()

	buf := make([]byte, 1024)
	for {
		select {
		case <-ctx.Done():
			return pc
		default:
			pc.SetReadDeadline(time.Now().Add(1 * time.Second)) // unblock on shutdown
			n, addr, err := pc.ReadFrom(buf)
			if err != nil {
				if os.IsTimeout(err) || ctx.Err() != nil {
					continue
				}
				fmt.Println("UDP read error:", err)
				continue
			}
			text := strings.TrimSpace(string(buf[:n]))
			fmt.Printf("UDP text \"%s\" received from %s:%d\n", text, getIP(addr), getUDPPort(addr))
			if text == "hello" {
				ip := getIP(addr)
				port := getUDPPort(addr)
				key := ip + fmt.Sprint(port)

				udpLock.Lock()
				udpPorts[key] = port
				udpLock.Unlock()

				fmt.Printf("👋 UDP hello from %s:%d\n", ip, port)

				// Respond with the visible remote port (NAT source port)
				response := map[string]any{
					"your_udp_port": port,
				}
				jsonBytes, err := json.Marshal(response)
				if err == nil {
					pc.WriteTo(jsonBytes, addr)
					fmt.Printf("📤 Sent UDP response to %s:%d → %s\n", ip, port, jsonBytes)
				}
			}

		}
	}
}

func tcpListen(ctx context.Context) net.Listener {
	ln, err := net.Listen("tcp", tcpPort)
	if err != nil {
		panic(err)
	}
	fmt.Println("🧠 TCP matchmaking listening on", tcpPort)

	go func() {
		<-ctx.Done()
		fmt.Println("📴 Closing TCP listener")
		ln.Close()
	}()

	for {
		conn, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return ln // context was canceled
			}
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
