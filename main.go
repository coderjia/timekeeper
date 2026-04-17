package main

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"log"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/mgr"
)

const (
	serviceName = "CoderJia-TimeKeeper"
	ntpDelta    = 2208988800 // NTP epoch (1900) to Unix epoch (1970)
)

// 中国大陆常用 NTP（UDP 123），按顺序尝试，失败则换下一个。
var ntpServers = []string{
	"ntp.aliyun.com:123",
	"ntp1.aliyun.com:123",
	"ntp2.aliyun.com:123",
	"ntp.tencent.com:123",
	"time1.tencent.com:123",
	"time2.tencent.com:123",
	"ntp.ntsc.ac.cn:123",
	"cn.pool.ntp.org:123",
}

var (
	modkernel32   = syscall.NewLazyDLL("kernel32.dll")
	procSetSysUTC = modkernel32.NewProc("SetSystemTime")
)

type systemTime struct {
	Year         uint16
	Month        uint16
	DayOfWeek    uint16
	Day          uint16
	Hour         uint16
	Minute       uint16
	Second       uint16
	Milliseconds uint16
}

type timekeeper struct {
	mu        sync.RWMutex
	baseUnix  int64
	baseMono  time.Time
	lastSync  time.Time
	resyncDur time.Duration
}

func newTimekeeper() *timekeeper {
	return &timekeeper{resyncDur: 10 * time.Minute}
}

func (t *timekeeper) initFromNTP() error {
	unixTs, addr, err := queryNTPUnixFirstOK(ntpServers, 5*time.Second)
	if err != nil {
		return err
	}
	nowMono := time.Now()
	t.mu.Lock()
	t.baseUnix = unixTs
	t.baseMono = nowMono
	t.lastSync = nowMono
	t.mu.Unlock()
	log.Printf("NTP 校时成功: unix=%d server=%s", unixTs, addr)
	return nil
}

func (t *timekeeper) currentUTCTime() time.Time {
	t.mu.RLock()
	baseUnix := t.baseUnix
	baseMono := t.baseMono
	t.mu.RUnlock()
	return time.Unix(baseUnix, 0).Add(time.Since(baseMono)).UTC()
}

func (t *timekeeper) maybeResync() {
	t.mu.RLock()
	last := t.lastSync
	t.mu.RUnlock()
	if time.Since(last) < t.resyncDur {
		return
	}
	if err := t.initFromNTP(); err != nil {
		log.Printf("NTP 重同步失败: %v", err)
	}
}

func queryNTPUnixFirstOK(addrs []string, timeout time.Duration) (unix int64, used string, err error) {
	var errs []error
	for _, addr := range addrs {
		u, e := queryNTPUnix(addr, timeout)
		if e == nil {
			return u, addr, nil
		}
		log.Printf("NTP 尝试失败 server=%s: %v", addr, e)
		errs = append(errs, fmt.Errorf("%s: %w", addr, e))
	}
	if len(errs) == 0 {
		return 0, "", errors.New("未配置任何 NTP 服务器")
	}
	return 0, "", fmt.Errorf("全部 NTP 不可用（%d 个节点均失败）: %w", len(addrs), errors.Join(errs...))
}

func queryNTPUnix(addr string, timeout time.Duration) (int64, error) {
	conn, err := net.DialTimeout("udp", addr, timeout)
	if err != nil {
		return 0, fmt.Errorf("连接 NTP 失败: %w", err)
	}
	defer conn.Close()

	_ = conn.SetDeadline(time.Now().Add(timeout))

	req := make([]byte, 48)
	req[0] = 0x1B // LI=0, VN=3, Mode=3(client)
	if _, err = conn.Write(req); err != nil {
		return 0, fmt.Errorf("发送 NTP 请求失败: %w", err)
	}

	resp := make([]byte, 48)
	n, err := conn.Read(resp)
	if err != nil {
		return 0, fmt.Errorf("读取 NTP 响应失败: %w", err)
	}
	if n < 48 {
		return 0, fmt.Errorf("NTP 响应长度异常: %d", n)
	}

	seconds := binary.BigEndian.Uint32(resp[40:44]) // Transmit Timestamp seconds
	if seconds < ntpDelta {
		return 0, fmt.Errorf("NTP 秒值异常: %d", seconds)
	}
	return int64(seconds - ntpDelta), nil
}

func setSystemUTC(t time.Time) error {
	utc := t.UTC()
	st := systemTime{
		Year:         uint16(utc.Year()),
		Month:        uint16(utc.Month()),
		DayOfWeek:    uint16(utc.Weekday()),
		Day:          uint16(utc.Day()),
		Hour:         uint16(utc.Hour()),
		Minute:       uint16(utc.Minute()),
		Second:       uint16(utc.Second()),
		Milliseconds: uint16(utc.Nanosecond() / int(time.Millisecond)),
	}
	ret, _, callErr := procSetSysUTC.Call(uintptr(unsafe.Pointer(&st)))
	if ret == 0 {
		if callErr != syscall.Errno(0) {
			return fmt.Errorf("SetSystemTime 调用失败: %w", callErr)
		}
		return errors.New("SetSystemTime 调用失败: 未知错误")
	}
	return nil
}

func runCore(stop <-chan struct{}) error {
	log.Println("核心守护进程启动")
	tk := newTimekeeper()
	if err := tk.initFromNTP(); err != nil {
		return fmt.Errorf("初始化 NTP 校时失败: %w", err)
	}

	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-stop:
			log.Println("收到停止信号，守护进程退出")
			return nil
		case <-ticker.C:
			tk.maybeResync()
			current := tk.currentUTCTime()
			if err := setSystemUTC(current); err != nil {
				log.Printf("设置系统时间失败: %v", err)
			}
		}
	}
}

type serviceProgram struct{}

func (p *serviceProgram) Execute(_ []string, req <-chan svc.ChangeRequest, status chan<- svc.Status) (bool, uint32) {
	const accepted = svc.AcceptStop | svc.AcceptShutdown
	status <- svc.Status{State: svc.StartPending}

	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		if err := runCore(stop); err != nil {
			log.Printf("服务核心循环退出异常: %v", err)
		}
	}()

	status <- svc.Status{State: svc.Running, Accepts: accepted}
	for {
		select {
		case c := <-req:
			switch c.Cmd {
			case svc.Interrogate:
				status <- c.CurrentStatus
			case svc.Stop, svc.Shutdown:
				status <- svc.Status{State: svc.StopPending}
				close(stop)
				<-done
				status <- svc.Status{State: svc.Stopped}
				return false, 0
			default:
			}
		case <-done:
			status <- svc.Status{State: svc.Stopped}
			return false, 0
		}
	}
}

func initLogger() error {
	exePath, err := os.Executable()
	if err != nil {
		return fmt.Errorf("获取可执行文件路径失败: %w", err)
	}
	exePath, _ = filepath.EvalSymlinks(exePath)
	logPath := filepath.Join(filepath.Dir(exePath), "timekeeper.log")
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		return fmt.Errorf("打开日志文件失败: %w", err)
	}
	log.SetOutput(logFile)
	log.SetFlags(log.LstdFlags | log.Lmicroseconds)
	log.Printf("日志初始化完成: %s", logPath)
	return nil
}

func installService(exePath string) error {
	m, err := mgr.Connect()
	if err != nil {
		return fmt.Errorf("连接服务管理器失败: %w", err)
	}
	defer m.Disconnect()

	if s, err := m.OpenService(serviceName); err == nil {
		s.Close()
		return fmt.Errorf("服务 %s 已存在", serviceName)
	}

	cfg := mgr.Config{
		DisplayName: serviceName,
		StartType:   mgr.StartAutomatic,
		Description: "通过 NTP 获取标准时间并每秒校正系统 UTC，用于缓解硬件时钟停滞或走时异常。By coderjia@qq.com",
	}

	s, err := m.CreateService(serviceName, exePath, cfg)
	if err != nil {
		return fmt.Errorf("创建服务失败: %w", err)
	}
	defer s.Close()

	log.Printf("服务安装成功: %s", serviceName)
	return nil
}

func removeService() error {
	m, err := mgr.Connect()
	if err != nil {
		return fmt.Errorf("连接服务管理器失败: %w", err)
	}
	defer m.Disconnect()

	s, err := m.OpenService(serviceName)
	if err != nil {
		return fmt.Errorf("打开服务失败: %w", err)
	}
	defer s.Close()

	status, err := s.Query()
	if err == nil && status.State == svc.Running {
		_, _ = s.Control(svc.Stop)
		time.Sleep(2 * time.Second)
	}

	if err := s.Delete(); err != nil {
		return fmt.Errorf("删除服务失败: %w", err)
	}
	log.Printf("服务卸载成功: %s", serviceName)
	return nil
}

func serviceExists() (bool, error) {
	m, err := mgr.Connect()
	if err != nil {
		return false, err
	}
	defer m.Disconnect()

	s, err := m.OpenService(serviceName)
	if err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "does not exist") {
			return false, nil
		}
		if errors.Is(err, windows.ERROR_SERVICE_DOES_NOT_EXIST) {
			return false, nil
		}
		return false, fmt.Errorf("打开服务失败: %w", err)
	}
	s.Close()
	return true, nil
}

func startServiceIfStopped() error {
	m, err := mgr.Connect()
	if err != nil {
		return err
	}
	defer m.Disconnect()
	s, err := m.OpenService(serviceName)
	if err != nil {
		return err
	}
	defer s.Close()
	st, err := s.Query()
	if err == nil && st.State == svc.Running {
		return nil
	}
	return s.Start()
}

func isAdmin() bool {
	adminSid, err := windows.CreateWellKnownSid(windows.WinBuiltinAdministratorsSid)
	if err != nil {
		return false
	}
	token := windows.Token(0)
	member, err := token.IsMember(adminSid)
	return err == nil && member
}

func waitAnyKeyAndExit() {
	fmt.Println("请按任意键退出...")
	_, _ = bufio.NewReader(os.Stdin).ReadByte()
}

func handleInstall() error {
	exePath, err := os.Executable()
	if err != nil {
		return err
	}
	if err := installService(exePath); err != nil {
		return err
	}
	if err := startServiceIfStopped(); err != nil {
		log.Printf("服务已安装，但启动失败: %v", err)
		return nil
	}
	log.Println("服务已安装并启动")
	return nil
}

func runInteractive() error {
	exists, err := serviceExists()
	if err != nil {
		return fmt.Errorf("检查服务状态失败: %w", err)
	}
	if !exists {
		if !isAdmin() {
			fmt.Println("请以管理员身份运行进行安装")
			waitAnyKeyAndExit()
			return nil
		}
		log.Println("检测到未安装服务，开始自动安装")
		if err := handleInstall(); err != nil {
			return err
		}
		fmt.Println("服务安装并启动完成。")
		return nil
	}

	if err := startServiceIfStopped(); err != nil {
		log.Printf("服务已存在，但启动失败: %v", err)
		fmt.Printf("服务已存在，但启动失败: %v\n", err)
		waitAnyKeyAndExit()
		return nil
	}
	fmt.Println("服务已存在并处于运行状态。")
	return nil
}

func main() {
	if err := initLogger(); err != nil {
		fmt.Printf("日志初始化失败: %v\n", err)
		return
	}

	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "-install":
			if err := handleInstall(); err != nil {
				log.Printf("安装失败: %v", err)
				fmt.Printf("安装失败: %v\n", err)
				return
			}
			fmt.Println("安装成功。")
			return
		case "-remove":
			if err := removeService(); err != nil {
				log.Printf("卸载失败: %v", err)
				fmt.Printf("卸载失败: %v\n", err)
				return
			}
			fmt.Println("卸载成功。")
			return
		default:
			fmt.Println("支持参数: -install | -remove")
			return
		}
	}

	isSvc, err := svc.IsWindowsService()
	if err != nil {
		log.Printf("判断服务环境失败: %v", err)
		fmt.Printf("判断服务环境失败: %v\n", err)
		return
	}
	if isSvc {
		log.Printf("以 Windows Service 模式运行: %s", serviceName)
		if err := svc.Run(serviceName, &serviceProgram{}); err != nil {
			log.Printf("服务运行失败: %v", err)
		}
		return
	}

	log.Println("以交互模式运行")
	if err := runInteractive(); err != nil {
		log.Printf("交互模式执行失败: %v", err)
		fmt.Printf("执行失败: %v\n", err)
	}
}
