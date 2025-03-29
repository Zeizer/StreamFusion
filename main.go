package main

import (
	"bufio"
	"fmt"
	"net"
	"os"
	"os/exec"
	"strings"
	"time"
	"log"
)

// 定义遥控器按键与功能的映射
var commandMap = map[string]func(){
	"KEY_UP":     func() { fmt.Println("上键被按下") },
	"KEY_DOWN":   func() { fmt.Println("下键被按下") },
	"KEY_LEFT":   func() { fmt.Println("左键被按下") },
	"KEY_RIGHT":  func() { fmt.Println("右键被按下") },
	"KEY_OK":     func() { fmt.Println("确定键被按下") },
	"KEY_MENU":   func() { fmt.Println("菜单键被按下") },
	"KEY_BACK":   func() { fmt.Println("返回键被按下") },
	"KEY_POWER":  func() { fmt.Println("电源键被按下") },
	"KEY_VOLUME_UP": func() { changeVolume("+5%") },
	"KEY_VOLUME_DOWN": func() { changeVolume("-5%") },
}

// 修改系统音量
func changeVolume(change string) {
	cmd := exec.Command("amixer", "set", "Master", change)
	err := cmd.Run()
	if err != nil {
		fmt.Println("调整音量失败:", err)
	} else {
		fmt.Println("音量已调整:", change)
	}
}

// 连接LIRC守护进程以接收遥控器信号
func connectToLIRC() (net.Conn, error) {
	// 检查LIRC socket文件是否存在
	socketPaths := []string{
		"/var/run/lirc/lircd", 
		"/run/lirc/lircd",
		"/dev/lircd",
		"/var/run/lirc/lircd.socket",
	}
	
	var conn net.Conn
	var err error
	
	for _, path := range socketPaths {
		fmt.Printf("尝试连接LIRC socket: %s\n", path)
		if _, statErr := os.Stat(path); statErr == nil {
			conn, err = net.Dial("unix", path)
			if err == nil {
				fmt.Printf("成功连接到LIRC socket: %s\n", path)
				return conn, nil
			}
			fmt.Printf("连接到 %s 失败: %v\n", path, err)
		} else {
			fmt.Printf("Socket路径不存在: %s\n", path)
		}
	}
	
	return nil, fmt.Errorf("无法连接到任何LIRC socket: %v", err)
}

// 添加直接处理IR信号的函数
func processDirectIRSignals() {
	fmt.Println("开始直接处理IR信号...")

	// 创建一个通道用于定期检查IR信号
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			cmd := exec.Command("ir-keytable", "-t")
			out, err := cmd.CombinedOutput()
			if err == nil && len(out) > 0 {
				fmt.Printf("检测到键盘事件: %s\n", string(out))
				
				// 尝试从输出中提取按键信息
				lines := strings.Split(string(out), "\n")
				for _, line := range lines {
					if strings.Contains(line, "KEY_") {
						parts := strings.Fields(line)
						for _, part := range parts {
							if strings.HasPrefix(part, "KEY_") {
								fmt.Printf("检测到按键: %s\n", part)
								if handler, ok := commandMap[part]; ok {
									handler()
								} else {
									fmt.Printf("未映射的按键: %s\n", part)
								}
							}
						}
					}
				}
			}
		}
	}
}

// 处理遥控器信号 - 使用更可靠的方式
func processRemoteSignals(conn net.Conn) {
	fmt.Println("开始处理遥控器信号...")
	
	// 启动直接IR信号处理
	go processDirectIRSignals()
	
	// 定期运行mode2捕捉原始IR信号
	go func() {
		for {
			cmd := exec.Command("timeout", "3", "mode2", "-d", "/dev/lirc0")
			var outBuf strings.Builder
			cmd.Stdout = &outBuf
			
			err := cmd.Start()
			if err == nil {
				go func() {
					time.Sleep(1 * time.Second)
					fmt.Println("请按下遥控器按钮...")
				}()
				
				err = cmd.Wait()
				output := outBuf.String()
				
				if len(output) > 0 && strings.Contains(output, "pulse") {
					fmt.Println("检测到IR脉冲，按钮被按下!")
					// 触发一个通用按钮按下事件
					fmt.Println("触发通用按钮事件")
				}
			}
			time.Sleep(5 * time.Second)
		}
	}()
	
	// 使用evtest监听输入事件
	go func() {
		for {
			fmt.Println("使用evtest监听输入事件...")
			cmd := exec.Command("timeout", "10", "evtest", "--query", "/dev/input/event0")
			output, _ := cmd.CombinedOutput()
			if len(output) > 0 {
				fmt.Printf("检测到输入事件: %s\n", string(output))
			}
			time.Sleep(5 * time.Second)
		}
	}()
	
	// 使用irw命令直接监听，但处理输出
	go func() {
		for {
			fmt.Println("启动irw监听...")
			cmd := exec.Command("irw")
			
			stdout, err := cmd.StdoutPipe()
			if err != nil {
				fmt.Printf("无法获取irw输出: %v\n", err)
				time.Sleep(5 * time.Second)
				continue
			}
			
			if err := cmd.Start(); err != nil {
				fmt.Printf("启动irw失败: %v\n", err)
				time.Sleep(5 * time.Second)
				continue
			}
			
			scanner := bufio.NewScanner(stdout)
			irwActive := true
			
			go func() {
				// 20秒后终止irw
				time.Sleep(20 * time.Second)
				if irwActive {
					cmd.Process.Kill()
					irwActive = false
				}
			}()
			
			for scanner.Scan() {
				line := scanner.Text()
				fmt.Printf("irw接收到: %s\n", line)
				
				// 处理irw输出行
				parts := strings.Fields(line)
				if len(parts) >= 3 {
					buttonName := parts[1]
					if handler, ok := commandMap[buttonName]; ok {
						handler()
					} else {
						fmt.Printf("未映射的按键: %s\n", buttonName)
					}
				}
			}
			
			irwActive = false
			cmd.Wait()
			time.Sleep(10 * time.Second)
		}
	}()
	
	// 原始socket监听
	scanner := bufio.NewScanner(conn)
	fmt.Println("等待LIRC socket信号...")
	
	for scanner.Scan() {
		line := scanner.Text()
		fmt.Printf("从socket接收到信号: %s\n", line)
		
		// LIRC通常会发送类似这样的数据: <code> <repeat count> <button name> <remote name>
		parts := strings.Fields(line)
		
		// 检查是否是按键信号
		if len(parts) >= 4 && parts[0] != "BEGIN" && parts[0] != "END" && 
		   parts[0] != "ERROR" && parts[0] != "DATA" {
			
			code := parts[0]
			repeatCount := parts[1]
			buttonName := parts[2]
			remoteName := parts[3]
			
			fmt.Printf("解析信号: 代码=%s, 重复次数=%s, 按键=%s, 遥控器=%s\n", 
					   code, repeatCount, buttonName, remoteName)
			
			// 只处理一次按键，忽略重复信号
			if repeatCount == "00" {
				if handler, ok := commandMap[buttonName]; ok {
					handler()
				} else {
					fmt.Printf("接收到未映射的按键: %s (来自遥控器: %s)\n", buttonName, remoteName)
				}
			}
		} else if strings.Contains(line, "error") || strings.Contains(line, "ERROR") {
			fmt.Printf("LIRC错误: %s\n", line)
		}
	}
	
	if err := scanner.Err(); err != nil {
		fmt.Println("读取LIRC数据出错:", err)
	}
	
	fmt.Println("信号处理循环结束")
}

// 检查遥控器是否工作
func checkRemoteWorking() {
	fmt.Println("检查遥控器是否工作...")
	
	// 检查mode2能否直接接收IR信号
	cmd := exec.Command("timeout", "5", "mode2", "-d", "/dev/lirc0")
	fmt.Println("开始检测IR信号，请按下遥控器按钮...")
	output, err := cmd.CombinedOutput()
	
	if err != nil && !strings.Contains(err.Error(), "exit status") {
		fmt.Printf("mode2测试失败: %v\n", err)
		fmt.Println("请确保IR接收器连接正确，且您的遥控器能发出IR信号")
	} else if len(output) > 0 {
		fmt.Println("检测到IR原始信号，接收器工作正常")
		fmt.Printf("原始信号样本: %s\n", string(output)[:min(100, len(output))])
	} else {
		fmt.Println("未检测到IR信号，请检查:")
		fmt.Println("1. 遥控器电池是否正常")
		fmt.Println("2. 遥控器是否正对IR接收器")
		fmt.Println("3. IR接收器是否正确连接到GPIO")
	}
	
	// 检查LIRC配置是否包含您的遥控器
	checkLircRemotes()
}

// 检查LIRC中的遥控器配置
func checkLircRemotes() {
	fmt.Println("检查LIRC遥控器配置...")
	
	// 检查配置目录中的文件
	cmd := exec.Command("ls", "-la", "/etc/lirc/lircd.conf.d/")
	output, _ := cmd.CombinedOutput()
	fmt.Printf("LIRC配置目录内容:\n%s\n", string(output))
	
	// 逐个检查可能的配置文件
	possibleConfigs := []string{
		"/etc/lirc/lircd.conf.d/devinput.lircd.conf",
		"/etc/lirc/lircd.conf",
		"/etc/lirc/lircd.conf.d/hello.conf",
	}
	
	foundConfig := false
	
	for _, configPath := range possibleConfigs {
		if _, err := os.Stat(configPath); err == nil {
			fmt.Printf("发现配置文件: %s\n", configPath)
			
			// 读取文件内容
			data, err := os.ReadFile(configPath)
			if err == nil {
				fmt.Printf("配置文件内容样本:\n")
				
				// 只显示前500个字符
				content := string(data)
				if len(content) > 500 {
					content = content[:500] + "..."
				}
				fmt.Println(content)
				
				// 检查是否包含按键定义
				if strings.Contains(content, "KEY_") {
					fmt.Println("配置包含有效的按键定义")
					foundConfig = true
				}
			}
		}
	}
	
	if !foundConfig {
		fmt.Println("未找到有效的遥控器配置定义")
		fmt.Println("将尝试使用直接IR输入模式")
	}
}

// 辅助函数，返回两个int的较小值
func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// 添加一个诊断函数来测试LIRC接收功能
func testLircReceiving() {
	fmt.Println("测试LIRC接收功能...")
	
	// 使用irw命令来测试接收
	cmd := exec.Command("timeout", "5", "irw")
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	
	fmt.Println("请在5秒内按下遥控器按钮...")
	err := cmd.Run()
	
	if err != nil && err.Error() != "exit status 124" { // timeout正常退出码是124
		fmt.Printf("irw测试失败: %v\n", err)
		fmt.Println("这可能意味着您的IR接收器没有正确设置")
	} else {
		fmt.Println("irw测试完成")
	}
}

// 检查LIRC是否已安装并运行
func checkLIRCInstallation() bool {
	// 检查LIRC服务是否运行
	cmd := exec.Command("systemctl", "is-active", "lircd")
	output, err := cmd.Output()
	if err != nil {
		fmt.Printf("检查LIRC服务状态出错: %v\n", err)
		return false
	}
	
	isActive := strings.TrimSpace(string(output)) == "active"
	if !isActive {
		// 尝试其他方法检查
		cmd = exec.Command("pgrep", "lircd")
		output, err = cmd.Output()
		if err == nil && len(output) > 0 {
			fmt.Println("LIRC进程正在运行，但systemd服务可能不活跃")
			return true
		}
	}
	
	return isActive
}

func testLircConfig() {
	// 测试LIRC配置
	cmd := exec.Command("irsend", "LIST", "", "")
	output, err := cmd.CombinedOutput()
	if err != nil {
		fmt.Printf("获取LIRC配置失败: %v\n", err)
		fmt.Println("请确保已经配置了遥控器:")
		fmt.Println("1. 运行 'sudo irrecord -d /dev/lirc0 ~/lircd.conf' 创建配置")
		fmt.Println("2. 复制配置 'sudo cp ~/lircd.conf /etc/lirc/lircd.conf.d/'")
		fmt.Println("3. 重启LIRC 'sudo systemctl restart lircd'")
	} else {
		fmt.Printf("LIRC配置信息:\n%s\n", string(output))
	}
}

// 检查硬件配置
func checkHardwareConfig() {
	// 检查config.txt文件中的dtoverlay设置
	// 支持不同版本的Raspberry Pi OS中可能的config.txt位置
	configPaths := []string{
		"/boot/firmware/config.txt",  // 较新版本的Raspberry Pi OS
		"/boot/config.txt",           // 传统版本的Raspberry Pi OS
	}
	
	configFound := false
	var configPath string
	var output []byte
	
	for _, path := range configPaths {
		if _, err := os.Stat(path); err == nil {
			configPath = path
			configFound = true
			
			cmd := exec.Command("grep", "dtoverlay=gpio-ir", path)
			output, _ = cmd.CombinedOutput()
			
			fmt.Printf("使用配置文件: %s\n", path)
			break
		}
	}
	
	if !configFound {
		fmt.Println("警告: 未找到config.txt文件")
		fmt.Println("请确认您的Raspberry Pi OS版本以及config.txt的位置")
		return
	}
	
	if len(output) == 0 {
		fmt.Printf("警告: 没有在%s中找到gpio-ir配置\n", configPath)
		fmt.Println("您可能需要添加以下行到config.txt:")
		fmt.Println("dtoverlay=gpio-ir,gpio_pin=18")  // 通常使用GPIO18作为IR接收器
		fmt.Println("然后重启树莓派")
	} else {
		fmt.Printf("找到IR配置: %s\n", string(output))
	}
	
	// 检查/dev/lirc0是否存在
	if _, err := os.Stat("/dev/lirc0"); os.IsNotExist(err) {
		fmt.Println("警告: /dev/lirc0设备不存在")
		fmt.Println("这可能意味着IR接收器驱动没有正确加载")
	} else {
		fmt.Println("IR设备 /dev/lirc0 存在")
	}
}

// 检查系统中可用的输入设备
func checkInputDevices() {
	fmt.Println("检查系统输入设备...")
	
	// 列出所有输入设备
	cmd := exec.Command("ls", "-la", "/dev/input/")
	output, _ := cmd.CombinedOutput()
	fmt.Printf("输入设备列表:\n%s\n", string(output))
	
	// 使用evtest查看设备详情
	cmd = exec.Command("timeout", "1", "evtest", "--info")
	output, _ = cmd.CombinedOutput()
	if len(output) > 0 {
		fmt.Printf("输入设备信息:\n%s\n", string(output))
	}
}

func main() {
	// 设置日志
	log.SetOutput(os.Stdout)
	log.SetFlags(log.Ldate | log.Ltime | log.Lmicroseconds)
	
	fmt.Println("开始接收遥控器信号...")
	
	// 显示LIRC版本信息
	cmd := exec.Command("lircd", "--version")
	output, err := cmd.CombinedOutput()
	if err == nil {
		fmt.Printf("LIRC版本: %s\n", string(output))
	} else {
		fmt.Printf("获取LIRC版本失败: %v\n", err)
	}
	
	// 检查LIRC安装
	if !checkLIRCInstallation() {
		fmt.Println("LIRC似乎没有正确安装或运行。请运行以下命令安装:")
		fmt.Println("sudo apt-get install lirc")
		fmt.Println("然后配置LIRC并启动服务")
		os.Exit(1)
	}
	
	// 测试LIRC配置
	testLircConfig()
	
	// 检查遥控器是否工作（新增）
	checkRemoteWorking()
	
	// 改进LIRC配置检查
	checkLircRemotes()
	
	// 测试LIRC接收功能
	testLircReceiving()
	
	// 检查硬件配置
	checkHardwareConfig()
	
	// 添加对输入设备的检查
	checkInputDevices()
	
	// 连接到LIRC
	conn, err := connectToLIRC()
	if err != nil {
		fmt.Println(err)
		fmt.Println("请确保:")
		fmt.Println("1. LIRC已经安装: sudo apt-get install lirc")
		fmt.Println("2. LIRC服务正在运行: sudo systemctl start lircd")
		fmt.Println("3. LIRC已正确配置您的遥控器")
		os.Exit(1)
	}
	defer conn.Close()
	
	fmt.Println("成功连接到LIRC，正在等待遥控器信号...")
	
	// 处理信号
	go processRemoteSignals(conn)
	
	// 防止程序立即退出
	fmt.Println("主程序继续运行中...")
	for {
		time.Sleep(time.Second * 60)
		fmt.Println("程序仍在运行，等待遥控器信号...")
	}
}
