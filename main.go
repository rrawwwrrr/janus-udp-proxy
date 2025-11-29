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
	conn            *net.UDPConn
	ffmpeg          *exec.Cmd
	lastSeen        time.Time
	lastSeqNum      uint16
	lastTimestamp   uint32
	lastReceiveTime time.Time
	initialized     bool
	mutex           sync.RWMutex
}

var (
	streams         = make(map[uint16]*Stream)
	streamsMu       sync.RWMutex
	enableRecording bool
)

// Извлечение SSRC из RTP заголовка
func getSSRC(packet []byte) (uint32, bool) {
	if len(packet) < 12 {
		return 0, false
	}
	ssrc := uint32(packet[8])<<24 | uint32(packet[9])<<16 | uint32(packet[10])<<8 | uint32(packet[11])
	return ssrc, true
}

// Извлечение RTP sequence number и timestamp
func getRTPInfo(packet []byte) (seqNum uint16, timestamp uint32, err error) {
	if len(packet) < 12 {
		return 0, 0, fmt.Errorf("пакет слишком короткий (%d байт)", len(packet))
	}
	seqNum = uint16(packet[2])<<8 | uint16(packet[3])
	timestamp = uint32(packet[4])<<24 | uint32(packet[5])<<16 | uint32(packet[6])<<8 | uint32(packet[7])
	return seqNum, timestamp, nil
}

// Расчёт потерянных пакетов с учётом переполнения 16-битного sequence number
func calculateLostPackets(lastSeq, currentSeq uint16) uint16 {
	if currentSeq == lastSeq {
		return 0 // дубликат
	}

	diff := int(currentSeq) - int(lastSeq)
	if diff < 0 {
		diff += 0x10000 // 65536
	}

	if diff == 0 {
		return 0 // дубликат (после коррекции)
	}
	return uint16(diff - 1)
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

	cmd.Stderr = os.Stderr
	cmd.Stdout = os.Stdout

	if err := cmd.Start(); err != nil {
		os.Remove(sdpFilename)
		return nil, fmt.Errorf("не удалось запустить ffmpeg: %w", err)
	}

	time.Sleep(1 * time.Second)

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
		}
	}

	stream := &Stream{
		conn:            conn,
		ffmpeg:          ffmpeg,
		lastSeen:        time.Now(),
		lastSeqNum:      0,
		lastTimestamp:   0,
		lastReceiveTime: time.Now(),
		initialized:     false,
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
		receiveTime := time.Now() // Фиксируем время получения пакета

		// === 🔥 ДЕТАЛЬНЫЙ АНАЛИЗ ПАКЕТА ===
		if n > 12 {
			// 1. Извлекаем RTP информацию
			seqNum, timestamp, rtpErr := getRTPInfo(buffer[:n])
			if rtpErr != nil {
				log.Printf(rtpErr.Error())
			}
			nalTypeDesc := "неизвестно"
			payload := buffer[12:n]

			// 2. Анализируем тип кадра
			if len(payload) > 0 {
				nalTypeDesc = analyzeNALType(payload)
			}

			// 3. Логируем I-кадры мгновенно
			if nalTypeDesc == "I-кадр (IDR)" {
				log.Printf("🔥 [Порт %d] I-КАДР (IDR) seq=%d, ts=%d, размер=%d байт, время=%s",
					port, seqNum, timestamp, n, receiveTime.Format("15:04:05.000"))
			}

			// 4. Проверка потерь
			lostPackets := uint16(0)
			if stream.initialized {
				lostPackets = calculateLostPackets(stream.lastSeqNum, seqNum)
				if lostPackets > 0 {
					log.Printf("⚠️ [Порт %d] ПОТЕРЯНО %d ПАКЕТОВ! [последний seq=%d → текущий seq=%d]",
						port, lostPackets, stream.lastSeqNum, seqNum)
				}
			} else {
				stream.initialized = true
				log.Printf("🟢 [Порт %d] СТАРТ ПОТОКА seq=%d, ts=%d, тип=%s",
					port, seqNum, timestamp, nalTypeDesc)
			}

			// 5. Анализ задержки (ИСПРАВЛЕНО!)
			if stream.initialized {
				receiveTimeMS := uint64(receiveTime.UnixNano() / 1e6)

				if stream.lastTimestamp > 0 {
					// Вычисляем ожидаемый интервал между пакетами (в миллисекундах)
					expectedInterval := (timestamp - stream.lastTimestamp) / 90
					actualInterval := receiveTimeMS - uint64(stream.lastReceiveTime.UnixNano()/1e6)

					if actualInterval > uint64(expectedInterval)+50 {
						log.Printf("⏱️ [Порт %d] ЗАДЕРЖКА! Ожидаемый интервал: %dms, Фактический: %dms (seq=%d)",
							port, expectedInterval, actualInterval, seqNum)
					}
				}
				stream.lastReceiveTime = receiveTime
			}

			// 6. Обновляем состояние потока
			stream.lastSeqNum = seqNum
			stream.lastTimestamp = timestamp
		}
		// ================================

		// Отправляем копию трафика на порт для записи ffmpeg (BASE_PORT + SSRC)
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

		// Отправляем по назначению (оригинальный адрес)
		_, err = stream.conn.Write(buffer[:n])
		stream.mutex.Unlock()

		if err != nil {
			log.Printf("Ошибка отправки на порт %d: %v", port, err)
		}
	}
}
