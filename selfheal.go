package main

import (
	"errors"
	"fmt"
	"log"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/wanvfx/luci-app-model-gateway/config"
	"github.com/wanvfx/luci-app-model-gateway/storage"
)

// ============================================================================
// A5：goroutine panic 恢复
// ============================================================================

// safeGo 启动一个带 panic 恢复的后台 goroutine。
// 任一后台协程 panic 时不再拖垮整个守护进程，而是记录日志后按退避重启该协程。
// 这是软路由场景的刚需：设备常年不重启，一次偶发 panic 若杀掉进程，
// 用户侧表现为「网关突然没了」，且 procd 拉起后状态（巡检评分等）全部丢失。
//
// restart=false 时只恢复不重启（适用于一次性任务）。
func safeGo(name string, restart bool, fn func()) {
	go func() {
		backoff := time.Second
		for {
			crashed := runGuarded(name, fn)
			if !crashed || !restart {
				return
			}
			log.Printf("[selfheal] goroutine %s crashed, restarting in %v", name, backoff)
			time.Sleep(backoff)
			// 指数退避，上限 60s，避免持续 panic 时刷爆日志/占满 CPU
			if backoff < 60*time.Second {
				backoff *= 2
				if backoff > 60*time.Second {
					backoff = 60 * time.Second
				}
			}
		}
	}()
}

// runGuarded 执行 fn 并捕获 panic，返回是否发生了 panic。
func runGuarded(name string, fn func()) (crashed bool) {
	defer func() {
		if rec := recover(); rec != nil {
			crashed = true
			log.Printf("[selfheal] PANIC in goroutine %s: %v", name, rec)
		}
	}()
	fn()
	return false
}

// ============================================================================
// A4：配置损坏自动回滚
// ============================================================================

// loadConfigResilient 加载配置；若主配置损坏/缺失，依次尝试从备份还原。
// 候选备份按优先级：<cfg>.preset_bak（一键配置快照）→ <cfg>.bak（通用备份）→ <cfg>.autobak（本函数维护的滚动快照）。
// 任一备份能成功解析即写回主配置并使用之，避免「配置文件被写坏 → 守护进程 Fatalf 退出 → 面板整个打不开」。
//
// 成功加载后会刷新 <cfg>.autobak，保证下次损坏时有一份「上次可用」的快照可回滚。
func loadConfigResilient(cfgPath string) (*config.Config, error) {
	cfg, err := config.Load(cfgPath)
	if err == nil {
		refreshAutoBackup(cfgPath)
		return cfg, nil
	}
	log.Printf("[selfheal] 主配置加载失败(%v)，尝试从备份自动回滚", err)

	candidates := []string{
		cfgPath + ".preset_bak",
		cfgPath + ".bak",
		cfgPath + ".autobak",
	}
	for _, bak := range candidates {
		data, rerr := os.ReadFile(bak)
		if rerr != nil || len(data) == 0 {
			continue
		}
		// 先落到临时文件校验能否解析，避免把另一份坏文件覆盖上去
		tmp := cfgPath + ".restore_probe"
		if werr := os.WriteFile(tmp, data, 0644); werr != nil {
			continue
		}
		probeCfg, perr := config.Load(tmp)
		_ = os.Remove(tmp)
		if perr != nil || probeCfg == nil {
			log.Printf("[selfheal] 备份 %s 同样无法解析，跳过", bak)
			continue
		}
		if werr := os.WriteFile(cfgPath, data, 0644); werr != nil {
			log.Printf("[selfheal] 回滚写入失败: %v", werr)
			continue
		}
		restored, lerr := config.Load(cfgPath)
		if lerr != nil {
			continue
		}
		log.Printf("[selfheal] 已从备份 %s 自动回滚配置，服务继续启动", bak)
		return restored, nil
	}
	return nil, fmt.Errorf("配置损坏且所有备份均不可用: %w", err)
}

// refreshAutoBackup 在配置成功加载后刷新一份滚动快照 <cfg>.autobak。
// 只在内容确实变化时才写盘，避免每次启动都做无谓 IO（软路由多为 flash，写入寿命有限）。
func refreshAutoBackup(cfgPath string) {
	cur, err := os.ReadFile(cfgPath)
	if err != nil || len(cur) == 0 {
		return
	}
	bak := cfgPath + ".autobak"
	if old, err := os.ReadFile(bak); err == nil && string(old) == string(cur) {
		return
	}
	if err := os.WriteFile(bak, cur, 0644); err != nil {
		log.Printf("[selfheal] 写入配置滚动快照失败: %v", err)
	}
}

// ============================================================================
// A12：端口冲突自动重试
// ============================================================================

// listenWithRetry 在 host:port 上建立监听；端口被占用时自动 +1 递增探测。
// 返回实际监听器与最终生效的端口号。
// 软路由上端口冲突很常见（用户装了别的服务占了 12211），原先直接 Fatalf 退出，
// 用户只能看到「服务启动不了」却不知道原因；现在自动挪到下一个可用端口并回写 UCI。
func listenWithRetry(host string, port, maxTries int) (net.Listener, int, error) {
	if maxTries < 1 {
		maxTries = 1
	}
	var lastErr error
	for i := 0; i < maxTries; i++ {
		p := port + i
		if p > 65535 {
			break
		}
		addr := net.JoinHostPort(host, fmt.Sprint(p))
		ln, err := net.Listen("tcp", addr)
		if err == nil {
			if i > 0 {
				log.Printf("[selfheal] 端口 %d 被占用，已自动切换到 %d", port, p)
			}
			return ln, p, nil
		}
		if !isAddrInUse(err) {
			// 非「端口占用」错误（如权限不足、地址非法）没必要继续递增试
			return nil, 0, err
		}
		lastErr = err
	}
	if lastErr == nil {
		lastErr = errors.New("no available port")
	}
	return nil, 0, lastErr
}

// isAddrInUse 判断错误是否为「地址已被占用」。
// 用字符串匹配而非 syscall.EADDRINUSE，是为了跨平台编译（本机 Windows 交叉编译到 Linux）。
func isAddrInUse(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "address already in use") ||
		strings.Contains(msg, "only one usage of each socket address") ||
		strings.Contains(msg, "已被占用")
}

// ============================================================================
// A13：数据目录超限自动裁剪
// ============================================================================

// diskGuardConfig 数据目录容量看护参数
type diskGuardConfig struct {
	Dir       string        // 数据目录
	MaxBytes  int64         // 触发裁剪的总容量阈值
	Interval  time.Duration // 检查周期
	KeepRatio float64       // 裁剪后目标占用比例（相对 MaxBytes）
}

// defaultDiskGuard 返回默认看护参数：64MB 上限、10 分钟一检、裁到 70%。
// 阈值按「数据目录总大小」而非磁盘剩余空间来判定，原因有二：
//  1. 跨平台可编译（statfs 在 Windows 下不可用，本项目在 Windows 上交叉编译）；
//  2. 软路由的 overlay 分区常常只有几十上百 MB，限制本应用自身占用比盯着全盘更精准也更礼貌。
func defaultDiskGuard(dir string) diskGuardConfig {
	return diskGuardConfig{
		Dir:       dir,
		MaxBytes:  64 << 20,
		Interval:  10 * time.Minute,
		KeepRatio: 0.7,
	}
}

// runDiskGuard 周期性检查数据目录占用，超阈值时按优先级裁剪可再生文件。
// 裁剪顺序（越靠前越先删，都是可再生/低价值数据）：
//
//  1. history.N.jsonl / usage.N.jsonl 轮转副本（最旧优先）
//  2. calllog.ndjson（调用日志，仅用于展示与日报）
//  3. cache/ 目录下的响应缓存
//
// 绝不动 vault.key / vkeys.json / budget.json / templates.json 等不可再生的关键数据。
func runDiskGuard(cfg diskGuardConfig, stop <-chan struct{}) {
	ticker := time.NewTicker(cfg.Interval)
	defer ticker.Stop()
	for {
		select {
		case <-stop:
			return
		case <-ticker.C:
			if freed, total := trimDataDir(cfg); freed > 0 {
				log.Printf("[selfheal] 数据目录超过 %dMB，已裁剪 %dKB（裁剪前 %dKB）",
					cfg.MaxBytes>>20, freed>>10, total>>10)
			}
		}
	}
}

// trimDataDir 执行一次裁剪，返回释放的字节数与裁剪前总占用。
func trimDataDir(cfg diskGuardConfig) (freed int64, total int64) {
	total = dirSize(cfg.Dir)
	if total <= cfg.MaxBytes {
		return 0, total
	}
	target := int64(float64(cfg.MaxBytes) * cfg.KeepRatio)
	cur := total

	// 第一梯队：轮转副本（history.1.jsonl、usage.2.jsonl 之类），按序号从大到小（越大越旧）删
	rotated := listRotatedFiles(cfg.Dir)
	for _, f := range rotated {
		if cur <= target {
			return freed, total
		}
		if sz, err := fileSize(f); err == nil {
			if os.Remove(f) == nil {
				cur -= sz
				freed += sz
			}
		}
	}

	// 第二梯队：调用日志落盘文件（可再生，只影响历史展示）
	for _, name := range []string{"calllog.ndjson", "calllog.1.ndjson"} {
		if cur <= target {
			return freed, total
		}
		p := filepath.Join(cfg.Dir, name)
		if sz, err := fileSize(p); err == nil {
			if os.Remove(p) == nil {
				cur -= sz
				freed += sz
			}
		}
	}

	// 第三梯队：响应缓存（纯加速用途，删了只是下次不命中）
	if cur > target {
		cacheDir := filepath.Join(cfg.Dir, "cache")
		if sz := dirSize(cacheDir); sz > 0 {
			if os.RemoveAll(cacheDir) == nil {
				cur -= sz
				freed += sz
				_ = os.MkdirAll(cacheDir, 0755)
			}
		}
	}
	return freed, total
}

// listRotatedFiles 返回数据目录下的轮转副本文件，按「越旧越靠前」排序。
func listRotatedFiles(dir string) []string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		n := e.Name()
		// 形如 history.1.jsonl / usage.3.jsonl
		if (strings.HasPrefix(n, "history.") || strings.HasPrefix(n, "usage.")) &&
			strings.HasSuffix(n, ".jsonl") && strings.Count(n, ".") >= 2 {
			out = append(out, filepath.Join(dir, n))
		}
	}
	// 名称降序 ≈ 序号大的（更旧的）在前
	sort.Sort(sort.Reverse(sort.StringSlice(out)))
	return out
}

// dirSize 递归统计目录占用字节数（目录不存在返回 0）。
func dirSize(dir string) int64 {
	var total int64
	_ = filepath.Walk(dir, func(_ string, info os.FileInfo, err error) error {
		if err != nil || info == nil || info.IsDir() {
			return nil
		}
		total += info.Size()
		return nil
	})
	return total
}

// fileSize 返回文件大小。
func fileSize(path string) (int64, error) {
	st, err := os.Stat(path)
	if err != nil {
		return 0, err
	}
	return st.Size(), nil
}

// ============================================================================
// A11：每日用量日报
// ============================================================================

// runDailyReport 每天定时聚合昨日用量，生成简单文本报告，并写入报告文件。
// 失败仅记录日志，不阻塞主流程。
func runDailyReport(stop <-chan struct{}, usage *storage.Usage, dataDir string) {
	// 等待到次日 00:05 再出第一份日报
	waitNextDay := func() {
		now := time.Now()
		next := time.Date(now.Year(), now.Month(), now.Day()+1, 0, 5, 0, 0, now.Location())
		if next.Before(now) {
			next = next.Add(24 * time.Hour)
		}
		select {
		case <-time.After(time.Until(next)):
		case <-stop:
			return
		}
	}

	for {
		waitNextDay()
		select {
		case <-stop:
			return
		default:
		}

		// 聚合昨日用量
		yesterday := time.Now().AddDate(0, 0, -1)
		records, err := usage.Read(1)
		if err != nil {
			log.Printf("[daily-report] read usage failed: %v", err)
			continue
		}
		var totalTokens int
		var providers map[string]int
		var models map[string]int
		providers = map[string]int{}
		models = map[string]int{}
		for _, r := range records {
			if r.Time.Before(yesterday) || r.Time.After(yesterday.AddDate(0, 0, 1)) {
				continue
			}
			totalTokens += r.TotalTokens
			providers[r.Provider]++
			models[r.Model]++
		}

		_ = totalTokens
		_ = providers
		_ = models

		// 组装简易文本报告
		report := fmt.Sprintf("每日用量报告 (%s)\n", yesterday.Format("2006-01-02"))
		report += fmt.Sprintf("总请求: %d\n", len(records))
		report += fmt.Sprintf("总 Tokens: %d\n", totalTokens)

		// 写入日报文件（可被外部采集）
		reportPath := filepath.Join(dataDir, "daily-report.txt")
		if err := os.WriteFile(reportPath, []byte(report), 0644); err != nil {
			log.Printf("[daily-report] write report failed: %v", err)
		}

		log.Printf("[daily-report] 日报已生成 (%s): %d 请求, %d tokens", yesterday.Format("2006-01-02"), len(records), totalTokens)
	}
}
