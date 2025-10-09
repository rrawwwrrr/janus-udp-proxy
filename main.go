package main

import (
	"fmt"
	"log"
	"net"
	"os"
	"strconv"
	"sync"
	"time"
)

const (
	LISTEN_PORT = 4000
	TIMEOUT     = 30 * time.Second
)

type Stream struct {
	conn     *net.UDPConn
	lastSeen time.Time
	mutex    sync.RWMutex
}

var (
	streams   = make(map[uint16]*Stream)
	streamsMu sync.RWMutex
)

func getSSRC(packet []byte) (uint32, bool) {
	if len(packet) < 12 {
		return 0, false
	}
	ssrc := uint32(packet[8])<<24 | uint32(packet[9])<<16 | uint32(packet[10])<<8 | uint32(packet[11])
	return ssrc, true
}

func getOrCreateStream(port uint16) (*Stream, error) {
	streamsMu.Lock()
	defer streamsMu.Unlock()

	if stream, exists := streams[port]; exists {
		return stream, nil
	}

	targetHost := os.Getenv("UDP_TARGET_HOST")
	if targetHost == "" {
		targetHost = "127.0.0.1"
	}

	targetAddr := net.JoinHostPort(targetHost, strconv.Itoa(int(port)))

	udpAddr, err := net.ResolveUDPAddr("udp", targetAddr)
	if err != nil {
		return nil, fmt.Errorf("не удалось разрешить адрес %s: %w", targetAddr, err)
	}

	conn, err := net.DialUDP("udp", nil, udpAddr)
	if err != nil {
		return nil, fmt.Errorf("не удалось открыть UDP-сокет к %s: %w", targetAddr, err)
	}

	stream := &Stream{
		conn:     conn,
		lastSeen: time.Now(),
	}

	streams[port] = stream
	log.Printf("[Порт %d] Новый поток к %s", port, targetAddr)

	go autoCleanup(port)

	return stream, nil
}

func autoCleanup(port uint16) {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	for range ticker.C {
		streamsMu.RLock()
		stream, exists := streams[port]
		streamsMu.RUnlock()

		if !exists {
			return
		}

		stream.mutex.RLock()
		lastSeen := stream.lastSeen
		stream.mutex.RUnlock()

		if time.Since(lastSeen) > TIMEOUT {
			log.Printf("🧹 [Порт %d] Таймаут — закрываем", port)

			streamsMu.Lock()
			if s, exists := streams[port]; exists {
				s.conn.Close()
				delete(streams, port)
				log.Printf("🗑️ [Порт %d] Удалён", port)
			}
			streamsMu.Unlock()

			return
		}
	}
}

func main() {
	addr, err := net.ResolveUDPAddr("udp", fmt.Sprintf(":%d", LISTEN_PORT))
	if err != nil {
		log.Fatal("ResolveUDPAddr: ", err)
	}

	conn, err := net.ListenUDP("udp", addr)
	if err != nil {
		log.Fatal("ListenUDP: ", err)
	}
	defer conn.Close()

	log.Printf("🚀 Слушаем RTP на :%d. SSRC = порт назначения. Таймаут: %v", LISTEN_PORT, TIMEOUT)

	buffer := make([]byte, 1500)

	for {
		n, _, err := conn.ReadFromUDP(buffer)
		if err != nil {
			log.Printf("Ошибка чтения: %v", err)
			continue
		}

		ssrc, ok := getSSRC(buffer[:n])
		if !ok {
			continue
		}

		port := uint16(ssrc)

		// → Убрали проверку MIN/MAX — доверяем API
		// Единственное — проверим, что порт не 0 (некорректный SSRC)
		if port == 0 {
			log.Printf("Пропускаем SSRC=0 (некорректный RTP)")
			continue
		}

		stream, err := getOrCreateStream(port)
		if err != nil {
			log.Printf("%v", err)
			continue
		}

		stream.mutex.Lock()
		stream.lastSeen = time.Now()
		stream.mutex.Unlock()

		_, err = stream.conn.Write(buffer[:n])
		if err != nil {
			log.Printf("Ошибка отправки на порт %d: %v", port, err)
		}
	}
}
