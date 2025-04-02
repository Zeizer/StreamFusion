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
	"bufio"
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
		// 确保先停止当前播放
		h.mutex.Unlock() // 临时释放锁以避免死锁
		h.StopPlay()
		h.mutex.Lock() // 重新获取锁
	}
	
	// 确保播放器完全关闭并等待一小段时间
	time.Sleep(500 * time.Millisecond)
	
	// 检查播放器是否存在
	_, err := exec.LookPath(h.playerBin)
	if err != nil {
		return fmt.Errorf("找不到播放器 %s: %v", h.playerBin, err)
	}
	
	// 验证流可访问性
	if err := checkStreamAvailability(rtpURL); err != nil {
		log.Printf("警告: 流可能不可用 %s: %v", rtpURL, err)
		// Continue anyway, but log the warning
	}
	
	// 构建播放命令
	var args []string
	var env []string = os.Environ()
	
	// 添加DISPLAY环境变量确保输出到正确显示器
	// env = append(env, "DISPLAY=:0.0")
	// 禁用VLC的X11视频嵌入
	// env = append(env, "VLC_PLUGIN_PATH=/dev/null")
	
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
		// 更新VLC参数，使用更稳定的配置
		args = []string{
			rtpURL,                // 先放URL确保被正确解析
			// "--fullscreen",        // 全屏显示
			// "--video-on-top",      // 视频置顶
			// "--no-video-title-show", // 不显示标题
			// "--no-embedded-video", // 不在终端嵌入视频
			// "--no-qt-error-dialogs", // 禁止错误对话框
			// "--vout=xcb_xv",       // 使用XCB视频输出模块
			// "--aout=alsa",         // 使用ALSA音频输出
			// "--no-keyboard-events", // 禁用键盘事件 
			// "--no-mouse-events",   // 禁用鼠标事件
			// "--network-caching=200", // 低缓存值用于降低延迟但增加稳定性
			// "--live-caching=200",  // 直播流缓存
			// "--file-caching=200",  // 文件缓存
			// "--no-video-deco",     // 无装饰
			// "--quiet",             // 减少错误信息
			// "--play-and-exit",     // 播放完退出
		}
	} else {
		// 通用参数，只传入URL
		args = []string{rtpURL}
	}
	
	// 创建命令
	cmd := exec.Command(h.playerBin, args...)
	cmd.Env = env // 设置环境变量
	
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
	log.Printf("启动HDMI播放: %s %v", h.playerBin, args)
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
				if err != io.EOF {
					log.Printf("读取播放器输出遇到错误: %v", err)
				}
				break
			}
		}
	}()
	
	go func() {
		buf := make([]byte, 4096)
		for {
			n, err := stderr.Read(buf)
			if n > 0 {
				errOutput := string(buf[:n])
				log.Printf("播放器错误: %s", errOutput)
				
				// 检查是否有常见错误指示流问题
				if strings.Contains(errOutput, "connection failed") || 
				   strings.Contains(errOutput, "no suitable demux") ||
				   strings.Contains(errOutput, "cannot connect") {
					log.Printf("检测到流连接问题，将尝试备用方法")
					// 尝试自动重启播放，采用备用方法
					go h.tryFallbackMethod(rtpURL)
				}
			}
			if err != nil {
				if err != io.EOF {
					log.Printf("读取播放器错误输出遇到错误: %v", err)
				}
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
			
			// 获取当前时间，如果进程运行时间非常短，可能是流问题
			if time.Since(time.Now()) < 3*time.Second {
				log.Printf("播放进程运行时间太短，可能存在流问题")
				// 可以在这里添加更多恢复逻辑
			}
		} else {
			log.Println("播放正常结束")
		}
	}()
	
	// 增加5秒后检查播放状态
	go func() {
		time.Sleep(5 * time.Second)
		
		h.mutex.Lock()
		defer h.mutex.Unlock()
		
		if h.isPlaying && h.cmd != nil {
			// 检查进程状态
			if h.cmd.ProcessState == nil || !h.cmd.ProcessState.Exited() {
				log.Printf("播放进程仍在运行，但验证其是否正常显示视频")
				// 这里可以添加更多的验证逻辑
			}
		}
	}()
	
	log.Printf("开始通过HDMI播放RTP流: %s", rtpURL)
	return nil
}

// tryFallbackMethod 尝试使用备用方法播放
func (h *HDMIPlayer) tryFallbackMethod(rtpURL string) {
	// 等待一段时间确保当前播放已停止
	time.Sleep(2 * time.Second)
	
	h.mutex.Lock()
	if h.isPlaying {
		// 如果仍在播放，不执行备用方法
		h.mutex.Unlock()
		return
	}
	h.mutex.Unlock()
	
	log.Printf("尝试使用备用方法播放: %s", rtpURL)
	
	// 使用不同的VLC参数尝试播放
	fallbackArgs := []string{
		"--fullscreen", 
		"--intf", "dummy",  // 使用无界面模式
		"--no-video-title-show",
		"--no-embedded-video",
		"--no-keyboard-events",
		"--no-mouse-events",
		rtpURL,
	}
	
	cmd := exec.Command(h.playerBin, fallbackArgs...)
	// 设置DISPLAY环境变量
	env := os.Environ()
	env = append(env, "DISPLAY=:0.0")
	cmd.Env = env
	
	// 启动备用播放
	log.Printf("启动备用播放方法: %s %v", h.playerBin, fallbackArgs)
	if err := cmd.Start(); err != nil {
		log.Printf("备用播放方法失败: %v", err)
		return
	}
	
	h.mutex.Lock()
	h.cmd = cmd
	h.isPlaying = true
	h.mutex.Unlock()
	
	// 监控备用播放进程
	go func() {
		err := cmd.Wait()
		h.mutex.Lock()
		defer h.mutex.Unlock()
		
		h.isPlaying = false
		h.cmd = nil
		
		if err != nil {
			log.Printf("备用播放方法停止，错误: %v", err)
		} else {
			log.Println("备用播放方法正常结束")
		}
	}()
}

// 检查RTP流是否可用
func checkStreamAvailability(rtpURL string) error {
	// 从URL中提取地址和端口
	urlParts := strings.Split(rtpURL, "://")
	if len(urlParts) != 2 {
		return fmt.Errorf("无效的URL格式")
	}
	
	addrParts := strings.Split(urlParts[1], ":")
	if len(addrParts) != 2 {
		return fmt.Errorf("无效的地址格式")
	}
	
	// 尝试解析UDP地址
	addr, err := net.ResolveUDPAddr("udp", urlParts[1])
	if err != nil {
		return fmt.Errorf("解析地址失败: %v", err)
	}
	
	// 检查多播地址可达性
	// 注意：这只是一个基本检查，不能保证流内容可播放
	if addr.IP.IsMulticast() {
		ifaces, err := net.Interfaces()
		if err != nil {
			return fmt.Errorf("获取网络接口失败: %v", err)
		}
		
		foundMulticastIface := false
		for _, iface := range ifaces {
			if iface.Flags&net.FlagMulticast != 0 && iface.Flags&net.FlagUp != 0 {
				foundMulticastIface = true
				break
			}
		}
		
		if !foundMulticastIface {
			return fmt.Errorf("没有可用的多播网络接口")
		}
		
		log.Printf("多播地址 %s 初步检查通过", addr.String())
		return nil
	}
	
	// 对于非多播地址进行简单连接测试
	conn, err := net.DialTimeout("udp", addr.String(), 2*time.Second)
	if err != nil {
		return fmt.Errorf("连接测试失败: %v", err)
	}
	defer conn.Close()
	
	return nil
}

// StopPlay 停止播放
func (h *HDMIPlayer) StopPlay() error {
	h.mutex.Lock()
	defer h.mutex.Unlock()
	
	if (!h.isPlaying || h.cmd == nil) {
		return nil // 已经停止了
	}
	
	log.Printf("停止HDMI播放: %s", h.playingURL)
	
	// 更彻底地关闭进程
	// 先尝试正常关闭
	if strings.HasSuffix(h.playerBin, "vlc") || strings.HasSuffix(h.playerBin, "cvlc") {
		// VLC可以通过SIGTERM信号正常退出
		if err := h.cmd.Process.Signal(syscall.SIGTERM); err != nil {
			log.Printf("发送SIGTERM到VLC失败: %v，尝试强制终止", err)
		} else {
			// 给VLC一点时间优雅退出
			time.Sleep(500 * time.Millisecond)
		}
	} else if h.playerBin == "omxplayer" {
		// omxplayer特定的停止逻辑
		if err := h.cmd.Process.Signal(syscall.SIGINT); err != nil {
			log.Printf("发送SIGINT失败: %v，尝试强制终止", err)
		} else {
			time.Sleep(500 * time.Millisecond)
		}
	}
	
	// 如果进程仍在运行，更强硬地终止
	if h.cmd.ProcessState == nil || !h.cmd.ProcessState.Exited() {
		// 先尝试SIGKILL
		if err := h.cmd.Process.Kill(); err != nil {
			log.Printf("无法终止播放进程: %v", err)
			// 即使出错也继续尝试清理
		}
		
		// 等待超时机制确保进程结束
		done := make(chan error, 1)
		go func() {
			done <- h.cmd.Wait()
		}()
		
		select {
		case <-done:
			// 进程已经退出
			log.Println("播放进程已终止")
		case <-time.After(2 * time.Second):
			log.Println("进程终止超时，强制清理")
		}
	}
	
	// 完全清理进程状态
	h.isPlaying = false
	h.cmd = nil
	h.playingURL = ""
	
	// 确保VLC不会在控制台留下任何残留的显示
	if strings.HasSuffix(h.playerBin, "vlc") || strings.HasSuffix(h.playerBin, "cvlc") {
		// 清理可能的遗留进程
		exec.Command("pkill", "-9", "vlc").Run()
		exec.Command("pkill", "-9", "cvlc").Run()
		
		// 在某些情况下，可能需要重置终端
		// 这通常在桌面环境不需要，但在纯控制台环境可能有用
		exec.Command("reset").Run()
	}
	
	log.Println("HDMI播放已完全停止")
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
			rtpURL = "rtp://" + multicastAddr
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

// Channel represents a TV channel from channels.json
type Channel struct {
	ChannelID   string `json:"ChannelID"`
	ChannelName string `json:"ChannelName"`
	ChannelURL  string `json:"ChannelURL"`
}

// loadChannels loads the channel list from the JSON file
func loadChannels(filePath string) ([]Channel, error) {
	data, err := ioutil.ReadFile(filePath)
	if err != nil {
		return nil, fmt.Errorf("failed to read channels file: %v", err)
	}
	
	var channels []Channel
	if err := json.Unmarshal(data, &channels); err != nil {
		return nil, fmt.Errorf("failed to parse channel data: %v", err)
	}
	
	return channels, nil
}

// TranscodeMKVToMP4 转码MKV文件为MP4文件
func TranscodeMKVToMP4(inputPath, outputPath string) error {
	ffmpegPath := "ffmpeg" // 假设ffmpeg在系统路径中
	args := []string{
		"-i", inputPath,
		"-c:v", "libx264", // 使用H.264编码
		"-c:a", "aac",     // 使用AAC音频编码
		"-strict", "experimental",
		"-y", // 覆盖输出文件
		outputPath,
	}

	cmd := exec.Command(ffmpegPath, args...)
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("转码失败: %v", err)
	}
	return nil
}

var channelsList []Channel
var currentChannelIndex int = 0
var channelMutex sync.Mutex

// IRRemoteListener starts a goroutine that listens for IR remote signals
// and handles channel switching when UP/DOWN keys are detected
func startIRRemoteListener(hdmiPlayer *HDMIPlayer) {
	log.Println("Starting IR remote control listener")
	
	// Make sure we have channels loaded
	if len(channelsList) == 0 {
		var err error
		channelsList, err = loadChannels("/home/pi/projects/my_stream_server/channels.json")
		if err != nil {
			log.Printf("Error loading channels for IR control: %v", err)
			return
		}
		log.Printf("Loaded %d channels for IR remote control", len(channelsList))
	}
	
	go func() {
		cmd := exec.Command("irw")
		stdout, err := cmd.StdoutPipe()
		if err != nil {
			log.Printf("Error creating irw pipe: %v", err)
			return
		}
		
		if err := cmd.Start(); err != nil {
			log.Printf("Error starting irw: %v", err)
			return
		}
		
		// Create a scanner to read irw output
		scanner := bufio.NewScanner(stdout)
		
		log.Println("IR remote listener started, waiting for key presses")
		
		for scanner.Scan() {
			line := scanner.Text()
			log.Printf("IR received: %s", line)
			
			// Check for UP/DOWN keys
			if strings.Contains(line, "KEY_UP") {
				log.Println("UP key detected, switching to next channel")
				stepChannel(hdmiPlayer, "up")
			} else if strings.Contains(line, "KEY_DOWN") {
				log.Println("DOWN key detected, switching to previous channel")
				stepChannel(hdmiPlayer, "down")
			}
		}
		
		if err := scanner.Err(); err != nil {
			log.Printf("Error reading from irw: %v", err)
		}
		
		log.Println("IR remote listener stopped")
		
		// If irw exits, wait for the process to complete
		if err := cmd.Wait(); err != nil {
			log.Printf("irw exited with error: %v", err)
		}
	}()
}

// stepChannel changes the TV channel in the specified direction
func stepChannel(hdmiPlayer *HDMIPlayer, direction string) {
	channelMutex.Lock()
	defer channelMutex.Unlock()
	
	if len(channelsList) == 0 {
		log.Println("No channels available for switching")
		return
	}
	
	// 记录之前的频道索引，以便失败时恢复
	previousIndex := currentChannelIndex
	
	// 先完全停止当前播放，确保前一个进程结束
	if err := hdmiPlayer.StopPlay(); err != nil {
		log.Printf("Error stopping current playback: %v", err)
		// 尝试强制清理VLC进程
		exec.Command("pkill", "-9", "vlc").Run()
		exec.Command("pkill", "-9", "cvlc").Run()
		// 给系统一点时间清理
		time.Sleep(1 * time.Second)
	}
	
	// Update the channel index based on direction
	if direction == "up" {
		currentChannelIndex = (currentChannelIndex + 1) % len(channelsList)
	} else if direction == "down" {
		currentChannelIndex = (currentChannelIndex - 1 + len(channelsList)) % len(channelsList)
	}
	
	// Get the selected channel
	channel := channelsList[currentChannelIndex]
	log.Printf("Switching to channel: %s (%s)", channel.ChannelName, channel.ChannelURL)
	
	// Add rtp:// prefix if not present
	rtpURL := channel.ChannelURL
	if !strings.HasPrefix(rtpURL, "rtp://") {
		rtpURL = "rtp://" + rtpURL
	}
	
	// 确保完全停止后再开始新播放
	time.Sleep(1 * time.Second)
	
	// Start playing the channel
	if err := hdmiPlayer.StartPlay(rtpURL); err != nil {
		log.Printf("Error playing channel: %v", err)
		log.Printf("Trying to continue with next channel")
		
		// 如果播放失败，尝试下一个频道
		if direction == "up" {
			currentChannelIndex = (currentChannelIndex + 1) % len(channelsList)
		} else if direction == "down" {
			currentChannelIndex = (currentChannelIndex - 1 + len(channelsList)) % len(channelsList)
		}
		
		// 第二次尝试
		nextChannel := channelsList[currentChannelIndex]
		log.Printf("Trying next channel: %s (%s)", nextChannel.ChannelName, nextChannel.ChannelURL)
		
		rtpURL = nextChannel.ChannelURL
		if !strings.HasPrefix(rtpURL, "rtp://") {
			rtpURL = "rtp://" + rtpURL
		}
		
		if err := hdmiPlayer.StartPlay(rtpURL); err != nil {
			log.Printf("Second attempt also failed: %v", err)
			// 恢复到之前的频道索引
			currentChannelIndex = previousIndex
		}
	}
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

	// Load channel list at startup
	var err error
	channelsList, err = loadChannels("/home/pi/projects/my_stream_server/channels.json")
	if err != nil {
		log.Printf("Error loading channels: %v", err)
	} else {
		log.Printf("Loaded %d channels", len(channelsList))
	}

	// Create HDMI player
	hdmiPlayer := NewHDMIPlayer(*hdmiPlayerBin)
	
	// Start IR remote listener
	startIRRemoteListener(hdmiPlayer)
	
	// 启动RTP服务器
	rtpServer, err := NewRTPServer()
	if err != nil {
		log.Fatalf("无法创建RTP服务器: %v", err)
	}
	
	// 注册RTP处理函数
	// registerRTPHandlers(rtpServer)
	
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
		if !strings.HasPrefix(filepath.Clean(filePath), filepath.Clean(*videoDir)) {
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

		// 设置10GB的上传限制，适合更大的电视剧文件
		if err := r.ParseMultipartForm(10 << 30); err != nil {
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

	// 处理频道列表请求
	http.HandleFunc("/api/channels", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Access-Control-Allow-Origin", "*")
		
		channelsFile := filepath.Join(".", "channels.json")
		channels, err := loadChannels(channelsFile)
		
		if err != nil {
			log.Printf("Error loading channels: %v", err)
			http.Error(w, "Failed to load channel list", http.StatusInternalServerError)
			return
		}
		
		json.NewEncoder(w).Encode(channels)
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



// 在主函数中使用
func startRTPServer() {
	rtpServer, err := NewRTPServer()
	if err != nil {
		log.Fatalf("无法创建RTP服务器: %v", err)
	}

	// 注册HTTP处理函数
	// registerRTPHandlers(rtpServer)

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
