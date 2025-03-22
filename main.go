package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
)

type VideoItem struct {
	Name     string      `json:"name"`
	Path     string      `json:"path"`
	IsDir    bool        `json:"isDir"`
	Children []VideoItem `json:"children,omitempty"`
}

func getVideoList(directory string, relativePath string) []VideoItem {
	var videos []VideoItem
	files, err := ioutil.ReadDir(directory)
	if err != nil {
		log.Printf("Error reading directory: %v", err)
		return videos
	}

	for _, file := range files {
		currentPath := filepath.Join(directory, file.Name())
		relativeName := filepath.Join(relativePath, file.Name())

		if file.IsDir() {
			children := getVideoList(currentPath, relativeName)
			if len(children) > 0 {
				videos = append(videos, VideoItem{
					Name:     file.Name(),
					Path:     relativeName,
					IsDir:    true,
					Children: children,
				})
			}
		} else {
			ext := strings.ToLower(filepath.Ext(file.Name()))
			if ext == ".mp4" || ext == ".webm" || ext == ".mkv" {
				videos = append(videos, VideoItem{
					Name:  file.Name(),
					Path:  "/video/" + relativeName,
					IsDir: false,
				})
			}
		}
	}
	return videos
}

// HLSConfig 配置HLS流参数
type HLSConfig struct {
	SegmentDuration  int    // 段时长(秒)
	PlaylistSize     int    // 播放列表大小(段数量)
	OutputDir        string // 输出目录
	FFmpegBin        string // FFmpeg可执行文件路径
	EnableTranscoding bool   // 是否启用转码
	VideoCodec       string // 视频编码
	AudioCodec       string // 音频编码
}

// HLSManager 管理HLS转换和分发
type HLSManager struct {
	config       HLSConfig
	cmd          *exec.Cmd
	mutex        sync.Mutex
	isRunning    bool
	lastError    error
	streamActive bool
}

// NewHLSManager 创建新的HLS管理器
func NewHLSManager(config HLSConfig) *HLSManager {
	// 确保输出目录存在
	os.MkdirAll(config.OutputDir, 0755)
	
	return &HLSManager{
		config:      config,
		isRunning:   false,
		streamActive: false,
	}
}

// StartTranscoding 开始将RTP流转为HLS
func (h *HLSManager) StartTranscoding(rtpAddress string) error {
	h.mutex.Lock()
	defer h.mutex.Unlock()

	if h.isRunning {
		return fmt.Errorf("转码已在运行")
	}

	// 检查FFmpeg是否存在
	ffmpegPath := h.config.FFmpegBin
	if ffmpegPath == "" {
		ffmpegPath = "ffmpeg" // 使用系统路径查找
	}

	// 构建FFmpeg命令
	args := []string{
		"-y",                     // 覆盖输出文件
		"-i", "rtp://" + rtpAddress, // RTP输入地址
		"-fflags", "nobuffer",    // 减少缓冲
		"-flags", "low_delay",    // 低延迟模式
		"-max_delay", "5000000", // 5 seconds
		"-fifo_size", "1000000",
	}

	// 如果启用转码
	if h.config.EnableTranscoding {
		if h.config.VideoCodec != "" {
			args = append(args, "-c:v", h.config.VideoCodec)
		} else {
			args = append(args, "-c:v", "copy") // 默认复制视频流
		}
		
		if h.config.AudioCodec != "" {
			args = append(args, "-c:a", h.config.AudioCodec)
		} else {
			args = append(args, "-c:a", "copy") // 默认复制音频流
		}
	} else {
		// 不转码，只复制流
		args = append(args, "-c", "copy")
	}

	// HLS特定参数
	args = append(args, []string{
		"-hls_time", fmt.Sprintf("%d", h.config.SegmentDuration),
		"-hls_list_size", fmt.Sprintf("%d", h.config.PlaylistSize),
		"-hls_flags", "delete_segments+append_list+omit_endlist",  // Add omit_endlist flag
		"-hls_segment_type", "mpegts",
		"-hls_segment_filename", filepath.Join(h.config.OutputDir, "segment_%03d.ts"),
		filepath.Join(h.config.OutputDir, "playlist.m3u8"),
	}...)

	// 创建FFmpeg命令
	cmd := exec.Command(ffmpegPath, args...)
	
	// 捕获错误输出用于调试
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return fmt.Errorf("创建stderr管道失败: %v", err)
	}

	// 启动命令
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("启动FFmpeg失败: %v", err)
	}

	h.cmd = cmd
	h.isRunning = true
	h.streamActive = true

	// 处理FFmpeg输出
	go func() {
		buf := make([]byte, 4096)
		for {
			n, err := stderr.Read(buf)
			if n > 0 {
				log.Printf("FFmpeg: %s", string(buf[:n]))
			}
			if err != nil {
				if err != io.EOF {
					h.mutex.Lock()
					h.lastError = fmt.Errorf("读取FFmpeg输出错误: %v", err)
					h.mutex.Unlock()
				}
				break
			}
		}
	}()

	// 监控FFmpeg进程
	go func() {
		err := cmd.Wait()
		h.mutex.Lock()
		defer h.mutex.Unlock()
		
		h.isRunning = false
		if err != nil {
			h.lastError = fmt.Errorf("FFmpeg进程退出: %v", err)
		}
		log.Printf("转码进程已结束, 错误: %v", h.lastError)
		
		// 重启转码，如果流仍然活跃
		if h.streamActive {
			log.Println("尝试自动重启转码...")
			h.mutex.Unlock() // 临时释放锁，防止死锁
			err := h.StartTranscoding(rtpAddress)
			h.mutex.Lock() // 重新获取锁
			if err != nil {
				log.Printf("重启转码失败: %v", err)
			}
		}
	}()

	log.Printf("开始将RTP流(%s)转为HLS", rtpAddress)
	return nil
}

// StopTranscoding 停止转码进程
func (h *HLSManager) StopTranscoding() error {
	h.mutex.Lock()
	defer h.mutex.Unlock()

	h.streamActive = false

	if !h.isRunning || h.cmd == nil {
		return nil
	}

	// 发送终止信号
	if err := h.cmd.Process.Signal(syscall.SIGTERM); err != nil {
		// 如果失败，强制终止
		if killErr := h.cmd.Process.Kill(); killErr != nil {
			return fmt.Errorf("无法终止FFmpeg进程: %v", killErr)
		}
	}

	// 等待进程完全退出的超时机制
	done := make(chan error, 1)
	go func() {
		done <- h.cmd.Wait()
	}()

	select {
	case <-done:
		// 进程已经退出
	case <-time.After(5 * time.Second):
		// 超时，强制终止
		if err := h.cmd.Process.Kill(); err != nil {
			return fmt.Errorf("强制终止FFmpeg超时: %v", err)
		}
	}

	h.isRunning = false
	h.cmd = nil
	log.Println("HLS转码已停止")
	return nil
}

// IsRunning 检查转码是否在运行
func (h *HLSManager) IsRunning() bool {
	h.mutex.Lock()
	defer h.mutex.Unlock()
	return h.isRunning
}

// GetLastError 获取最后一个错误
func (h *HLSManager) GetLastError() error {
	h.mutex.Lock()
	defer h.mutex.Unlock()
	return h.lastError
}

// RegisterHLSHandlers 注册HLS相关的HTTP处理函数
func RegisterHLSHandlers(hlsManager *HLSManager) {
	http.HandleFunc("/hls/", func(w http.ResponseWriter, r *http.Request) {
		// CORS头
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, OPTIONS")
		
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusOK)
			return
		}
		
		// 获取请求的文件路径
		requestPath := strings.TrimPrefix(r.URL.Path, "/hls/")
		filePath := filepath.Join(hlsManager.config.OutputDir, requestPath)
		
		// 安全检查：确保路径不跳出指定目录
		if !strings.HasPrefix(filepath.Clean(filePath), filepath.Clean(hlsManager.config.OutputDir)) {
			http.Error(w, "Forbidden", http.StatusForbidden)
			return
		}
		
		// 设置恰当的内容类型
		if strings.HasSuffix(filePath, ".m3u8") {
			w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
		} else if strings.HasSuffix(filePath, ".ts") {
			w.Header().Set("Content-Type", "video/mp2t")
		}
		
		// 提供文件
		http.ServeFile(w, r, filePath)
	})

	// HLS转码控制API
	http.HandleFunc("/api/hls/start", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "POST, OPTIONS")
		
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusOK)
			return
		}
		
		if r.Method != http.MethodPost {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}
		
		// 从请求中获取RTP地址
		rtpAddr := r.URL.Query().Get("rtp_addr")
		if rtpAddr == "" {
			rtpAddr = multicastAddr // 使用默认多播地址
		}
		
		// 启动转码
		err := hlsManager.StartTranscoding(rtpAddr)
		if err != nil {
			http.Error(w, fmt.Sprintf("启动转码失败: %v", err), http.StatusInternalServerError)
			return
		}
		
		json.NewEncoder(w).Encode(map[string]string{
			"status": "success",
			"message": fmt.Sprintf("正在将RTP流(%s)转为HLS", rtpAddr),
		})
	})
	
	http.HandleFunc("/api/hls/stop", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "POST, OPTIONS")
		
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusOK)
			return
		}
		
		if r.Method != http.MethodPost {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}
		
		// 停止转码
		err := hlsManager.StopTranscoding()
		if err != nil {
			http.Error(w, fmt.Sprintf("停止转码失败: %v", err), http.StatusInternalServerError)
			return
		}
		
		json.NewEncoder(w).Encode(map[string]string{
			"status": "success",
			"message": "HLS转码已停止",
		})
	})
	
	http.HandleFunc("/api/hls/status", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Access-Control-Allow-Origin", "*")
		
		status := map[string]interface{}{
			"running": hlsManager.IsRunning(),
		}
		
		if err := hlsManager.GetLastError(); err != nil {
			status["error"] = err.Error()
		}
		
		json.NewEncoder(w).Encode(status)
	})
}

// 添加HLS播放器页面
func addHLSPlayer(hlsManager *HLSManager) {
	http.HandleFunc("/hls-player", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=UTF-8")
		html := `
        <!DOCTYPE html>
        <html>
        <head>
            <title>HLS流播放器</title>
            <style>
                body { font-family: Arial, sans-serif; margin: 20px; }
                .container { max-width: 800px; margin: 0 auto; }
                .form-group { margin-bottom: 15px; }
                label { display: block; margin-bottom: 5px; }
                input, button { padding: 8px; }
                button { cursor: pointer; margin-right: 10px; }
                video { width: 100%; max-height: 450px; background: #000; }
                .controls { margin-top: 15px; }
            </style>
            <script src="https://cdn.jsdelivr.net/npm/hls.js@latest"></script>
        </head>
        <body>
            <div class="container">
                <h1>HLS流播放器</h1>
                
                <div id="videoContainer">
                    <video id="video" controls></video>
                </div>
                
                <div class="controls">
                    <div class="form-group">
                        <label for="rtpAddr">RTP流地址(如需自定义)</label>
                        <input type="text" id="rtpAddr" placeholder="224.30.30.226:8245">
                    </div>
                    
                    <button id="startBtn">开始转码并播放</button>
                    <button id="stopBtn">停止转码</button>
                </div>
                
                <div id="status" style="margin-top: 15px;"></div>
            </div>
            
            <script>
                const video = document.getElementById('video');
                const startBtn = document.getElementById('startBtn');
                const stopBtn = document.getElementById('stopBtn');
                const rtpAddrInput = document.getElementById('rtpAddr');
                const statusDiv = document.getElementById('status');
                let hlsPlayer = null;
                
                function showStatus(message, isError = false) {
                    statusDiv.textContent = message;
                    statusDiv.style.color = isError ? 'red' : 'green';
                }
                
                // 检查HLS.js是否可用
                if (Hls.isSupported()) {
                    setupHlsPlayer();
                } else if (video.canPlayType('application/vnd.apple.mpegurl')) {
                    // 对于支持HLS但不支持HLS.js的设备(如iOS的Safari)
                    video.src = '/hls/playlist.m3u8';
                    video.addEventListener('loadedmetadata', function() {
                        showStatus('使用原生HLS支持播放');
                    });
                } else {
                    showStatus('您的浏览器不支持HLS播放', true);
                }
                
                function setupHlsPlayer() {
                    if (hlsPlayer) {
                        hlsPlayer.destroy();
                    }
                    
                    hlsPlayer = new Hls({
                        debug: false,
                        enableWorker: true,
                        lowLatencyMode: true,
                    });
                    
                    hlsPlayer.attachMedia(video);
                    hlsPlayer.on(Hls.Events.MEDIA_ATTACHED, function() {
                        hlsPlayer.loadSource('/hls/playlist.m3u8');
                    });
                    
                    hlsPlayer.on(Hls.Events.MANIFEST_PARSED, function() {
                        showStatus('HLS流已加载，正在播放');
                        video.play();
                    });
                    
                    hlsPlayer.on(Hls.Events.ERROR, function(event, data) {
                        if (data.fatal) {
                            switch(data.type) {
                                case Hls.ErrorTypes.NETWORK_ERROR:
                                    // 尝试重连
                                    showStatus('网络错误，尝试重连...', true);
                                    hlsPlayer.startLoad();
                                    break;
                                case Hls.ErrorTypes.MEDIA_ERROR:
                                    showStatus('媒体错误，尝试恢复...', true);
                                    hlsPlayer.recoverMediaError();
                                    break;
                                default:
                                    // 无法恢复的错误
                                    showStatus('播放错误: ' + data.details, true);
                                    hlsPlayer.destroy();
                                    break;
                            }
                        }
                    });
                }
                
                // 启动转码并播放
                startBtn.addEventListener('click', async function() {
                    let rtpAddr = rtpAddrInput.value.trim();
                    
                    // 显示加载状态
                    showStatus('正在启动转码...');
                    startBtn.disabled = true;
                    
                    try {
                        // 构建API请求URL
                        let apiUrl = '/api/hls/start';
                        if (rtpAddr) {
                            apiUrl += '?rtp_addr=' + encodeURIComponent(rtpAddr);
                        }
                        
                        // 发送启动请求
                        const response = await fetch(apiUrl, {method: 'POST'});
                        const result = await response.json();
                        
                        if (response.ok) {
                            showStatus('转码已启动，准备播放');
                            
                            // 等待几秒后初始化播放器
                            setTimeout(() => {
                                if (Hls.isSupported()) {
                                    setupHlsPlayer();
                                } else if (video.canPlayType('application/vnd.apple.mpegurl')) {
                                    video.src = '/hls/playlist.m3u8';
                                    video.play();
                                }
                            }, 3000);
                        } else {
                            showStatus('启动转码失败: ' + result.message, true);
                        }
                    } catch (error) {
                        showStatus('请求错误: ' + error.message, true);
                    } finally {
                        startBtn.disabled = false;
                    }
                });
                
                // 停止转码
                stopBtn.addEventListener('click', async function() {
                    try {
                        // 发送停止请求
                        const response = await fetch('/api/hls/stop', {method: 'POST'});
                        const result = await response.json();
                        
                        if (response.ok) {
                            showStatus('转码已停止');
                            
                            // 停止播放器
                            if (hlsPlayer) {
                                hlsPlayer.destroy();
                                hlsPlayer = null;
                            }
                            video.pause();
                            video.removeAttribute('src');
                            video.load();
                        } else {
                            showStatus('停止转码失败: ' + result.message, true);
                        }
                    } catch (error) {
                        showStatus('请求错误: ' + error.message, true);
                    }
                });
                
                // 页面加载时检查转码状态
                async function checkTranscodingStatus() {
                    try {
                        const response = await fetch('/api/hls/status');
                        const result = await response.json();
                        
                        if (result.running) {
                            showStatus('转码已在运行，可以播放');
                            
                            // 初始化播放器
                            if (Hls.isSupported()) {
                                setupHlsPlayer();
                            } else if (video.canPlayType('application/vnd.apple.mpegurl')) {
                                video.src = '/hls/playlist.m3u8';
                                video.play();
                            }
                        }
                    } catch (error) {
                        console.error('检查转码状态失败:', error);
                    }
                }
                
                // 页面加载时检查状态
                window.addEventListener('DOMContentLoaded', checkTranscodingStatus);
            </script>
        </body>
        </html>
        `
        w.Write([]byte(html))
    })
}

type HDMIPlayer struct {
	mutex        sync.Mutex
	cmd          *exec.Cmd
	isPlaying    bool
	playingURL   string
	lastError    error
	playerBin    string // omxplayer或其他播放器路径
}

// NewHDMIPlayer 创建新的HDMI播放器
func NewHDMIPlayer(playerBin string) *HDMIPlayer {
	// 如果未指定播放器路径，默认使用cvlc (命令行版VLC)
	if playerBin == "" {
		playerBin = "cvlc"
	}
	
	return &HDMIPlayer{
		playerBin: playerBin,
		isPlaying: false,
	}
}

// StartPlay 开始通过HDMI播放RTP流
func (h *HDMIPlayer) StartPlay(rtpURL string) error {
	h.mutex.Lock()
	defer h.mutex.Unlock()
	
	if h.isPlaying {
		return fmt.Errorf("已有视频正在播放")
	}
	
	// 检查播放器是否存在
	_, err := exec.LookPath(h.playerBin)
	if err != nil {
		return fmt.Errorf("找不到播放器 %s: %v", h.playerBin, err)
	}
	
	// 构建播放命令
	var args []string
	
	// 使用不同播放器的对应参数
	if h.playerBin == "omxplayer" {
		args = []string{
			"--live",              // 低延迟模式
			"--adev", "hdmi",      // 音频输出到HDMI
			"--no-osd",            // 不显示屏幕信息
			"--timeout", "30",     // 连接超时时间
			"--avdict", "rtsp_transport:udp", // 使用UDP传输
			rtpURL,
		}
	} else if strings.HasSuffix(h.playerBin, "vlc") || strings.HasSuffix(h.playerBin, "cvlc") {
		// VLC命令行参数
		args = []string{
			"--fullscreen",        // 全屏显示
			"--no-video-title",    // 不显示标题
			"--aout=alsa",         // 使用ALSA而非PulseAudio进行音频输出
			"--network-caching=100", // 低缓存值用于降低延迟
			"--file-caching=100",
			"--sout-mux-caching=100",
			"--no-audio-time-stretch", // 禁用时间拉伸
			"--no-video-deco",      // 无装饰
			"--quiet",              // 减少错误信息
			rtpURL,                // 播放URL
			"vlc://quit",          // 播放完毕后退出
		}
	} else {
		// 通用参数，只传入URL
		args = []string{rtpURL}
	}
	
	// 创建命令
	cmd := exec.Command(h.playerBin, args...)
	
	// 捕获输出用于调试
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("创建stdout管道失败: %v", err)
	}
	
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return fmt.Errorf("创建stderr管道失败: %v", err)
	}
	
	// 启动命令
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("启动播放器失败: %v", err)
	}
	
	h.cmd = cmd
	h.isPlaying = true
	h.playingURL = rtpURL
	h.lastError = nil
	
	// 处理输出
	go func() {
		buf := make([]byte, 4096)
		for {
			n, err := stdout.Read(buf)
			if n > 0 {
				log.Printf("播放器输出: %s", string(buf[:n]))
			}
			if err != nil {
				break
			}
		}
	}()
	
	go func() {
		buf := make([]byte, 4096)
		for {
			n, err := stderr.Read(buf)
			if n > 0 {
				log.Printf("播放器错误: %s", string(buf[:n]))
			}
			if err != nil {
				break
			}
		}
	}()
	
	// 监控播放进程
	go func() {
		err := cmd.Wait()
		h.mutex.Lock()
		defer h.mutex.Unlock()
		
		h.isPlaying = false
		h.cmd = nil
		
		if err != nil && err.Error() != "exit status 0" {
			h.lastError = fmt.Errorf("播放器退出: %v", err)
			log.Printf("播放停止，错误: %v", err)
		} else {
			log.Println("播放正常结束")
		}
	}()
	
	log.Printf("开始通过HDMI播放RTP流: %s", rtpURL)
	return nil
}

// StopPlay 停止播放
func (h *HDMIPlayer) StopPlay() error {
	h.mutex.Lock()
	defer h.mutex.Unlock()
	
	if (!h.isPlaying || h.cmd == nil) {
		return nil // 已经停止了
	}
	
	// 向进程发送退出命令
	if h.playerBin == "omxplayer" {
		// omxplayer 特定的停止逻辑
		if err := h.cmd.Process.Signal(syscall.SIGINT); err != nil {
			log.Printf("发送SIGINT失败: %v，尝试强制终止", err)
		} else {
			// 给一点时间优雅退出
			time.Sleep(500 * time.Millisecond)
		}
	} else if strings.HasSuffix(h.playerBin, "vlc") || strings.HasSuffix(h.playerBin, "cvlc") {
		// VLC 可以用 SIGTERM 或 SIGINT 信号终止
		if err := h.cmd.Process.Signal(syscall.SIGTERM); err != nil {
			log.Printf("发送SIGTERM到VLC失败: %v，尝试强制终止", err)
		} else {
			// 给VLC一点时间优雅退出
			time.Sleep(1000 * time.Millisecond)
		}
	}
	
	// 如果进程仍在运行，发送终止信号
	if h.cmd.ProcessState == nil || !h.cmd.ProcessState.Exited() {
		if err := h.cmd.Process.Signal(syscall.SIGTERM); err != nil {
			// 如果失败，强制终止
			if killErr := h.cmd.Process.Kill(); killErr != nil {
				return fmt.Errorf("无法终止播放进程: %v", killErr)
			}
		}
	}
	
	// 等待进程完全退出的超时机制
	done := make(chan error, 1)
	go func() {
		done <- h.cmd.Wait()
	}()
	
	select {
	case <-done:
		// 进程已经退出
	case <-time.After(3 * time.Second):
		// 超时，强制终止
		if h.cmd.Process != nil {
			if err := h.cmd.Process.Kill(); err != nil {
				return fmt.Errorf("强制终止播放器超时: %v", err)
			}
		}
	}
	
	h.isPlaying = false
	h.cmd = nil
	h.playingURL = ""
	log.Println("HDMI播放已停止")
	return nil
}

// IsPlaying 检查是否正在播放
func (h *HDMIPlayer) IsPlaying() bool {
	h.mutex.Lock()
	defer h.mutex.Unlock()
	return h.isPlaying
}

// GetPlayingURL 获取当前播放的URL
func (h *HDMIPlayer) GetPlayingURL() string {
	h.mutex.Lock()
	defer h.mutex.Unlock()
	return h.playingURL
}

// GetLastError 获取最后一个错误
func (h *HDMIPlayer) GetLastError() error {
	h.mutex.Lock()
	defer h.mutex.Unlock()
	return h.lastError
}

// 注册HDMI播放相关的API处理函数
func RegisterHDMIHandlers(hdmiPlayer *HDMIPlayer) {
	// 开始HDMI播放API
	http.HandleFunc("/api/hdmi/play", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "POST, GET, OPTIONS")
		
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusOK)
			return
		}
		
		if r.Method != http.MethodPost && r.Method != http.MethodGet {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}
		
		// 获取RTP地址
		rtpURL := r.URL.Query().Get("url")
		if rtpURL == "" {
			// 如果未指定，使用默认多播地址
			rtpURL = "udp://" + multicastAddr
		}
		
		// 启动播放
		err := hdmiPlayer.StartPlay(rtpURL)
		if err != nil {
			http.Error(w, fmt.Sprintf("启动HDMI播放失败: %v", err), http.StatusInternalServerError)
			return
		}
		
		json.NewEncoder(w).Encode(map[string]string{
			"status": "success",
			"message": fmt.Sprintf("正在通过HDMI播放 %s", rtpURL),
			"url": rtpURL,
		})
	})
	
	// 停止HDMI播放API
	http.HandleFunc("/api/hdmi/stop", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "POST, GET, OPTIONS")
		
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusOK)
			return
		}
		
		if r.Method != http.MethodPost && r.Method != http.MethodGet {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}
		
		// 停止播放
		err := hdmiPlayer.StopPlay()
		if err != nil {
			http.Error(w, fmt.Sprintf("停止HDMI播放失败: %v", err), http.StatusInternalServerError)
			return
		}
		
		json.NewEncoder(w).Encode(map[string]string{
			"status": "success",
			"message": "HDMI播放已停止",
		})
	})
	
	// 查询HDMI播放状态API
	http.HandleFunc("/api/hdmi/status", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Access-Control-Allow-Origin", "*")
		
		status := map[string]interface{}{
			"playing": hdmiPlayer.IsPlaying(),
		}
		
		if hdmiPlayer.IsPlaying() {
			status["url"] = hdmiPlayer.GetPlayingURL()
		}
		
		if err := hdmiPlayer.GetLastError(); err != nil {
			status["error"] = err.Error()
		}
		
		json.NewEncoder(w).Encode(status)
	})
}

// 添加HDMI控制页面
func addHDMIController(hdmiPlayer *HDMIPlayer, rtpServer *RTPServer) {
	http.HandleFunc("/hdmi-controller", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=UTF-8")
		html := `
        <!DOCTYPE html>
        <html>
        <head>
            <title>HDMI播放控制器</title>
            <style>
                body { font-family: Arial, sans-serif; margin: 20px; }
                .container { max-width: 800px; margin: 0 auto; }
                .form-group { margin-bottom: 15px; }
                label { display: block; margin-bottom: 5px; }
                input, button, select { padding: 8px; }
                button { cursor: pointer; margin-right: 10px; }
                .controls { margin-top: 20px; }
                .status { margin-top: 20px; padding: 10px; background-color: #f5f5f5; border-radius: 5px; }
            </style>
        </head>
        <body>
            <div class="container">
                <h1>树莓派HDMI播放控制</h1>
                
                <div class="form-group">
                    <label for="rtpURL">RTP流地址</label>
                    <input type="text" id="rtpURL" style="width: 70%;" placeholder="udp://224.30.30.226:8245">
                    <button id="useDefault">使用默认</button>
                </div>
                
                <div class="form-group">
                    <label>多播地址选择</label>
                    <select id="multicastSelect" style="width: 70%;">
                        <option value="">加载中...</option>
                    </select>
                    <button id="refreshMulticast">刷新</button>
                </div>
                
                <div class="controls">
                    <button id="playBtn" class="primary">开始播放</button>
                    <button id="stopBtn">停止播放</button>
                </div>
                
                <div class="status" id="status">
                    <h3>播放状态</h3>
                    <div id="statusContent">
                        正在检查...
                    </div>
                </div>
            </div>
            
            <script>
                const rtpURLInput = document.getElementById('rtpURL');
                const useDefaultBtn = document.getElementById('useDefault');
                const playBtn = document.getElementById('playBtn');
                const stopBtn = document.getElementById('stopBtn');
                const multicastSelect = document.getElementById('multicastSelect');
                const refreshMulticastBtn = document.getElementById('refreshMulticast');
                const statusContent = document.getElementById('statusContent');
                
                // 默认RTP地址
                const defaultRtpURL = 'rtp://224.30.30.226:8245';
                
                // 使用默认地址
                useDefaultBtn.addEventListener('click', function() {
                    rtpURLInput.value = defaultRtpURL;
                });
                
                // 加载多播地址列表
                async function loadMulticastAddresses() {
                    try {
                        const response = await fetch('/api/multicast/list');
                        if (response.ok) {
                            const data = await response.json();
                            multicastSelect.innerHTML = '';
                            
                            if (data.length === 0) {
                                const option = document.createElement('option');
                                option.value = '';
                                option.textContent = '无可用多播地址';
                                multicastSelect.appendChild(option);
                            } else {
                                data.forEach(item => {
                                    const option = document.createElement('option');
                                    option.value = 'rtp://' + item.address;
                                    option.textContent = item.address + (item.active ? ' (活跃)' : '');
                                    if (item.active) {
                                        option.selected = true;
                                    }
                                    multicastSelect.appendChild(option);
                                });
                            }
                        } else {
                            multicastSelect.innerHTML = '<option value="">无法加载多播地址</option>';
                        }
                    } catch (error) {
                        multicastSelect.innerHTML = '<option value="">加载失败: ' + error.message + '</option>';
                    }
                }
                
                // 刷新多播地址列表
                refreshMulticastBtn.addEventListener('click', loadMulticastAddresses);
                
                // 从多播下拉框选择地址
                multicastSelect.addEventListener('change', function() {
                    if (multicastSelect.value) {
                        rtpURLInput.value = multicastSelect.value;
                    }
                });
                
                // 开始播放
                playBtn.addEventListener('click', async function() {
                    const rtpURL = rtpURLInput.value.trim() || defaultRtpURL;
                    
                    try {
                        playBtn.disabled = true;
                        statusContent.textContent = '正在启动播放...';
                        
                        const response = await fetch('/api/hdmi/play?url=' + encodeURIComponent(rtpURL), {
                            method: 'POST'
                        });
                        
                        const result = await response.json();
                        
                        if (response.ok) {
                            statusContent.textContent = '播放已开始: ' + rtpURL;
                            updateStatus(); // 立即更新状态
                        } else {
                            statusContent.textContent = '启动播放失败: ' + (result.message || response.statusText);
                        }
                    } catch (error) {
                        statusContent.textContent = '请求错误: ' + error.message;
                    } finally {
                        playBtn.disabled = false;
                    }
                });
                
                // 停止播放
                stopBtn.addEventListener('click', async function() {
                    try {
                        stopBtn.disabled = true;
                        statusContent.textContent = '正在停止播放...';
                        
                        const response = await fetch('/api/hdmi/stop', {
                            method: 'POST'
                        });
                        
                        const result = await response.json();
                        
                        if (response.ok) {
                            statusContent.textContent = '播放已停止';
                            updateStatus(); // 立即更新状态
                        } else {
                            statusContent.textContent = '停止播放失败: ' + (result.message || response.statusText);
                        }
                    } catch (error) {
                        statusContent.textContent = '请求错误: ' + error.message;
                    } finally {
                        stopBtn.disabled = false;
                    }
                });
                
                // 定期更新状态
                async function updateStatus() {
                    try {
                        const response = await fetch('/api/hdmi/status');
                        if (response.ok) {
                            const status = await response.json();
                            
                            if (status.playing) {
                                statusContent.innerHTML = '<div style="color: green;">正在播放</div>' +
                                                        '<div>URL: ' + (status.url || '未知') + '</div>';
                                playBtn.disabled = true;
                                stopBtn.disabled = false;
                            } else {
                                statusContent.innerHTML = '<div>未播放</div>';
                                if (status.error) {
                                    statusContent.innerHTML += '<div style="color: red;">错误: ' + status.error + '</div>';
                                }
                                playBtn.disabled = false;
                                stopBtn.disabled = true;
                            }
                        } else {
                            statusContent.textContent = '无法获取状态: ' + response.statusText;
                        }
                    } catch (error) {
                        statusContent.textContent = '状态更新错误: ' + error.message;
                    }
                }
                
                // 页面加载时执行
                window.addEventListener('DOMContentLoaded', function() {
                    loadMulticastAddresses();
                    updateStatus();
                    
                    // 定时更新状态
                    setInterval(updateStatus, 5000);
                });
            </script>
        </body>
        </html>
        `
        w.Write([]byte(html))
    })
}

func main() {
	// 添加命令行参数
	httpPort := flag.Int("port", 8088, "HTTP服务器端口")
	videoDir := flag.String("videos", "./videos", "视频文件目录")
	
	// 添加HLS相关参数
	hlsDir := flag.String("hls-dir", "./hls", "HLS输出目录")
	hlsSegDuration := flag.Int("hls-segment", 4, "HLS段时长(秒)")
	hlsListSize := flag.Int("hls-list-size", 5, "HLS播放列表大小")
	ffmpegPath := flag.String("ffmpeg", "ffmpeg", "FFmpeg可执行文件路径")
	enableTranscode := flag.Bool("transcode", false, "启用视频转码(需更多CPU)")
	videoCodec := flag.String("vcodec", "", "视频编码(如h264,libx264)")
	audioCodec := flag.String("acodec", "", "音频编码(如aac,libfdk_aac)")
	
	// 添加HDMI播放器相关参数，默认修改为cvlc
	hdmiPlayerBin := flag.String("player", "cvlc", "媒体播放器可执行文件路径(默认cvlc)")
	
	flag.Parse()

	// 确保视频目录存在
	if err := os.MkdirAll(*videoDir, 0755); err != nil {
		log.Fatal("Failed to create video directory:", err)
	}
	
	// 确保HLS目录存在
	if err := os.MkdirAll(*hlsDir, 0755); err != nil {
		log.Fatal("Failed to create HLS directory:", err)
	}

	// 配置HLS管理器
	hlsConfig := HLSConfig{
		SegmentDuration:  *hlsSegDuration,
		PlaylistSize:     *hlsListSize,
		OutputDir:        *hlsDir,
		FFmpegBin:        *ffmpegPath,
		EnableTranscoding: *enableTranscode,
		VideoCodec:       *videoCodec,
		AudioCodec:       *audioCodec,
	}
	
	hlsManager := NewHLSManager(hlsConfig)
	
	// 注册HLS相关的HTTP处理函数
	RegisterHLSHandlers(hlsManager)
	addHLSPlayer(hlsManager)

	// 创建HDMI播放器
	hdmiPlayer := NewHDMIPlayer(*hdmiPlayerBin)
	
	// 启动RTP服务器
	rtpServer, err := NewRTPServer()
	if err != nil {
		log.Fatalf("无法创建RTP服务器: %v", err)
	}
	
	// 注册RTP处理函数
	registerRTPHandlers(rtpServer)
	
	// 注册HDMI播放相关的处理函数
	RegisterHDMIHandlers(hdmiPlayer)
	addHDMIController(hdmiPlayer, rtpServer)
	
	// 启动RTP服务器
	go rtpServer.Start()

	// 处理视频列表请求
	http.HandleFunc("/api/playlist", func(w http.ResponseWriter, r *http.Request) {
		videos := getVideoList(*videoDir, "")
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Access-Control-Allow-Origin", "*")
		json.NewEncoder(w).Encode(videos)
	})

	// 处理视频文件请求
	http.HandleFunc("/video/", func(w http.ResponseWriter, r *http.Request) {
		// CORS 头
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type")

		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusOK)
			return
		}

		relativePath := strings.TrimPrefix(r.URL.Path, "/video/")
		filePath := filepath.Join(*videoDir, relativePath)

		// 安全检查：确保文件路径在videoDir目录下
		if (!strings.HasPrefix(filepath.Clean(filePath), filepath.Clean(*videoDir))) {
			http.Error(w, "Forbidden", http.StatusForbidden)
			return
		}

		ext := strings.ToLower(filepath.Ext(relativePath))
		if ext == ".mkv" {
			w.Header().Set("Content-Type", "video/mkv")
		} else {
			w.Header().Set("Content-Type", "video/mp4")
		}
		http.ServeFile(w, r, filePath)
	})

	// 处理上传视频请求
	http.HandleFunc("/api/upload", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "POST, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "*")

		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusOK)
			return
		}

		if r.Method != http.MethodPost {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}

		// 设置2GB的上传限制
		if err := r.ParseMultipartForm(2 << 30); err != nil {
			http.Error(w, fmt.Sprintf("Error parsing form: %v", err), http.StatusBadRequest)
			return
		}

		file, handler, err := r.FormFile("video")
		if err != nil {
			http.Error(w, fmt.Sprintf("Error retrieving file: %v", err), http.StatusBadRequest)
			return
		}
		defer file.Close()

		// 清理和验证文件名
		safeName := filepath.Clean(filepath.Base(handler.Filename))
		ext := strings.ToLower(filepath.Ext(safeName))
		if ext != ".mp4" && ext != ".webm" && ext != ".mkv" {
			http.Error(w, "Invalid file type", http.StatusBadRequest)
			return
		}

		// 如果文件已存在，添加时间戳
		dstPath := filepath.Join(*videoDir, safeName)
		if _, err := os.Stat(dstPath); err == nil {
			nameWithoutExt := strings.TrimSuffix(safeName, ext)
			safeName = fmt.Sprintf("%s_%d%s", nameWithoutExt, time.Now().Unix(), ext)
			dstPath = filepath.Join(*videoDir, safeName)
		}

		dst, err := os.OpenFile(dstPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0644)
		if err != nil {
			http.Error(w, fmt.Sprintf("Error creating file: %v", err), http.StatusInternalServerError)
			return
		}
		defer dst.Close()

		if _, err := io.Copy(dst, file); err != nil {
			os.Remove(dstPath) // 清理失败的文件
			http.Error(w, fmt.Sprintf("Error saving file: %v", err), http.StatusInternalServerError)
			return
		}

		w.WriteHeader(http.StatusCreated)
		w.Write([]byte("Upload successful"))
	})

	// 添加获取客户端IP的API端点
	http.HandleFunc("/api/client-ip", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Access-Control-Allow-Origin", "*")

		// 获取客户端IP
		clientIP := getClientIP(r)

		response := map[string]string{"ip": clientIP}
		json.NewEncoder(w).Encode(response)
	})

	// 尝试启动HTTP服务器，如果端口被占用，尝试其他端口
	serverAddr := fmt.Sprintf(":%d", *httpPort)
	log.Printf("尝试在端口 %d 上启动HTTP服务器", *httpPort)

	server := &http.Server{Addr: serverAddr}
	if err := server.ListenAndServe(); err != nil {
		if strings.Contains(err.Error(), "address already in use") {
			// 尝试其他端口
			for port := *httpPort + 1; port < *httpPort+10; port++ {
				serverAddr = fmt.Sprintf(":%d", port)
				log.Printf("端口 %d 被占用，尝试端口 %d", *httpPort, port)
				server = &http.Server{Addr: serverAddr}
				if err := server.ListenAndServe(); err == nil {
					break
				}
				if !strings.Contains(err.Error(), "address already in use") {
					log.Fatal("HTTP服务器启动失败:", err)
				}
			}
		} else {
			log.Fatal("HTTP服务器启动失败:", err)
		}
	}
}

const (
	// RTP服务监听地址
	rtpListenAddr = ":8554"
	// 每个数据包的最大大小
	packetSize = 2048
	// 多播地址和端口
	multicastAddr = "224.30.30.226:8245"
)

// RTPClient 表示接收RTP流的客户端连接
type RTPClient struct {
	conn     *net.UDPConn
	addr     *net.UDPAddr
	lastSeen int64
}

// RTPServer 管理RTP流和客户端
type RTPServer struct {
	listenConn          *net.UDPConn
	clients             map[string]*RTPClient
	mutex               sync.RWMutex
	sourceAddr          *net.UDPAddr
	isRunning           bool
	multicastConns      map[string]*net.UDPConn // 管理多个多播连接
	multicastMutex      sync.RWMutex
	activeMulticastAddr string // 当前活跃的多播地址
	clientsPerMulticast map[string]int // 每个多播地址的客户端数量
}

// NewRTPServer 创建新的RTP服务器
func NewRTPServer() (*RTPServer, error) {
	// 解析监听地址
	addr, err := net.ResolveUDPAddr("udp", rtpListenAddr)
	if (err != nil) {
		return nil, fmt.Errorf("无法解析UDP地址: %v", err)
	}

	// 创建监听连接
	conn, err := net.ListenUDP("udp", addr)
	if (err != nil) {
		return nil, fmt.Errorf("无法监听UDP: %v", err)
	}

	return &RTPServer{
		listenConn:          conn,
		clients:             make(map[string]*RTPClient),
		mutex:               sync.RWMutex{},
		multicastConns:      make(map[string]*net.UDPConn),
		clientsPerMulticast: make(map[string]int),
	}, nil
}

// Start 开始监听和处理RTP数据包
func (s *RTPServer) Start() {
	s.isRunning = true

	// 创建停止信号处理
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		<-sigChan
		s.Stop()
	}()

	log.Printf("RTP服务器开始在 %s 上监听", rtpListenAddr)

	// 处理接收的RTP数据包
	buffer := make([]byte, packetSize)
	for s.isRunning {
		n, addr, err := s.listenConn.ReadFromUDP(buffer)
		if err != nil {
			if s.isRunning {
				log.Printf("读取UDP数据包错误: %v", err)
			}
			continue
		}

		// 如果是从新的源地址接收的，记录下来
		if (s.sourceAddr == nil || addr.String() != s.sourceAddr.String()) && s.isRunning {
			s.sourceAddr = addr
			log.Printf("从新源接收RTP流: %s", addr.String())
		}

		// 转发数据包给所有已连接的客户端
		s.forwardPacket(buffer[:n])
	}
}

// JoinMulticastGroup 加入指定的多播组并开始接收数据
func (s *RTPServer) JoinMulticastGroup(multicastAddrStr string) error {
	s.multicastMutex.Lock()
	defer s.multicastMutex.Unlock()

	// 检查是否已经连接到此多播地址
	if _, exists := s.multicastConns[multicastAddrStr]; exists {
		// 如果已连接，只设置为活跃
		if s.activeMulticastAddr != multicastAddrStr {
			s.activeMulticastAddr = multicastAddrStr
			log.Printf("已切换到多播地址: %s", multicastAddrStr)
		}
		return nil
	}

	// 如果没有客户端请求此多播地址，暂时不加入
	// 除非这是明确的加入请求（通过API调用）
	if s.clientsPerMulticast[multicastAddrStr] == 0 && len(s.clients) == 0 {
		// 记录为活跃，但实际上不连接，直到有客户端
		s.activeMulticastAddr = multicastAddrStr
		log.Printf("已设置多播地址为活跃（等待客户端连接）: %s", multicastAddrStr)
		return nil
	}

	// 解析多播地址
	mcAddr, err := net.ResolveUDPAddr("udp", multicastAddrStr)
	if err != nil {
		return fmt.Errorf("无法解析多播地址: %v", err)
	}

	// 尝试连接到多播组
	conn, err := net.ListenMulticastUDP("udp4", nil, mcAddr)
	if err != nil {
		// 如果第一次尝试失败，使用明确的接口尝试
		success := false
		interfaces, errIface := net.Interfaces()
		if errIface == nil {
			for _, iface := range interfaces {
				// 跳过不支持多播、未激活或环回接口
				if iface.Flags&net.FlagMulticast == 0 ||
					iface.Flags&net.FlagUp == 0 ||
					iface.Flags&net.FlagLoopback != 0 {
					continue
				}

				conn, err = net.ListenMulticastUDP("udp4", &iface, mcAddr)
				if err != nil {
					continue
				}

				success = true
				log.Printf("在接口 %s 上成功加入多播组 %s", iface.Name, multicastAddrStr)
				break
			}
		}

		if !success {
			return fmt.Errorf("无法连接到多播组: %v", err)
		}
	}

	// 设置接收缓冲区大小
	if err := conn.SetReadBuffer(1024 * 1024); err != nil {
		log.Printf("无法设置接收缓冲区大小: %v", err)
	}

	// 保存连接并设置为活跃
	s.multicastConns[multicastAddrStr] = conn
	s.activeMulticastAddr = multicastAddrStr

	log.Printf("成功加入多播组 %v", multicastAddrStr)

	// 启动接收协程
	go s.receiveMulticast(multicastAddrStr, conn)

	return nil
}

// LeaveMulticastGroup 离开指定的多播组
func (s *RTPServer) LeaveMulticastGroup(multicastAddrStr string) error {
	s.multicastMutex.Lock()
	defer s.multicastMutex.Unlock()

	conn, exists := s.multicastConns[multicastAddrStr]
	if !exists {  // Fixed: removed unnecessary parentheses
		return fmt.Errorf("未连接到多播组 %s", multicastAddrStr)
	}

	conn.Close()
	delete(s.multicastConns, multicastAddrStr)
	log.Printf("已离开多播组 %s", multicastAddrStr)

	// 如果离开的是活跃多播地址，切换到其他地址（如果有）
	if s.activeMulticastAddr == multicastAddrStr {
		s.activeMulticastAddr = ""
		for addr := range s.multicastConns {
			s.activeMulticastAddr = addr
			log.Printf("已切换到多播地址: %s", addr)
			break
		}
	}

	return nil
}

// GetActiveMulticastAddr 获取当前活跃的多播地址
func (s *RTPServer) GetActiveMulticastAddr() string {
	s.multicastMutex.RLock()
	defer s.multicastMutex.RUnlock()
	return s.activeMulticastAddr
}

// ListMulticastAddrs 列出所有已连接的多播地址
func (s *RTPServer) ListMulticastAddrs() []string {
	s.multicastMutex.RLock()
	defer s.multicastMutex.RUnlock()

	addrs := make([]string, 0, len(s.multicastConns))
	for addr := range s.multicastConns {
		addrs = append(addrs, addr)
	}
	return addrs
}

// receiveMulticast 接收多播数据并转发给客户端
func (s *RTPServer) receiveMulticast(multicastAddr string, conn *net.UDPConn) {
	buffer := make([]byte, packetSize)
	log.Printf("开始从多播地址 %s 接收数据", multicastAddr)

	for s.isRunning {
		n, _, err := conn.ReadFromUDP(buffer)
		if err != nil {
			// 检查连接是否已关闭
			s.multicastMutex.RLock()
			_, stillExists := s.multicastConns[multicastAddr]
			s.multicastMutex.RUnlock()

			if s.isRunning && stillExists {
				log.Printf("从多播组 %s 读取数据失败: %v", multicastAddr, err)
			} else {
				// 连接已关闭，退出协程
				log.Printf("多播地址 %s 的接收已停止", multicastAddr)
				return
			}
			continue
		}

		// 只有从当前活跃的多播地址接收的数据才转发给客户端
		s.multicastMutex.RLock()
		isActive := (multicastAddr == s.activeMulticastAddr)
		s.multicastMutex.RUnlock()

		if isActive {
			s.forwardPacket(buffer[:n])
		}
	}
}

// forwardPacket 将RTP数据包转发给所有客户端
func (s *RTPServer) forwardPacket(packet []byte) {
	s.mutex.RLock()
	defer s.mutex.RUnlock()

	for key, client := range s.clients {
		_, err := client.conn.Write(packet)
		if err != nil {
			log.Printf("转发数据包到客户端 %s 失败: %v", key, err)
			// 客户端可能已断开，后台执行删除操作
			go s.UnregisterClient(client.addr)
		}
	}
}

// RegisterClient 注册新的RTP客户端
func (s *RTPServer) RegisterClient(addr *net.UDPAddr) {
	s.mutex.Lock()
	defer s.mutex.Unlock()

	key := addr.String()
	if _, exists := s.clients[key]; !exists {
		conn, err := net.DialUDP("udp", nil, addr)
		if err != nil {
			log.Printf("无法连接到客户端 %s: %v", key, err)
			return
		}

		s.clients[key] = &RTPClient{
			conn:     conn,
			addr:     addr,
			lastSeen: time.Now().Unix(),
		}
		log.Printf("新客户端已连接: %s", key)
		
		// 检查是否需要启动多播接收
		s.multicastMutex.Lock()
		defer s.multicastMutex.Unlock()
		
		activeAddr := s.activeMulticastAddr
		if activeAddr != "" {
			s.clientsPerMulticast[activeAddr] = s.clientsPerMulticast[activeAddr] + 1
			
			// 先检查连接是否存在
			_, exists := s.multicastConns[activeAddr]
			// 如果这是第一个客户端且连接不存在，启动多播接收
			if s.clientsPerMulticast[activeAddr] == 1 && !exists {
				// 不等待错误处理，避免阻塞注册过程
				go func(addr string) {
					if err := s.joinMulticastGroupInternal(addr); err != nil {
						log.Printf("为客户端启动多播接收失败 %s: %v", addr, err)
					}
				}(activeAddr)
			}
		}
	} else {
		// 更新最后一次活动时间
		s.clients[key].lastSeen = time.Now().Unix()
		log.Printf("客户端已重新连接: %s", key)
	}
}

// UnregisterClient 注销RTP客户端
func (s *RTPServer) UnregisterClient(addr *net.UDPAddr) {
	s.mutex.Lock()
	defer s.mutex.Unlock()

	key := addr.String()
	if client, exists := s.clients[key]; exists {
		client.conn.Close()
		delete(s.clients, key)
		log.Printf("客户端断开连接: %s", key)
		
		// 更新多播地址的客户端计数
		s.multicastMutex.Lock()
		defer s.multicastMutex.Unlock()
		
		activeAddr := s.activeMulticastAddr
		if activeAddr != "" {
			s.clientsPerMulticast[activeAddr] = s.clientsPerMulticast[activeAddr] - 1
			
			// 如果没有客户端了，停止多播接收
			if s.clientsPerMulticast[activeAddr] <= 0 {
				s.clientsPerMulticast[activeAddr] = 0
				
				// 如果多播连接存在，关闭它
				if conn, exists := s.multicastConns[activeAddr]; exists {
					conn.Close()
					delete(s.multicastConns, activeAddr)
					log.Printf("无客户端连接，已停止多播地址 %s 的接收", activeAddr)
				}
			}
		}
	}
}

// joinMulticastGroupInternal 内部方法，实际执行多播组加入
// 不包含互斥锁，调用者需负责锁定
func (s *RTPServer) joinMulticastGroupInternal(multicastAddrStr string) error {
	// 解析多播地址
	mcAddr, err := net.ResolveUDPAddr("udp", multicastAddrStr)
	if err != nil {
		return fmt.Errorf("无法解析多播地址: %v", err)
	}

	// 尝试连接到多播组
	conn, err := net.ListenMulticastUDP("udp4", nil, mcAddr)
	if err != nil {
		// 如果第一次尝试失败，使用明确的接口尝试
		success := false
		interfaces, errIface := net.Interfaces()
		if errIface == nil {
			for _, iface := range interfaces {
				// 跳过不支持多播、未激活或环回接口
				if iface.Flags&net.FlagMulticast == 0 ||
					iface.Flags&net.FlagUp == 0 ||
					iface.Flags&net.FlagLoopback != 0 {
					continue
				}

				conn, err = net.ListenMulticastUDP("udp4", &iface, mcAddr)
				if err != nil {
					continue
				}

				success = true
				log.Printf("在接口 %s 上成功加入多播组 %s", iface.Name, multicastAddrStr)
				break
			}
		}

		if !success {
			return fmt.Errorf("无法连接到多播组: %v", err)
		}
	}

	// 设置接收缓冲区大小
	if err := conn.SetReadBuffer(1024 * 1024); err != nil {
		log.Printf("无法设置接收缓冲区大小: %v", err)
	}

	// 保存连接
	s.multicastConns[multicastAddrStr] = conn
	log.Printf("成功加入多播组 %v", multicastAddrStr)

	// 启动接收协程
	go s.receiveMulticast(multicastAddrStr, conn)

	return nil
}

// Stop 停止RTP服务器
func (s *RTPServer) Stop() {
	if !s.isRunning {
		return
	}

	s.isRunning = false
	s.listenConn.Close()

	// 关闭所有多播连接
	s.multicastMutex.Lock()
	for addr, conn := range s.multicastConns {
		conn.Close()
		delete(s.multicastConns, addr)
	}
	s.multicastMutex.Unlock()
	log.Println("已关闭所有多播连接")

	s.mutex.Lock()
	defer s.mutex.Unlock()

	// 关闭所有客户端连接
	for key, client := range s.clients {
		client.conn.Close()
		delete(s.clients, key)
	}

	log.Println("RTP服务器已停止")
}

// 客户端注册HTTP处理函数
func registerRTPHandlers(server *RTPServer) {
	// 现有处理函数
	http.HandleFunc("/api/rtp/connect", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")

		clientIP := r.URL.Query().Get("ip")
		clientPort := r.URL.Query().Get("port")

		if clientIP == "" || clientPort == "" {
			http.Error(w, "缺少IP或端口参数", http.StatusBadRequest)
			return
		}

		clientAddr, err := net.ResolveUDPAddr("udp", clientIP+":"+clientPort)
		if err != nil {
			http.Error(w, "无法解析客户端地址", http.StatusBadRequest)
			return
		}

		server.RegisterClient(clientAddr)

		w.WriteHeader(http.StatusOK)
		w.Write([]byte("RTP客户端已连接"))
	})

	http.HandleFunc("/api/rtp/disconnect", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")

		clientIP := r.URL.Query().Get("ip")
		clientPort := r.URL.Query().Get("port")

		if clientIP == "" || clientPort == "" {
			http.Error(w, "缺少IP或端口参数", http.StatusBadRequest)
			return
		}

		clientAddr, err := net.ResolveUDPAddr("udp", clientIP+":"+clientPort)
		if err != nil {
			http.Error(w, "无法解析客户端地址", http.StatusBadRequest)
			return
		}

		server.UnregisterClient(clientAddr)

		w.WriteHeader(http.StatusOK)
		w.Write([]byte("RTP客户端已断开"))
	})

	// 新增的多播地址管理API
	http.HandleFunc("/api/multicast/list", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Access-Control-Allow-Origin", "*")

		addrs := server.ListMulticastAddrs()
		activeAddr := server.GetActiveMulticastAddr()

		type MulticastInfo struct {
			Address string `json:"address"`
			Active  bool   `json:"active"`
		}

		result := make([]MulticastInfo, 0, len(addrs))
		for _, addr := range addrs {
			result = append(result, MulticastInfo{
				Address: addr,
				Active:  addr == activeAddr,
			})
		}

		json.NewEncoder(w).Encode(result)
	})

	http.HandleFunc("/api/multicast/join", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Access-Control-Allow-Origin", "*")

		if r.Method != http.MethodPost && r.Method != http.MethodGet {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}

		addr := r.URL.Query().Get("addr")
		if addr == "" {
			http.Error(w, "Missing multicast address parameter", http.StatusBadRequest)
			return
		}

		err := server.JoinMulticastGroup(addr)
		if err != nil {
			http.Error(w, fmt.Sprintf("Failed to join multicast group: %v", err), http.StatusInternalServerError)
			return
		}

		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(map[string]string{"status": "success", "message": "Joined multicast group " + addr})
	})

	http.HandleFunc("/api/multicast/leave", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Access-Control-Allow-Origin", "*")

		if r.Method != http.MethodPost && r.Method != http.MethodGet {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}

		addr := r.URL.Query().Get("addr")
		if addr == "" {
			http.Error(w, "Missing multicast address parameter", http.StatusBadRequest)
			return
		}

		err := server.LeaveMulticastGroup(addr)
		if err != nil {
			http.Error(w, fmt.Sprintf("Failed to leave multicast group: %v", err), http.StatusInternalServerError)
			return
		}

		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(map[string]string{"status": "success", "message": "Left multicast group " + addr})
	})

	http.HandleFunc("/api/multicast/active", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Access-Control-Allow-Origin", "*")

		if r.Method == http.MethodGet {
			activeAddr := server.GetActiveMulticastAddr()
			json.NewEncoder(w).Encode(map[string]string{"active_multicast": activeAddr})
			return
		}

		if r.Method == http.MethodPost {
			addr := r.URL.Query().Get("addr")
			if addr == "" {
				http.Error(w, "Missing multicast address parameter", http.StatusBadRequest)
				return
			}

			// 先检查是否已连接，如果没有则先连接
			server.multicastMutex.RLock()
			_, exists := server.multicastConns[addr]
			server.multicastMutex.RUnlock()

			if !exists {
				if err := server.JoinMulticastGroup(addr); err != nil {
					http.Error(w, fmt.Sprintf("Failed to join multicast group: %v", err), http.StatusInternalServerError)
					return
				}
			} else {
				// 如果已存在，只需要设置为活跃
				server.multicastMutex.Lock()
				server.activeMulticastAddr = addr
				server.multicastMutex.Unlock()
				log.Printf("已切换到多播地址: %s", addr)
			}

			w.WriteHeader(http.StatusOK)
			json.NewEncoder(w).Encode(map[string]string{"status": "success", "active_multicast": addr})
			return
		}

		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
	})
}

// 在主函数中使用
func startRTPServer() {
	rtpServer, err := NewRTPServer()
	if err != nil {
		log.Fatalf("无法创建RTP服务器: %v", err)
	}

	// 注册HTTP处理函数
	registerRTPHandlers(rtpServer)

	// 设置默认多播地址为活跃，但不立即连接
	rtpServer.multicastMutex.Lock()
	rtpServer.activeMulticastAddr = multicastAddr
	rtpServer.multicastMutex.Unlock()
	log.Printf("已设置默认多播地址: %s (将在有客户端连接时启动接收)", multicastAddr)

	// 启动RTP服务器
	go rtpServer.Start()
}

// 获取客户端IP的函数
func getClientIP(r *http.Request) string {
	// 先尝试从X-Real-IP和X-Forwarded-For头获取客户端IP
	// 这些通常由代理或负载均衡器设置
	ip := r.Header.Get("X-Real-IP")
	if ip == "" {
		ip = r.Header.Get("X-Forwarded-For")
		if ip != "" {
			// X-Forwarded-For可能包含多个IP，取第一个
			parts := strings.Split(ip, ",")
			ip = strings.TrimSpace(parts[0])
		}
	}

	// 如果没有从头部获取到，则使用RemoteAddr
	if ip == "" {
		ip = r.RemoteAddr
		// RemoteAddr包含端口号，需要分离
		host, _, err := net.SplitHostPort(ip)
		if err == nil {
			ip = host
		}
	}

	return ip
}
