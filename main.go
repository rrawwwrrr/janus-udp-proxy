package main

import (
	"fmt"
	"log"
	"net"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	LISTEN_PORT = 4000
	BASE_PORT   = 3000
	TIMEOUT     = 30 * time.Second
)

type Stream struct {
	conn     *net.UDPConn
	ffmpeg   *exec.Cmd
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

func startFFmpegRecording(ssrcPort uint16) (*exec.Cmd, error) {
	listenPort := ssrcPort - BASE_PORT
	filename := fmt.Sprintf("video_%d_%s.mp4", ssrcPort, time.Now().Format("20060102_150405"))

	// FFmpeg слушает listenPort, перенаправляет на ssrcPort и записывает в файл
	cmd := exec.Command("ffmpeg",
		"-f", "rtp", // входной формат RTP
		"-i", fmt.Sprintf("rtp://127.0.0.1:%d", listenPort), // слушаем порт BASE_PORT + SSRC
		"-c", "copy", // без перекодирования
		"-f", "rtp", // выходной формат RTP
		fmt.Sprintf("rtp://127.0.0.1:%d", ssrcPort), // пересылаем на оригинальный порт
		"-c", "copy", // запись в файл тоже без перекодирования
		"-y", // перезаписывать файл
		filename,
	)

	// Направляем stderr в лог для отладки
	cmd.Stderr = os.Stderr

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("не удалось запустить ffmpeg: %w", err)
	}

	// Даем ffmpeg время начать слушать порт
	time.Sleep(500 * time.Millisecond)

	log.Printf("🎥 FFmpeg слушает порт %d, пересылает на %d, запись в %s", listenPort, ssrcPort, filename)
	return cmd, nil
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

	var ffmpeg *exec.Cmd

	if enableRecording {
		ffmpeg, err = startFFmpegRecording(port)
		if err != nil {
			log.Printf("⚠️ Не удалось запустить запись для порта %d: %v", port, err)
			// Продолжаем работу без записи
		}
	}

	stream := &Stream{
		conn:     conn,
		ffmpeg:   ffmpeg,
		lastSeen: time.Now(),
	}

	streams[port] = stream

	if enableRecording && ffmpeg != nil {
		log.Printf("[Порт %d] Новый поток к %s, FFmpeg записывает", port, targetAddr)
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
				if s.ffmpeg != nil && s.ffmpeg.Process != nil {
					s.ffmpeg.Process.Signal(os.Interrupt)
					time.Sleep(100 * time.Millisecond)
					s.ffmpeg.Process.Kill()
					log.Printf("🎥 [Порт %d] Запись видео остановлена", port)
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

	// Проверяем что ffmpeg установлен
	if enableRecording {
		if _, err := exec.LookPath("ffmpeg"); err != nil {
			log.Printf("⚠️ FFmpeg не найден, запись видео отключена")
			enableRecording = false
		} else {
			log.Printf("✅ FFmpeg найден, запись видео доступна")
		}
	}

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
		log.Printf("🚀 Слушаем RTP на :%d. FFmpeg записывает видео (порт = BASE_PORT + SSRC). Таймаут: %v", LISTEN_PORT, TIMEOUT)
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

		// Отправляем копию трафика на порт для записи ffmpeg (SSRC - BASE_PORT )
		if enableRecording && stream.ffmpeg != nil {
			localConn, err := net.DialUDP("udp", nil, &net.UDPAddr{
				IP:   net.IPv4(127, 0, 0, 1),
				Port: int(port - BASE_PORT),
			})
			if err == nil {
				localConn.Write(buffer[:n])
				localConn.Close()
			}
		}

		// Отправляем по назначению (оригинальный адрес)
		_, err = stream.conn.Write(buffer[:n])
		stream.mutex.Unlock()

		if err != nil {
			log.Printf("Ошибка отправки на порт %d: %v", port, err)
		}
	}
}
