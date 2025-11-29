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
	LISTEN_PORT      = 4000
	BASE_PORT        = 3000
	TIMEOUT          = 30 * time.Second
	MIN_LOG_INTERVAL = 5 * time.Second
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
	loggedPorts     sync.Map // port uint16 -> last log time (time.Time)
)

// Извлечение SSRC из RTP заголовка
func getSSRC(packet []byte) (uint32, bool) {
	if len(packet) < 12 {
		return 0, false
	}
	ssrc := uint32(packet[8])<<24 | uint32(packet[9])<<16 | uint32(packet[10])<<8 | uint32(packet[11])
	return ssrc, true
}

// Анализ типа NAL-юнита в H.264
func analyzeNALType(payload []byte) string {
	if len(payload) < 1 {
		return "пустой пакет"
	}

	nalType := payload[0] & 0x1F // последние 5 бит

	switch nalType {
	case 1:
		return "P/B-кадр"
	case 5:
		return "I-кадр (IDR)"
	case 7:
		return "SPS"
	case 8:
		return "PPS"
	case 9:
		return "AUD"
	default:
		return fmt.Sprintf("NAL %d", nalType)
	}
}

// Запуск ffmpeg для записи видео по SDP
func startFFmpegRecording(ssrcPort uint16) (*exec.Cmd, error) {
	listenPort := ssrcPort - BASE_PORT
	filename := fmt.Sprintf("video_%d_%s.mp4", ssrcPort, time.Now().Format("20060102_150405"))

	sdpContent := fmt.Sprintf(`v=0
o=- 0 0 IN IP4 127.0.0.1
s=H.264 Video Stream
c=IN IP4 127.0.0.1
t=0 0
m=video %d RTP/AVP 96
a=rtpmap:96 H264/90000
a=fmtp:96 packetization-mode=1
`, listenPort)

	sdpFilename := fmt.Sprintf("temp_%d.sdp", ssrcPort)
	if err := os.WriteFile(sdpFilename, []byte(sdpContent), 0644); err != nil {
		return nil, fmt.Errorf("не удалось создать SDP файл: %w", err)
	}

	cmd := exec.Command("ffmpeg",
		"-protocol_whitelist", "file,udp,rtp",
		"-i", sdpFilename,
		"-c", "copy",
		"-f", "mp4",
		"-y",
		filename,
	)

	// Направляем stderr и stdout в лог для отладки
	cmd.Stderr = os.Stderr
	cmd.Stdout = os.Stdout

	if err := cmd.Start(); err != nil {
		os.Remove(sdpFilename) // очищаем временный файл
		return nil, fmt.Errorf("не удалось запустить ffmpeg: %w", err)
	}

	// Даем ffmpeg время начать слушать порт
	time.Sleep(1 * time.Second)

	// Запускаем горутину для очистки временного SDP файла после завершения ffmpeg
	go func(sdpFile string, process *exec.Cmd) {
		process.Wait()
		if err := os.Remove(sdpFile); err == nil {
			log.Printf("🧹 Временный SDP файл %s удален", sdpFile)
		}
	}(sdpFilename, cmd)

	log.Printf("🎥 FFmpeg слушает порт %d (через SDP), запись в %s", listenPort, filename)
	return cmd, nil
}

// Получение или создание стрима для порта (на основе SSRC)
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

// Автоочистка неактивных стримов
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

// Вспомогательная функция min
func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// Основная функция
func main() {
	recordEnv := strings.ToLower(os.Getenv("RECORD_STREAMS"))
	enableRecording = recordEnv == "true" || recordEnv == "1" || recordEnv == "yes"

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
		log.Printf("🚀 Слушаем RTP на :%d. FFmpeg записывает видео через временные SDP файлы. Таймаут: %v", LISTEN_PORT, TIMEOUT)
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

		// === ЛОГИРОВАНИЕ ТИПА КАДРА (один раз в N секунд) ===
		if enableRecording {
			shouldLog := false
			now := time.Now()

			if lastLogI, loaded := loggedPorts.Load(port); loaded {
				if now.Sub(lastLogI.(time.Time)) >= MIN_LOG_INTERVAL {
					shouldLog = true
					loggedPorts.Store(port, now)
				}
			} else {
				shouldLog = true
				loggedPorts.Store(port, now)
			}

			if shouldLog && n > 12 {
				payload := buffer[12:n]
				nalTypeDesc := analyzeNALType(payload)
				log.Printf("[Порт %d] 📊 Анализ: %s (первые байты: % X)", port, nalTypeDesc, payload[:min(8, len(payload))])
			}
		}
		// ===================================================

		// Отправляем копию на localhost для ffmpeg
		if enableRecording {
			localConn, err := net.DialUDP("udp", nil, &net.UDPAddr{
				IP:   net.IPv4(127, 0, 0, 1),
				Port: int(port - BASE_PORT),
			})
			if err == nil {
				localConn.Write(buffer[:n])
				localConn.Close()
			}
		}

		// Пересылаем оригиналу
		_, err = stream.conn.Write(buffer[:n])
		stream.mutex.Unlock()

		if err != nil {
			log.Printf("Ошибка отправки на порт %d: %v", port, err)
		}
	}
}
