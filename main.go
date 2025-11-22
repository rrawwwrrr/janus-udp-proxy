package main

import (
	"fmt"
	"log"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	LISTEN_PORT = 4000
	TIMEOUT     = 30 * time.Second
)

type Stream struct {
	conn     *net.UDPConn
	file     *os.File
	lastSeen time.Time
	mutex    sync.RWMutex
}

var (
	streams         = make(map[uint16]*Stream)
	streamsMu       sync.RWMutex
	enableRecording bool
)

func getSSRC(packet []byte) (uint32, bool) {
	if len(packet) < 12 {
		return 0, false
	}
	ssrc := uint32(packet[8])<<24 | uint32(packet[9])<<16 | uint32(packet[10])<<8 | uint32(packet[11])
	return ssrc, true
}

func createSDPFile(port uint16, filename string) error {
	sdpContent := fmt.Sprintf(`v=0
o=- %d 1 IN IP4 0.0.0.0
s=Stream from port %d
c=IN IP4 0.0.0.0
t=0 0
m=audio %d RTP/AVP 96
a=rtpmap:96 OPUS/48000/2
`, time.Now().Unix(), port, port)

	sdpFilename := filename + ".sdp"
	return os.WriteFile(sdpFilename, []byte(sdpContent), 0644)
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

	var file *os.File
	var filename string

	if enableRecording {
		// Создаем файл для записи RTP
		filename = fmt.Sprintf("stream_%d_%s", port, time.Now().Format("20060102_150405"))
		file, err = os.Create(filename + ".rtp")
		if err != nil {
			conn.Close()
			return nil, fmt.Errorf("не удалось создать файл %s: %w", filename, err)
		}

		// Создаем SDP файл
		if err := createSDPFile(port, filename); err != nil {
			log.Printf("⚠️ Не удалось создать SDP файл для порта %d: %v", port, err)
		} else {
			log.Printf("📄 Создан SDP файл: %s.sdp", filename)
		}
	}

	stream := &Stream{
		conn:     conn,
		file:     file,
		lastSeen: time.Now(),
	}

	streams[port] = stream

	if enableRecording {
		log.Printf("[Порт %d] Новый поток к %s, запись в %s.rtp", port, targetAddr, filename)
	} else {
		log.Printf("[Порт %d] Новый поток к %s", port, targetAddr)
	}

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
				if s.file != nil {
					s.file.Close()
					log.Printf("💾 [Порт %d] Файл закрыт", port)
				}
				delete(streams, port)
				log.Printf("🗑️ [Порт %d] Удалён", port)
			}
			streamsMu.Unlock()

			return
		}
	}
}

func main() {
	// Проверяем переменную окружения для записи в файл
	recordEnv := strings.ToLower(os.Getenv("RECORD_STREAMS"))
	enableRecording = recordEnv == "true" || recordEnv == "1" || recordEnv == "yes"

	addr, err := net.ResolveUDPAddr("udp", fmt.Sprintf(":%d", LISTEN_PORT))
	if err != nil {
		log.Fatal("ResolveUDPAddr: ", err)
	}

	conn, err := net.ListenUDP("udp", addr)
	if err != nil {
		log.Fatal("ListenUDP: ", err)
	}
	defer conn.Close()

	if enableRecording {
		log.Printf("🚀 Слушаем RTP на :%d. Запись ВКЛЮЧЕНА. Таймаут: %v", LISTEN_PORT, TIMEOUT)
	} else {
		log.Printf("🚀 Слушаем RTP на :%d. Запись ОТКЛЮЧЕНА. Таймаут: %v", LISTEN_PORT, TIMEOUT)
	}

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

		// Записываем в файл если включено
		if enableRecording && stream.file != nil {
			if _, err := stream.file.Write(buffer[:n]); err != nil {
				log.Printf("Ошибка записи в файл для порта %d: %v", port, err)
			}
		}

		// Отправляем по назначению
		_, err = stream.conn.Write(buffer[:n])
		stream.mutex.Unlock()

		if err != nil {
			log.Printf("Ошибка отправки на порт %d: %v", port, err)
		}
	}
}
