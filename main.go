package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"github.com/spf13/viper"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

var deployRecordsFile = "deploy_records.json"
var appConfigFile = "app_config.json" // 应用配置文件，用于存储端口信息
var workingDir string                 // 全局变量存储当前工作目录
var uploadMutex sync.Mutex

// Config 结构体用于映射配置文件
type Config struct {
	Port     string `json:"port"`
	JavaFile string `json:"javafile"`
}

// 全局变量用于存储配置
var config Config

// 保存部署记录时，格式化时间
type DeployRecord struct {
	AppName    string `json:"app_name"`
	Version    string `json:"version"`
	ChangeLog  string `json:"change_log"`
	DeployTime string `json:"deploy_time"` // 保持为 time.Time 类型
	DeployType string `json:"deploy_type"` // 上传并部署，还是切换到旧版本
}

type AppConfig struct {
	Port            int    `json:"port"`
	PrometheusPort  int    `json:"prometheus_port"`             // Prometheus 端口
	JolokiaPort     int    `json:"jolokia_port"`                // Jolokia 端口
	JVMXms          string `json:"jvm_xms,omitempty"`           // eg: "1024m"
	JVMXmx          string `json:"jvm_xmx,omitempty"`           // eg: "2048m"
	MaxMetaspace    string `json:"max_metaspace,omitempty"`     // eg: "256m"
	MaxDirectMemory string `json:"max_direct_memory,omitempty"` // eg: "256m"
}

// 用于执行并发部署任务的通道
var deployChannel = make(chan deployTask, 100)

type deployTask struct {
	AppName  string
	FileName string
}

type JVMHeap struct {
	HeapUsedMB          int
	HeapMaxMB           int
	HeapUsagePercent    int
	NonHeapUsedMB       int
	NonHeapMaxMB        int
	NonHeapUsagePercent int
	MetaspaceUsedMB     int
	MetaspaceMaxMB      int
	MetaspacePercent    int
}

// 读取prometheus
func prometheusHandler(w http.ResponseWriter, r *http.Request) {
	appName := r.URL.Query().Get("app")
	if appName == "" {
		http.Error(w, "请提供 ?app= 应用名参数", http.StatusBadRequest)
		return
	}

	appConfigs := loadAppConfigs()
	appConfig, ok := appConfigs[appName]
	if !ok {
		http.Error(w, "找不到该应用配置: "+appName, http.StatusNotFound)
		return
	}

	prometheusPort := appConfig.PrometheusPort
	if prometheusPort == 0 {
		prometheusPort = appConfig.Port + 20000
	}

	url := fmt.Sprintf("http://127.0.0.1:%d/management/prometheus", prometheusPort)
	//prometheusPort = 28096
	//url := fmt.Sprintf("http://10.130.10.70:%d/management/prometheus", prometheusPort)
	resp, err := http.Get(url)
	if err != nil {
		http.Error(w, "无法访问 Prometheus: "+err.Error(), http.StatusInternalServerError)
		return
	}
	defer resp.Body.Close()

	type Metric struct {
		Name   string
		Labels map[string]string
		Value  float64
	}

	var metrics []Metric
	re := regexp.MustCompile(`^([\w:]+)(\{[^}]+\})?\s+([eE0-9\.+-]+)$`)
	scanner := bufio.NewScanner(resp.Body)

	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "#") || line == "" {
			continue
		}

		matches := re.FindStringSubmatch(line)
		if len(matches) != 4 {
			continue
		}

		name := matches[1]
		labelStr := matches[2]
		valStr := matches[3]

		value, err := strconv.ParseFloat(valStr, 64)
		if err != nil {
			continue
		}

		labels := make(map[string]string)
		if labelStr != "" {
			labelStr = strings.Trim(labelStr, "{}")
			parts := strings.Split(labelStr, ",")
			for _, p := range parts {
				kv := strings.SplitN(p, "=", 2)
				if len(kv) == 2 {
					labels[kv[0]] = strings.Trim(kv[1], `"`)
				}
			}
		}

		metrics = append(metrics, Metric{
			Name:   name,
			Labels: labels,
			Value:  value,
		})
	}

	findMetric := func(name string, labelKey string, labelVal string) float64 {
		for _, m := range metrics {
			if m.Name == name {
				if labelKey == "" || m.Labels[labelKey] == labelVal {
					return m.Value
				}
			}
		}
		return 0
	}

	fmt.Fprintln(w, "===== JVM 状态报告 =====")

	fmt.Fprintf(w, "\n🧵 线程信息\n")
	fmt.Fprintf(w, "Live Threads: %.0f\n", findMetric("jvm_threads_live_threads", "", ""))
	fmt.Fprintf(w, "Daemon Threads: %.0f\n", findMetric("jvm_threads_daemon_threads", "", ""))
	fmt.Fprintf(w, "Runnable Threads: %.0f\n", findMetric("jvm_threads_states_threads", "state", "runnable"))
	fmt.Fprintf(w, "Waiting Threads: %.0f\n", findMetric("jvm_threads_states_threads", "state", "waiting"))
	fmt.Fprintf(w, "Timed Waiting Threads: %.0f\n", findMetric("jvm_threads_states_threads", "state", "timed-waiting"))
	fmt.Fprintf(w, "Blocked Threads: %.0f\n", findMetric("jvm_threads_states_threads", "state", "blocked"))

	fmt.Fprintf(w, "\n🧠 堆内存使用\n")
	fmt.Fprintf(w, "Heap Used (G1 Old Gen): %.2f MB\n", findMetric("jvm_memory_used_bytes", "id", "G1 Old Gen")/1024/1024)
	fmt.Fprintf(w, "Heap Used (G1 Eden Space): %.2f MB\n", findMetric("jvm_memory_used_bytes", "id", "G1 Eden Space")/1024/1024)
	fmt.Fprintf(w, "Heap Used (G1 Survivor Space): %.2f MB\n", findMetric("jvm_memory_used_bytes", "id", "G1 Survivor Space")/1024/1024)

	fmt.Fprintf(w, "\n🧠 非堆内存使用\n")
	fmt.Fprintf(w, "Metaspace Used: %.2f MB\n", findMetric("jvm_memory_used_bytes", "id", "Metaspace")/1024/1024)

	// 如果你想计算非堆内存总和，可以额外写个函数遍历所有nonheap的指标累加
	// 聚合 GC 信息（基于 action 字段）
	gcCounts := map[string]float64{}
	gcPauses := map[string]float64{}

	for _, m := range metrics {
		if m.Name == "jvm_gc_pause_seconds_count" {
			action := m.Labels["action"]
			gcCounts[action] += m.Value
		}
		if m.Name == "jvm_gc_pause_seconds_sum" {
			action := m.Labels["action"]
			gcPauses[action] += m.Value
		}
	}

	fmt.Fprintf(w, "\n♻️ 垃圾回收（按 action 聚合）\n")
	if len(gcCounts) == 0 {
		fmt.Fprintln(w, "未检测到 GC 指标")
	} else {
		for action, count := range gcCounts {
			pause := gcPauses[action]
			fmt.Fprintf(w, "GC Action: %s → 次数: %.0f，总耗时: %.3f 秒\n", action, count, pause)
		}
	}

	fmt.Fprintf(w, "\n💾 HikariCP 连接池\n")
	fmt.Fprintf(w, "Active: %.0f / Idle: %.0f / Max: %.0f\n",
		findMetric("hikaricp_connections_active", "", ""),
		findMetric("hikaricp_connections_idle", "", ""),
		findMetric("hikaricp_connections_max", "", ""))

	fmt.Fprintf(w, "\n🏁 Top 5 耗时最长的 HTTP 接口（平均耗时）\n")

	type apiAvg struct {
		URI     string
		Count   float64
		Sum     float64
		Average float64
	}

	apiMap := map[string]*apiAvg{}

	for _, m := range metrics {
		if m.Name == "http_server_requests_seconds_count" || m.Name == "http_server_requests_seconds_sum" {
			uri := m.Labels["uri"]
			if uri == "" || uri == "UNKNOWN" {
				continue
			}
			stat, exists := apiMap[uri]
			if !exists {
				stat = &apiAvg{URI: uri}
				apiMap[uri] = stat
			}
			if m.Name == "http_server_requests_seconds_count" {
				stat.Count = m.Value
			} else {
				stat.Sum = m.Value
			}
		}
	}

	var avgList []apiAvg
	for _, s := range apiMap {
		if s.Count > 0 {
			s.Average = s.Sum / s.Count
			avgList = append(avgList, *s)
		}
	}

	sort.Slice(avgList, func(i, j int) bool {
		return avgList[i].Average > avgList[j].Average
	})

	topN := 5
	if len(avgList) < topN {
		topN = len(avgList)
	}

	if topN == 0 {
		fmt.Fprintln(w, "未找到接口耗时统计数据")
	} else {
		for i := 0; i < topN; i++ {
			item := avgList[i]
			fmt.Fprintf(w, "%d. %s -> %.3f 秒（%.0f 次）\n", i+1, item.URI, item.Average, item.Count)
		}
	}

	fmt.Fprintf(w, "\n🔋 CPU 与系统\n")
	fmt.Fprintf(w, "Process CPU Usage: %.2f%%\n", findMetric("process_cpu_usage", "", "")*100)
	fmt.Fprintf(w, "System CPU Usage: %.2f%%\n", findMetric("system_cpu_usage", "", "")*100)
	fmt.Fprintf(w, "1-min Load Average: %.2f\n", findMetric("system_load_average_1m", "", ""))
}

// fetchJVMHeapFromPrometheus 从Prometheus获取并解析JVM堆内存指标
func fetchJVMHeapFromPrometheus(prometheusPort int) (*JVMHeap, error) {
	// 构建Prometheus指标URL
	url := fmt.Sprintf("http://127.0.0.1:%d/management/prometheus", prometheusPort)
	resp, err := http.Get(url)
	if err != nil {
		return nil, fmt.Errorf("HTTP请求失败: %w", err)
	}
	defer resp.Body.Close()

	// 读取响应内容
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("读取响应失败: %w", err)
	}

	// 解析指标数据
	return parsePrometheusData(body)
}

// parsePrometheusData 解析Prometheus指标数据
func parsePrometheusData(data []byte) (*JVMHeap, error) {
	result := &JVMHeap{}
	heapUsed, heapMax := 0.0, 0.0
	nonHeapUsed, nonHeapMax := 0.0, 0.0
	metaspaceUsed, metaspaceMax := 0.0, 0.0

	// 正则表达式匹配指标行
	re := regexp.MustCompile(`^(jvm_memory_(used|max)_bytes)\{.*area="(heap|nonheap)",.*id="([^"]+)".*\}\s+(\S+)`)

	// 逐行处理指标数据
	lines := bytes.Split(data, []byte("\n"))
	for _, line := range lines {
		lineStr := strings.TrimSpace(string(line))
		if lineStr == "" || strings.HasPrefix(lineStr, "#") {
			continue
		}

		// 匹配内存指标
		matches := re.FindStringSubmatch(lineStr)
		if len(matches) != 6 {
			continue
		}

		// 解析指标值
		value, err := strconv.ParseFloat(matches[5], 64)
		if err != nil {
			continue
		}

		metricType := matches[2] // "used" 或 "max"
		area := matches[3]       // "heap" 或 "nonheap"
		id := matches[4]         // 内存区域ID

		// 根据内存区域和类型聚合数据
		switch area {
		case "heap":
			if metricType == "used" {
				heapUsed += value
			} else if metricType == "max" {
				heapMax += value
			}
		case "nonheap":
			if metricType == "used" {
				nonHeapUsed += value
				if id == "Metaspace" {
					metaspaceUsed = value
				}
			} else if metricType == "max" {
				nonHeapMax += value
				if id == "Metaspace" {
					metaspaceMax = value
				}
			}
		}
	}

	// 转换为MB (1 MB = 1048576 bytes)
	toMB := func(bytes float64) int {
		return int(bytes / 1048576)
	}

	// 计算百分比
	calcPercent := func(used, max float64) int {
		if max <= 0 {
			return 0
		}
		return int((used / max) * 100)
	}

	// 填充结果结构体
	result.HeapUsedMB = toMB(heapUsed)
	result.HeapMaxMB = toMB(heapMax)
	result.HeapUsagePercent = calcPercent(heapUsed, heapMax)

	result.NonHeapUsedMB = toMB(nonHeapUsed)
	result.NonHeapMaxMB = toMB(nonHeapMax)
	result.NonHeapUsagePercent = calcPercent(nonHeapUsed, nonHeapMax)

	result.MetaspaceUsedMB = toMB(metaspaceUsed)
	result.MetaspaceMaxMB = toMB(metaspaceMax)
	result.MetaspacePercent = calcPercent(metaspaceUsed, metaspaceMax)

	return result, nil
}

// 加载配置文件
func loadConfig() error {
	// 设置配置文件的路径和类型
	viper.SetConfigName("config") // 配置文件名 (不包含扩展名)
	viper.AddConfigPath(".")      // 配置文件路径

	// 读取配置文件
	if err := viper.ReadInConfig(); err != nil {
		return fmt.Errorf("failed to read config file: %v", err)
	}

	// 将配置文件内容映射到结构体
	if err := viper.Unmarshal(&config); err != nil {
		return fmt.Errorf("failed to unmarshal config: %v", err)
	}

	return nil
}

func main() {
	// 在 main() 中为全局变量赋值
	var err error
	// 加载配置文件
	if err := loadConfig(); err != nil {
		log.Fatal("Error loading config:", err)
	}
	//如果javafile不为空。显示地址
	if config.JavaFile != "" {
		fmt.Println("Java file path:", config.JavaFile)
	} else {
		fmt.Println("Java file not specified, using default 'java'")
	}

	workingDir, err = os.Getwd()
	if err != nil {
		fmt.Println("Error getting current working directory:", err)
		os.Exit(1)
	}

	fmt.Println("Current working directory set to:", workingDir)
	http.HandleFunc("/apps", appsHandler)
	http.HandleFunc("/history", historyHandler)
	http.HandleFunc("/uploadChunk", uploadChunkHandler)
	http.HandleFunc("/mergeChunks", mergeChunksHandler)
	http.HandleFunc("/switchVersion", switchVersionHandler)
	http.HandleFunc("/current_versions", currentVersionsHandler)
	http.HandleFunc("/startAllApps", startAllAppsHandler)
	http.HandleFunc("/manageAllApps", manageAllAppsHandler)
	http.HandleFunc("/jvmStatus", prometheusHandler)
	http.HandleFunc("/logs", logHandler)
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			// 非首页路径返回 404
			http.NotFound(w, r)
			return
		}
		http.ServeFile(w, r, filepath.Join(workingDir, "./frontend/index.html"))
	})

	// 启动处理部署任务的 goroutine
	go handleDeployTasks()

	fmt.Println("Server is running on port %s...", config.Port)
	http.ListenAndServe(config.Port, nil)
}

func uploadChunkHandler(w http.ResponseWriter, r *http.Request) {
	r.ParseMultipartForm(20 << 20) // 每块最多 20MB
	appName := r.FormValue("app_name")
	fileName := r.FormValue("file_name")
	chunkNumber := r.FormValue("chunk_number")

	file, _, err := r.FormFile("file")
	if err != nil {
		http.Error(w, "Error reading chunk", http.StatusBadRequest)
		return
	}
	defer file.Close()

	chunkDir := filepath.Join(workingDir, "uploads", appName, "chunks", fileName)
	os.MkdirAll(chunkDir, os.ModePerm)

	chunkPath := filepath.Join(chunkDir, fmt.Sprintf("%s.part%s", fileName, chunkNumber))
	out, err := os.Create(chunkPath)
	if err != nil {
		http.Error(w, "Failed to save chunk", http.StatusInternalServerError)
		return
	}
	defer out.Close()

	_, err = io.Copy(out, file)
	if err != nil {
		http.Error(w, "Error writing chunk", http.StatusInternalServerError)
		return
	}

	w.Write([]byte("Chunk uploaded"))
}

func mergeChunksHandler(w http.ResponseWriter, r *http.Request) {
	appName := r.FormValue("app_name")
	fileName := r.FormValue("file_name")
	totalChunks := r.FormValue("total_chunks") // string

	total, err := strconv.Atoi(totalChunks)
	if err != nil {
		http.Error(w, "Invalid total_chunks", http.StatusBadRequest)
		return
	}

	chunkDir := filepath.Join(workingDir, "uploads", appName, "chunks", fileName)
	finalDir := filepath.Join(workingDir, "uploads", appName)
	os.MkdirAll(finalDir, os.ModePerm)

	finalFilePath := filepath.Join(finalDir, fileName)
	finalFile, err := os.Create(finalFilePath)
	if err != nil {
		http.Error(w, "Failed to create final file", http.StatusInternalServerError)
		return
	}
	defer finalFile.Close()

	for i := 0; i < total; i++ {
		chunkPath := filepath.Join(chunkDir, fmt.Sprintf("%s.part%d", fileName, i))
		chunkData, err := os.Open(chunkPath)
		if err != nil {
			http.Error(w, fmt.Sprintf("Missing chunk %d", i), http.StatusInternalServerError)
			return
		}
		io.Copy(finalFile, chunkData)
		chunkData.Close()
	}

	// Optionally delete chunks
	os.RemoveAll(chunkDir)

	//存储部署记录
	deployRecord := DeployRecord{
		AppName:    appName,
		Version:    strings.TrimSuffix(fileName, filepath.Ext(fileName)),
		ChangeLog:  r.FormValue("change_log"),
		DeployTime: time.Now().Format("2006-01-02 15:04:05"), // 保证时间是 UTC
		DeployType: "upload",
	}
	appendDeployRecord(deployRecord)

	w.Write([]byte("File merged successfully"))

	// 提交部署任务
	deployChannel <- deployTask{
		AppName:  appName,
		FileName: fileName,
	}
}

// 处理并发部署任务
func handleDeployTasks() {
	for task := range deployChannel {
		// 执行部署任务
		deployNewJar(task.AppName, task.FileName)
	}
}

// 检查端口是否被占用
func isPortInUse(port int) bool {
	ln, err := net.Listen("tcp", fmt.Sprintf(":%d", port))
	if err != nil {
		return true
	}
	defer ln.Close()
	return false
}

func killProcessOnPort(port int) error {
	var cmd *exec.Cmd
	var output []byte
	var err error

	// 根据操作系统选择不同的命令
	if (runtime.GOOS == "linux") || (runtime.GOOS == "darwin") {
		// Linux 上使用 lsof 命令
		cmd = exec.Command("lsof", "-t", "-i", fmt.Sprintf(":%d", port))
		output, err = cmd.CombinedOutput()
		if err != nil {
			// 如果没有找到进程，返回相应错误
			if strings.Contains(string(output), "lsof: no process found") {
				fmt.Println("No process found on port", port)
				return nil
			}
			return fmt.Errorf("Error finding process on port %d: %v", port, err)
		}
	} else if runtime.GOOS == "windows" {
		// Windows 上使用 netstat 和 findstr
		cmd = exec.Command("cmd", "/C", fmt.Sprintf("netstat -ano | findstr :%d", port))
		output, err = cmd.CombinedOutput()
		if err != nil {
			return fmt.Errorf("Error finding process on port %d: %v", port, err)
		}
	}

	// 获取进程ID列表
	pidList := strings.TrimSpace(string(output))
	if pidList == "" {
		return fmt.Errorf("No process found on port %d", port)
	}

	// 如果是 Windows，根据 PID 查找并杀死进程
	if runtime.GOOS == "windows" {
		// 处理 netstat 输出，找到最后的 PID
		lines := strings.Split(pidList, "\n")
		for _, line := range lines {
			// 提取 PID，这里假设 PID 在输出的最后一列
			parts := strings.Fields(line)
			if len(parts) > 1 {
				pid := parts[len(parts)-1]
				// 检查进程是否还在
				_, err := exec.Command("tasklist", "/FI", fmt.Sprintf("PID eq %s", pid)).CombinedOutput()
				if err != nil {
					// 如果进程已经结束，跳过
					fmt.Printf("Process with PID %s is not running anymore.\n", pid)
					continue
				}
				// 使用 taskkill 杀死进程
				killCmd := exec.Command("taskkill", "/F", "/PID", pid)
				_, err = killCmd.CombinedOutput()
				if err != nil {
					fmt.Println("Error killing process with PID", pid, ":", err)
					return fmt.Errorf("Error killing process on port %d with PID %s: %v", port, pid, err)
				}
				fmt.Println("Successfully killed process with PID", pid)
				// 休眠 2 秒
				time.Sleep(2 * time.Second)
			}
		}
	} else {
		// Linux 上处理 PID 列表
		pids := strings.Split(pidList, "\n")
		for _, pid := range pids {
			pid = strings.TrimSpace(pid)
			if pid == "" {
				continue
			}

			// 检查进程是否存在
			_, err := exec.Command("ps", "-p", pid).CombinedOutput()
			if err != nil {
				// 如果进程已经退出，跳过
				fmt.Printf("Process with PID %s is not running anymore.\n", pid)
				continue
			}

			// 杀死进程
			killCmd := exec.Command("kill", "-9", pid)
			_, err = killCmd.CombinedOutput()
			if err != nil {
				fmt.Println("Error killing process with PID", pid, ":", err)
				return fmt.Errorf("Error killing process on port %d with PID %s: %v", port, pid, err)
			}
			fmt.Println("Successfully killed process with PID", pid)

			// 休眠 2 秒
			time.Sleep(2 * time.Second)
		}
	}

	return nil
}

// 从文件加载单个应用配置
func loadAppConfig(appName string) AppConfig {
	configs := loadAppConfigs()
	if config, ok := configs[appName]; ok {
		return config
	}
	return AppConfig{Port: 8080} // 返回默认端口配置
}

func deployNewJar(appName string, fileName string) {
	// 获取应用目录
	appDir := filepath.Join(workingDir, "uploads", appName)

	// 获取端口配置
	appconfig := loadAppConfig(appName)
	if appconfig.Port == 0 {
		appconfig.Port = 8080
	}

	// 构建 JAR 路径
	jarFilePath := filepath.Join(appDir, fileName)
	fmt.Println("Deploying JAR file:", jarFilePath)

	// 停止原有服务
	if isPortInUse(appconfig.Port) {
		fmt.Println("Port", appconfig.Port, "in use. Stopping existing service...")
		err := killProcessOnPort(appconfig.Port)
		if err != nil {
			fmt.Println("Failed to stop existing service:", err)
		} else {
			fmt.Println("Existing service stopped.")
		}
	}

	// 检查 JAR 是否存在
	if _, err := os.Stat(jarFilePath); os.IsNotExist(err) {
		fmt.Println("JAR not found:", jarFilePath)
		return
	}

	// 日志文件路径
	logFileName := fmt.Sprintf("%s.log", strings.TrimSuffix(fileName, filepath.Ext(fileName)))
	logFilePath := filepath.Join(workingDir, "logs", logFileName)
	//如果日志文件已存在，则用-1,-2, -3等命名
	if _, err := os.Stat(logFilePath); err == nil {
		i := 1
		for {
			newLogFilePath := fmt.Sprintf("%s-%d.log", strings.TrimSuffix(logFilePath, ".log"), i)
			if _, err := os.Stat(newLogFilePath); os.IsNotExist(err) {
				logFilePath = newLogFilePath
				break
			}
			i++
		}
	}

	// 创建日志文件
	logFile, err := os.Create(logFilePath)
	if err != nil {
		fmt.Println("Failed to create log file:", err)
		return
	}
	// 注意：这里不能 defer logFile.Close()，因为日志输出会持续，不能提前关闭

	// 准备命令
	javaFile := config.JavaFile
	if javaFile == "" {
		javaFile = "java"
	}
	// Jolokia agent 配置
	//jolokiaAgentPath := filepath.Join(workingDir, "jolokia-jvm-1.7.2.jar") // 当前目录
	//jolokiaPort := appconfig.JolokiaPort                                   // 你可以自定义传入不同端口
	//if jolokiaPort == 0 {
	//	// 如果没有配置 Jolokia 端口，使用3000加上appconfig.Port
	//	jolokiaPort = 3000 + appconfig.Port
	//}
	//
	//jolokiaOption := fmt.Sprintf("-javaagent:%s=port=%d,host=127.0.0.1", jolokiaAgentPath, jolokiaPort)

	xms := appconfig.JVMXms
	if xms == "" {
		xms = "1024m"
	}

	xmx := appconfig.JVMXmx
	if xmx == "" {
		xmx = "1024m"
	}

	maxMetaspace := appconfig.MaxMetaspace
	if maxMetaspace == "" {
		maxMetaspace = "256m"
	}

	maxDirect := appconfig.MaxDirectMemory
	if maxDirect == "" {
		maxDirect = "256m"
	}

	jvmOptions := []string{
		// jolokiaOption,
		"-Xms" + xms,
		"-Xmx" + xmx,
		"-XX:+UseG1GC",
		"-XX:MaxMetaspaceSize=" + maxMetaspace,
		"-XX:MaxDirectMemorySize=" + maxDirect,
		"-Dfile.encoding=UTF-8",
		"-jar", jarFilePath,
		fmt.Sprintf("--server.port=%d", appconfig.Port),
	}

	var cmd *exec.Cmd
	if runtime.GOOS == "windows" {
		cmd = exec.Command(javaFile, jvmOptions...)
	} else {
		cmd = exec.Command(javaFile, jvmOptions...)
	}
	setCmdSysProcAttr(cmd)

	cmd.Stdout = logFile
	cmd.Stderr = logFile
	cmd.Dir = appDir // 切换目录
	cmd.Env = append(cmd.Env, "CONFIG_PATH="+appDir)

	// 异步启动服务
	go func() {
		fmt.Println("Starting JAR in background:", cmd.String())
		err := cmd.Start()
		if err != nil {
			fmt.Println("Failed to start JAR:", err)
			logFile.Close()
			return
		}
		fmt.Printf("JAR started successfully on port %d. Logs: %s\n", appconfig.Port, logFilePath)
		updateCurrentVersion(appName, strings.TrimSuffix(fileName, filepath.Ext(fileName)), appconfig.Port)

		// 可选：等待子进程结束（不会阻塞主线程）
		err = cmd.Wait()
		if err != nil {
			fmt.Println("JAR exited with error:", err)
		} else {
			fmt.Println("JAR exited normally.")
		}
		logFile.Close()
	}()
}

// 从文件加载应用配置
func loadAppConfigs() map[string]AppConfig {
	configs := make(map[string]AppConfig)

	file, err := os.Open(filepath.Join(workingDir, appConfigFile))
	if err != nil {
		if os.IsNotExist(err) {
			return configs // 文件不存在时返回空配置
		}
		fmt.Println("Error opening app config file:", err)
		return nil
	}
	defer file.Close()

	decoder := json.NewDecoder(file)
	err = decoder.Decode(&configs)
	if err != nil && err.Error() != "EOF" {
		fmt.Println("Error decoding app config:", err)
	}

	return configs
}

// 保存部署记录
func appendDeployRecord(record DeployRecord) {
	// 加载原有部署记录
	records := loadDeployRecords()
	records = append(records, record)

	// 写入文件
	file, err := os.Create(filepath.Join(workingDir, deployRecordsFile))
	if err != nil {
		fmt.Println("Error creating deploy records file:", err)
		return
	}
	defer file.Close()

	encoder := json.NewEncoder(file)
	encoder.SetIndent("", "  ")
	err = encoder.Encode(records)
	if err != nil {
		fmt.Println("Error encoding deploy records to JSON:", err)
	}
}

// 从文件加载部署记录
func loadDeployRecords() []DeployRecord {
	var records []DeployRecord
	file, err := os.Open(filepath.Join(workingDir, deployRecordsFile))
	if err != nil {
		if os.IsNotExist(err) {
			return records // 文件不存在，返回空记录
		}
		fmt.Println("Error opening deploy records file:", err)
		return nil
	}
	defer file.Close()

	decoder := json.NewDecoder(file)
	err = decoder.Decode(&records)
	if err != nil && err.Error() != "EOF" {
		fmt.Println("Error decoding deploy records:", err)
	}
	return records
}

// 停止服务
func stopService(appName string) {
	// 根据应用名称停止对应服务
	fmt.Printf("Stopping service for %s...\n", appName)
	// 这里可以根据不同的应用名称调用不同的服务停止方法
}

// 查询历史记录并格式化时间
func historyHandler(w http.ResponseWriter, r *http.Request) {
	appName := r.URL.Query().Get("app_name")

	file, err := os.Open(filepath.Join(workingDir, deployRecordsFile))
	if err != nil {
		http.Error(w, "Failed to open deploy records", http.StatusInternalServerError)
		return
	}
	defer file.Close()

	var records []DeployRecord
	err = json.NewDecoder(file).Decode(&records)
	if err != nil {
		http.Error(w, "Failed to decode deploy records", http.StatusInternalServerError)
		return
	}

	if appName != "" {
		var filtered []DeployRecord
		for _, record := range records {
			if record.AppName == appName {
				filtered = append(filtered, record)
			}
		}
		records = filtered
	}

	// 按时间倒序排序
	sort.Slice(records, func(i, j int) bool {
		t1, err1 := time.Parse("2006-01-02 15:04:05", records[i].DeployTime)
		t2, err2 := time.Parse("2006-01-02 15:04:05", records[j].DeployTime)
		if err1 != nil || err2 != nil {
			return false
		}
		return t1.After(t2) // 倒序
	})
	//仅返回最近 30 条记录
	if len(records) > 30 {
		records = records[:30]
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(records)
}

func switchVersionHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}

	// 解析请求体
	// 请求体结构
	type SwitchVersionRequest struct {
		AppName string `json:"app_name"`
		Version string `json:"version"`
	}
	var req SwitchVersionRequest
	err := json.NewDecoder(r.Body).Decode(&req)
	if err != nil {
		http.Error(w, "Invalid JSON", http.StatusBadRequest)
		return
	}

	if req.AppName == "" || req.Version == "" {
		http.Error(w, "Missing app_name or version", http.StatusBadRequest)
		return
	}
	// 提交部署任务
	// req.Version 需要拼上.jar
	if !strings.HasSuffix(req.Version, ".jar") {
		req.Version += ".jar"
	}
	//存储部署记录
	deployRecord := DeployRecord{
		AppName:    req.AppName,
		Version:    strings.TrimSuffix(req.Version, filepath.Ext(req.Version)),
		ChangeLog:  "切换版本",
		DeployTime: time.Now().Format("2006-01-02 15:04:05"), // 保证时间是 UTC
		DeployType: "switch",
	}
	appendDeployRecord(deployRecord)
	deployChannel <- deployTask{
		AppName:  req.AppName,
		FileName: req.Version,
	}
}

// 返回应用名称的列表
func appsHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	// 加载应用配置
	configs := loadAppConfigs()

	// 获取应用名称列表
	var appNames []string
	for appName := range configs {
		appNames = append(appNames, appName)
	}

	// 返回 JSON 格式的应用名称列表
	data, err := json.Marshal(appNames)
	if err != nil {
		http.Error(w, "Error marshaling app names", http.StatusInternalServerError)
		return
	}

	w.Write(data)
}

type CurrentVersion struct {
	AppName   string `json:"app_name"`
	Version   string `json:"version"`
	Timestamp string `json:"timestamp"`
	Port      int    `json:"port"`
}

// 添加port参数
//
//	func updateCurrentVersion(appName, version string, port int) error {
//		var versions map[string]CurrentVersion
//
//		path := filepath.Join(workingDir, "current_versions.json")
//		data, err := os.ReadFile(path)
//		if err == nil {
//			json.Unmarshal(data, &versions)
//		} else {
//			versions = make(map[string]CurrentVersion)
//		}
//
//		versions[appName] = CurrentVersion{
//			AppName:   appName,
//			Version:   version,
//			Port:      port,
//			Timestamp: time.Now().Format("2006-01-02 15:04:05"),
//		}
//
//		updated, _ := json.MarshalIndent(versions, "", "  ")
//		return os.WriteFile(path, updated, 0644)
//	}
func updateCurrentVersion(appName, version string, port int) error {
	path := filepath.Join(workingDir, "current_versions.json")
	tmpPath := path + ".tmp"

	// 初始化为一个空 map
	versions := make(map[string]CurrentVersion)

	// 尝试读取原始 JSON 文件
	if data, err := os.ReadFile(path); err == nil && len(data) > 0 {
		// 尝试解析 JSON
		if err := json.Unmarshal(data, &versions); err != nil {
			fmt.Println("警告：current_versions.json 格式解析失败，已备份原始文件")
			_ = os.WriteFile(path+".bak", data, 0644)
			versions = make(map[string]CurrentVersion) // 清空旧内容
		}
	}

	// 更新或新增版本记录
	versions[appName] = CurrentVersion{
		AppName:   appName,
		Version:   version,
		Port:      port,
		Timestamp: time.Now().Format("2006-01-02 15:04:05"),
	}

	// 序列化 JSON
	updated, err := json.MarshalIndent(versions, "", "  ")
	if err != nil {
		return fmt.Errorf("序列化失败: %w", err)
	}

	// 写入临时文件
	if err := os.WriteFile(tmpPath, updated, 0644); err != nil {
		return fmt.Errorf("写入临时文件失败: %w", err)
	}

	// 原子替换旧文件
	if err := os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("替换原文件失败: %w", err)
	}

	return nil
}

func currentVersionsHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	path := filepath.Join(workingDir, "current_versions.json")
	data, err := os.ReadFile(path)
	if err != nil {
		http.Error(w, "无法读取当前部署记录", http.StatusInternalServerError)
		return
	}

	var versions map[string]map[string]interface{}
	if err := json.Unmarshal(data, &versions); err != nil {
		http.Error(w, "JSON 解析失败", http.StatusInternalServerError)
		return
	}

	// 添加运行状态
	for _, info := range versions {
		if portFloat, ok := info["port"].(float64); ok {
			port := int(portFloat)
			info["running"] = isPortInUse(port)
		} else {
			info["running"] = false
		}
		// 推断 Jolokia 端口，例如 appPort + 10000
		jolokiaPort := info["port"].(float64) + 20000 //todo: 此处需优化成从 appConfig 中获取
		if mem, err := fetchJVMHeapFromPrometheus(int(jolokiaPort)); err == nil {
			info["heapUsedMB"] = mem.HeapUsedMB
			info["heapMaxMB"] = mem.HeapMaxMB
			info["heapUsagePercent"] = mem.HeapUsagePercent

			info["nonHeapUsedMB"] = mem.NonHeapUsedMB
			info["nonHeapMaxMB"] = mem.NonHeapMaxMB
			info["nonHeapUsagePercent"] = mem.NonHeapUsagePercent

			info["metaspaceUsedMB"] = mem.MetaspaceUsedMB
			info["metaspaceMaxMB"] = mem.MetaspaceMaxMB
			info["metaspacePercent"] = mem.MetaspacePercent
		}
	}

	// 返回带状态的 JSON
	response, _ := json.MarshalIndent(versions, "", "  ")
	w.Write(response)
}

//func logHandler(w http.ResponseWriter, r *http.Request) {
//	appFile := r.URL.Query().Get("appfile")
//	if appFile == "" {
//		http.Error(w, "Missing app_name parameter", http.StatusBadRequest)
//		return
//	}
//
//	// 默认读取最近 100 行
//	linesParam := r.URL.Query().Get("lines")
//	numLines := 100
//	if linesParam != "" {
//		if parsed, err := strconv.Atoi(linesParam); err == nil && parsed > 0 {
//			numLines = parsed
//		}
//	}
//
//	logFilePath := filepath.Join(workingDir, "logs", appFile+".log")
//
//	if _, err := os.Stat(logFilePath); os.IsNotExist(err) {
//		http.Error(w, "Log file not found", http.StatusNotFound)
//		return
//	}
//
//	content, err := tailFile(logFilePath, numLines)
//	if err != nil {
//		http.Error(w, fmt.Sprintf("Error reading log: %v", err), http.StatusInternalServerError)
//		return
//	}
//
//	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
//	w.Write([]byte(content))
//}

func compareLogNames(a, b string) bool {
	baseA, numA := extractBaseAndNum(a)
	baseB, numB := extractBaseAndNum(b)

	if baseA != baseB {
		return baseA < baseB
	}
	return numA < numB
}

func extractBaseAndNum(filename string) (base string, num int) {
	if strings.HasSuffix(filename, ".log") {
		name := strings.TrimSuffix(filename, ".log")
		parts := strings.Split(name, "-")
		if len(parts) == 2 {
			// 形如 aaa-1.log
			n, err := strconv.Atoi(parts[1])
			if err == nil {
				return parts[0], n
			}
		}
		// 没有 - 数字 的部分
		return name, -1
	}
	return filename, -1
}

func logHandler(w http.ResponseWriter, r *http.Request) {
	appFile := r.URL.Query().Get("appfile")
	if appFile == "" {
		http.Error(w, "Missing appfile parameter", http.StatusBadRequest)
		return
	}

	// 默认读取最近 100 行
	linesParam := r.URL.Query().Get("lines")
	numLines := 100
	if linesParam != "" {
		if parsed, err := strconv.Atoi(linesParam); err == nil && parsed > 0 {
			numLines = parsed
		}
	}

	// 尝试查找最新的日志文件
	logDir := filepath.Join(workingDir, "logs")

	// 构建模式匹配所有版本的日志文件
	pattern := filepath.Join(logDir, appFile+"*.log")
	matches, err := filepath.Glob(pattern)
	if err != nil || len(matches) == 0 {
		http.Error(w, "Log file not found", http.StatusNotFound)
		return
	}

	// 选择最新的日志文件：按文件名排序，最后一个为最新（包含 -1, -2, ... 的）
	//sort.Strings(matches)
	sort.Slice(matches, func(i, j int) bool {
		return compareLogNames(matches[i], matches[j])
	})
	latestLogFile := matches[len(matches)-1]

	content, err := tailFile(latestLogFile, numLines)
	if err != nil {
		http.Error(w, fmt.Sprintf("Error reading log: %v", err), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Write([]byte(content))
}

func tailFile(filePath string, lines int) (string, error) {
	file, err := os.Open(filePath)
	if err != nil {
		return "", err
	}
	defer file.Close()

	var result []string
	buffer := make([]byte, 4096)
	stat, _ := file.Stat()
	filesize := stat.Size()
	var cursor int64 = filesize

	lineCount := 0
	lineBuffer := ""
	for cursor > 0 && lineCount < lines {
		readSize := int64(len(buffer))
		if cursor < readSize {
			readSize = cursor
		}
		cursor -= readSize

		file.Seek(cursor, io.SeekStart)
		n, err := file.Read(buffer[:readSize])
		if err != nil && err != io.EOF {
			return "", err
		}

		chunk := string(buffer[:n]) + lineBuffer
		linesInChunk := strings.Split(chunk, "\n")
		if len(linesInChunk) > 1 {
			lineBuffer = linesInChunk[0]
			linesInChunk = linesInChunk[1:]
		}
		result = append(linesInChunk, result...)
		lineCount = len(result)
	}

	// 只保留最后的 lines 行
	if len(result) > lines {
		result = result[len(result)-lines:]
	}

	return strings.Join(result, "\n"), nil
}

func loadCurrentVersions() []CurrentVersion {
	var versionMap map[string]CurrentVersion
	var versions []CurrentVersion

	filePath := filepath.Join(workingDir, "current_versions.json")
	if content, err := os.ReadFile(filePath); err == nil {
		if err := json.Unmarshal(content, &versionMap); err == nil {
			for _, v := range versionMap {
				versions = append(versions, v)
			}
		}
	}
	return versions
}

func startAllAppsHandler(w http.ResponseWriter, r *http.Request) {
	currentVersions := loadCurrentVersions()
	if len(currentVersions) == 0 {
		http.Error(w, "No deployed apps found", http.StatusNotFound)
		return
	}
	//从currentVersions中获取 Port 未被占用的app，并启动
	for _, v := range currentVersions {
		if !isPortInUse(v.Port) {
			go deployNewJar(v.AppName, v.Version+".jar")
		}
	}
	// 直接返回response.OK
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	w.Write([]byte(`{"status": "OK"}`))

}

func restartAllAppsHandler(w http.ResponseWriter, r *http.Request) {
	currentVersions := loadCurrentVersions()
	if len(currentVersions) == 0 {
		http.Error(w, "No deployed apps found", http.StatusNotFound)
		return
	}

	for _, v := range currentVersions {
		if isPortInUse(v.Port) {
			err := killProcessOnPort(v.Port)
			if err != nil {
				fmt.Println("Failed to stop existing service for", v.AppName, ":", err)
			} else {
				fmt.Println("Existing service for", v.AppName, "stopped.")
			}
		}
		go deployNewJar(v.AppName, v.Version+".jar")
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	w.Write([]byte(`{"status": "OK"}`))
}

func manageAllAppsHandler(w http.ResponseWriter, r *http.Request) {
	action := r.URL.Query().Get("action")
	if action != "restart" && action != "stop" {
		http.Error(w, "Invalid action, must be 'restart' or 'stop'", http.StatusBadRequest)
		return
	}

	currentVersions := loadCurrentVersions()
	if len(currentVersions) == 0 {
		http.Error(w, "No deployed apps found", http.StatusNotFound)
		return
	}

	for _, v := range currentVersions {
		if isPortInUse(v.Port) {
			err := killProcessOnPort(v.Port)
			if err != nil {
				fmt.Println("Failed to stop existing service for", v.AppName, ":", err)
			} else {
				fmt.Println("Existing service for", v.AppName, "stopped.")
			}
		}

		// restart 时启动新服务，stop 时不启动
		if action == "restart" {
			go deployNewJar(v.AppName, v.Version+".jar")
		}
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	w.Write([]byte(`{"status": "OK", "action": "` + action + `"}`))
}
