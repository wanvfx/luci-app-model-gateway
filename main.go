package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/wanvfx/luci-app-model-gateway/api"
	"github.com/wanvfx/luci-app-model-gateway/calllog"
	"github.com/wanvfx/luci-app-model-gateway/config"
	"github.com/wanvfx/luci-app-model-gateway/engine"
	"github.com/wanvfx/luci-app-model-gateway/proxy"
	"github.com/wanvfx/luci-app-model-gateway/proxy/translator"
	"github.com/wanvfx/luci-app-model-gateway/storage"
)

var (
	cfgMu        sync.RWMutex
	globalCfg    *config.Config
	appDirGlobal string
)

// generateAdminKey 生成随机 admin_key（格式：sk-local- + 32 位十六进制）。
// P3-3：crypto/rand 失败即 Fatal（熵源损坏属致命），绝不退化为全零/可预测弱密钥。
func generateAdminKey() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		log.Fatalf("crypto/rand failed to generate admin_key: %v", err)
	}
	return "sk-local-" + hex.EncodeToString(b)
}

// maskKey 脱敏显示
func maskKey(key string) string {
	if len(key) <= 12 {
		return "****"
	}
	return key[:6] + "****" + key[len(key)-4:]
}

// pollHTTPClient 创建绕过本地/内网代理的 HTTP 客户端（巡检用，复用 api 包统一实现）
func pollHTTPClient() *http.Client {
	return api.NewBypassClient(15 * time.Second)
}

// escapeUCIValue 对 UCI 值做必要转义，避免换行/单引号 breakout batch 命令（P2-1 / P2-3）。
// 与 api/uci.go 的 escapeUCIValue 保持同一实现，保证 unescape 严格互逆：
// 反斜杠→\\，单引号→'\”，真实换行/回车→\n / \r 字面。
func escapeUCIValue(v string) string {
	v = strings.ReplaceAll(v, `\`, `\\`)
	v = strings.ReplaceAll(v, "'", `'\''`)
	v = strings.ReplaceAll(v, "\n", `\n`)
	v = strings.ReplaceAll(v, "\r", `\r`)
	return v
}

// uciSetOption 调用 uci 工具写入配置（P3-4: 用 uci batch stdin 传递敏感值，避免进程列表泄露）。
// 返回 error（P2-4）：调用方可感知 UCI 写失败，避免「内存已更新但 UCI 未持久化」导致重启后 key 漂移。
func uciSetOption(sectionType, sectionName, key, value string) error {
	// 构造 uci batch 命令体（通过 stdin 传递，敏感值不在 args 中可见）
	var batch strings.Builder
	if sectionName != "" {
		batch.WriteString(fmt.Sprintf("set %s.%s\n", sectionType, sectionName))
		batch.WriteString(fmt.Sprintf("set %s.%s.%s=%s\n", sectionType, sectionName, key, escapeUCIValue(value)))
	} else {
		// 匿名段：uci batch 也支持 set configName.@type[index].key=value 语法
		batch.WriteString(fmt.Sprintf("set %s.@%s[0].%s=%s\n", sectionType, sectionType, key, escapeUCIValue(value)))
	}
	cmd := exec.Command("uci", "batch")
	cmd.Stdin = strings.NewReader(batch.String())
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("uci batch set failed: %v, output: %s", err, string(out))
	}
	// commit
	cmd = exec.Command("uci", "commit", sectionType)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("uci commit failed: %v, output: %s", err, string(out))
	}
	return nil
}

func main() {
	cfgPath := flag.String("c", "/etc/config/model-gateway", "UCI config path")
	flag.Parse()

	if env := os.Getenv("MODEL_GATEWAY_CONFIG"); env != "" {
		cfgPath = &env
	}

	dataDir := os.Getenv("MODEL_GATEWAY_DATA")
	if dataDir == "" {
		dataDir = "/var/lib/model-gateway"
	}
	os.MkdirAll(dataDir, 0755)

	// 提前确定应用目录（用于定位随包目录 providers_catalog.json），
	// 以便“保留配置升级”时为旧免 Key 实例补全 format（协议适配器自愈）。
	appDirGlobal, _ = os.Getwd()
	if envApp := os.Getenv("MODEL_GATEWAY_APP"); envApp != "" {
		appDirGlobal = envApp
	}
	if ex, err := os.Executable(); err == nil {
		appDirGlobal = filepath.Dir(ex)
	}

	// 通用协议适配器（Phase C）：加载用户自定义/覆盖的协议规格。
	// 内置规格已在 translator 包 init 注册；这里让用户能用一份 JSON 修正上游改版
	// 或新增任意非 OpenAI 兼容提供者，无需等新版本。文件不存在属正常情况，静默跳过。
	if n, err := translator.LoadAdaptersFile(filepath.Join(dataDir, "adapters.json")); err != nil {
		log.Printf("load custom adapters failed: %v", err)
	} else if n > 0 {
		log.Printf("loaded %d custom protocol adapter(s) from adapters.json", n)
	}

	// 一键配置断电恢复：若上次预设应用被强杀/断电，残留 .preset_bak，
	// 启动时自动还原并删除备份（与 Python 原版 1.6.1 _recover_preset_backup 一致）。
	recoverPresetBackup(*cfgPath)

	// A4 配置自愈：主配置损坏时自动从 .preset_bak/.bak/.autobak 逐级回滚，
	// 而不是直接 Fatalf 退出（那会让用户面对一个「服务起不来且不知为何」的黑盒）。
	cfg, err := loadConfigResilient(*cfgPath)
	if err != nil {
		log.Fatalf("load config failed: %v", err)
	}
	globalCfg = cfg

	// 协议适配器自愈：免 Key 实例在旧版（无适配器概念）创建时 format 为空，
	// 重装保留配置后默认回退 openai，导致 duckduckgo/felo/theoldllm 等适配器不被触发。
	// 从随包目录按 id（即实例 Name）取回非 openai 的 format 写入内存，使“保留配置”也能自动启用适配器。
	healKeyfreeAdapterFormats(cfg, appDirGlobal)

	// 密钥保险库：UCI 中 api_key 以 enc: 前缀密文存储，启动时解密到内存
	// （vault 主密钥落 MODEL_GATEWAY_DATA/vault.key，符合 iStoreOS 可写数据铁律）
	vault := storage.NewVault(dataDir)
	cfg.DecryptAPIKeys(vault.Decrypt)

	// 自动生成 admin_key（首次启动、placeholder，或旧的无前缀格式时）
	if cfg.AdminKey() == "" || cfg.AdminKey() == "AUTO_GENERATED_ON_FIRST_BOOT" || !strings.HasPrefix(cfg.AdminKey(), "sk-local-") {
		newKey := generateAdminKey()
		log.Printf("auto-generating admin_key: %s", maskKey(newKey))
		// 写入 UCI（section type=model-gateway, name=settings）。
		// P2-4：若 UCI 写失败，说明 admin_key 无法持久化，直接 fatal——
		// 否则内存虽已 SetAdminKey、重启后 UCI 仍是旧值，导致 key 漂移、用户旧 key 全部失效的隐蔽故障。
		if err := uciSetOption("model-gateway", "settings", "admin_key", newKey); err != nil {
			log.Fatalf("persist generated admin_key to UCI failed: %v", err)
		}
		// 同步更新内存中的 key，否则守护进程 /api/config 仍返回旧 key，
		// 造成 LuCI 显示新 key、监控面板显示旧 key 的不一致
		cfg.SetAdminKey(newKey)
	}

	log.Printf("config loaded: path=%s port=%d providers=%d routers=%d", *cfgPath, cfg.Port(), len(cfg.Providers), len(cfg.Routers))

	history, err := storage.NewHistory(dataDir)
	if err != nil {
		log.Fatalf("init history storage failed: %v", err)
	}
	defer history.Close()

	usage, err := storage.NewUsage(dataDir)
	if err != nil {
		log.Fatalf("init usage storage failed: %v", err)
	}
	defer usage.Close()

	// A10：调用日志落盘
	calllog.InitFileStore(dataDir)
	defer calllog.CloseFileStore()

	// A17：webhook 死信队列落盘
	proxy.InitDeadLetterQueue(dataDir)

	// 初始化熔断器池（3 次失败 / 60 秒恢复，与 Python 原版一致）
	scorer := engine.NewScorer()
	circuits := engine.NewCircuitPool(3, 60*time.Second)

	srv := proxy.New(cfg, dataDir, usage, circuits, scorer)

	// 注册管理面 API（admin_key 鉴权）
	appDir := appDirGlobal

	// reloadConfig 闭包：重载配置后同步更新 proxy.Server 与 adminHandler，
	// 两者均使用 atomic.Pointer 持有配置，彻底消除并发读写 data race 与两份配置偶发不同步（修复 #2）
	var adminHandler *api.AdminHandler
	reloadFn := func() (*config.Config, error) {
		newCfg, err := doReloadConfig(*cfgPath, vault.Decrypt)
		if err != nil {
			return nil, err
		}
		srv.UpdateConfig(newCfg)
		if adminHandler != nil {
			adminHandler.SetConfig(newCfg)
		}
		return newCfg, nil
	}

	adminHandler = api.NewAdminHandler(cfg, history, usage, dataDir, appDir, *cfgPath, reloadFn, srv.Meta(), &modelCacheAdapter{cache: srv.ModelDetailCache()})
	// 注入缓存/预算运行时（三缺口端点：/api/cache、/api/budget-status）
	adminHandler.SetGatewayRuntime(srv.ResponseCache(), srv.Budget())
	// 注入密钥保险库（批量加密 api_key）
	adminHandler.SetVault(vault)
	// 注入熔断器池（C4 锁定状态查询，/api/check/all 显示 🔒）
	adminHandler.SetCircuits(circuits)
	srv.RegisterAdminRoutes(adminHandler)

	// 虚拟密钥（子密钥）存储：落 MODEL_GATE_DATA/vkeys.json，供 /api/vkeys 与 Bearer 鉴权
	vkeyStore := storage.NewVKeyStore(dataDir)
	adminHandler.SetVKeyStore(vkeyStore)
	srv.SetVKeyStore(vkeyStore)

	// 模型价格自动同步器：落 MODEL_GATEWAY_DATA/models_catalog_sync.json 覆盖层，
	// 供 /api/price-sync 手动触发（后台循环在 ctx 定义后启动）
	priceSync := engine.NewPriceSync(appDir, dataDir, srv.Catalog(), pollHTTPClient())
	adminHandler.SetPriceSync(priceSync)
	// 成本/用量仪表盘：注入模型参考库（价格来源）
	adminHandler.SetCatalog(srv.Catalog())

	// 初始化巡检器
	poller := engine.NewPoller(time.Duration(cfg.PollInterval())*time.Second, scorer, history, pollHTTPClient(), circuits)
	// 注入别名解析：巡检逐模型探测时把别名解析为标准模型名（对齐 Python normalize_model）
	poller.SetResolver(srv.Meta().ResolveModel)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// 后台价格自动同步：每 24h 检查（覆盖层超 7 天未更新才真拉取），首次延迟 5 分钟
	// A15：是否启用/间隔由 UCI price_sync_enabled / price_sync_interval 控制
	priceSync.SetEnabled(cfg.PriceSyncEnabled())
	priceSync.SetInterval(time.Duration(cfg.PriceSyncInterval()) * time.Hour)
	safeGo("price-sync", true, func() { priceSync.AutoSyncLoop(ctx.Done()) })

	// B 方案：免费模型自动巡检，首次延迟 5 分钟
	freeModelGuard := engine.NewFreeModelGuard(
		func() []*config.Provider {
			cfgMu.RLock()
			defer cfgMu.RUnlock()
			if globalCfg != nil {
				return globalCfg.Providers
			}
			return cfg.Providers
		},
		func(name string, models []string) error {
			return adminHandler.UpdateProviderModels(name, models)
		},
		func(baseURL, apiKey, authHeader, authScheme string, freeOnly bool) ([]string, error) {
			return api.FetchModelsFromUpstream(baseURL, apiKey, authHeader, authScheme, freeOnly, nil)
		},
		pollHTTPClient(),
		log.Printf,
	)
	adminHandler.SetFreeModelGuard(freeModelGuard)
	freeModelGuard.SetEnabled(cfg.FreeModelGuardEnabled())
	freeModelGuard.SetInterval(cfg.FreeModelGuardInterval())
	safeGo("free-model-guard", true, func() { freeModelGuard.AutoGuardLoop(ctx.Done()) })

	// A12 端口冲突自愈：先自行建立监听器，被占用时自动 +1 递增探测（最多 10 次），
	// 成功后把实际端口回写 UCI，让 LuCI 面板与实际监听保持一致。
	listenHost := cfg.BindHost()
	ln, actualPort, lerr := listenWithRetry(listenHost, cfg.Port(), 10)
	if lerr != nil {
		log.Fatalf("listen failed on %s:%d: %v", listenHost, cfg.Port(), lerr)
	}
	if actualPort != cfg.Port() {
		log.Printf("[selfheal] 监听端口由 %d 自动调整为 %d，正在同步写回 UCI", cfg.Port(), actualPort)
		// P2-4：port 为自愈调整的次要持久化，失败仅告警不回滚（运行期用实际端口即可，
		// 重启后会再次自动调整重建），但与 admin_key 的分支不同——此处不 fatal。
		if err := uciSetOption("model-gateway", "settings", "port", fmt.Sprint(actualPort)); err != nil {
			log.Printf("[selfheal] 同步端口到 UCI 失败（非致命，重启将自动重试）: %v", err)
		}
		cfg.SetPort(actualPort)
	}
	log.Printf("model-gatewayd starting on %s", ln.Addr().String())
	safeGo("http-server", false, func() {
		if err := srv.Serve(ln); err != nil && err != http.ErrServerClosed {
			log.Fatalf("server exited: %v", err)
		}
	})

	// 启动后台真实巡检（每轮读取最新配置，覆盖一键配置热更新新增的 provider；
	// getStrategy 每轮读取最新策略，支持运行时热切换省电/持续模式）
	safeGo("poller", true, func() {
		poller.Start(ctx, func() []*config.Provider {
			cfgMu.RLock()
			defer cfgMu.RUnlock()
			if globalCfg != nil {
				return globalCfg.UsableProviders()
			}
			return cfg.UsableProviders()
		}, func() string {
			cfgMu.RLock()
			defer cfgMu.RUnlock()
			if globalCfg != nil {
				return globalCfg.PollStrategy()
			}
			return cfg.PollStrategy()
		})
	})

	// 预拉取模型详情缓存（启动时 + 每 5 分钟）
	safeGo("model-cache", true, func() {
		ticker := time.NewTicker(5 * time.Minute)
		defer ticker.Stop()
		srv.ModelCacheFetch()
		for range ticker.C {
			srv.ModelCacheFetch()
		}
	})

	// 定期清理过期用量记录（保留 30 天）
	safeGo("usage-cleanup", true, func() {
		ticker := time.NewTicker(5 * time.Minute)
		defer ticker.Stop()
		for range ticker.C {
			_, _ = usage.Cleanup(30 * 24 * time.Hour)
		}
	})

	// A13 磁盘看护：数据目录超过阈值时自动裁剪可再生文件（轮转日志/调用日志/响应缓存）
	safeGo("disk-guard", true, func() {
		runDiskGuard(defaultDiskGuard(dataDir), ctx.Done())
	})

	// A11 每日用量日报：每天定时聚合昨日用量并写入报告
	safeGo("daily-report", true, func() {
		runDailyReport(ctx.Done(), usage, dataDir)
	})

	// 等待 SIGTERM/SIGINT（procd 发 STOP 信号）
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGTERM, syscall.SIGINT)
	<-quit
	log.Println("shutting down...")

	cancel()
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer shutdownCancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Printf("graceful shutdown error: %v", err)
	}
}

// modelCacheAdapter 将 proxy.ModelCache 适配为 api.ModelCacheInterface
type modelCacheAdapter struct {
	cache interface {
		Get(modelID string) *proxy.ModelDetail
	}
}

func (a *modelCacheAdapter) Get(modelID string) *api.ModelDetailItem {
	d := a.cache.Get(modelID)
	if d == nil {
		return nil
	}
	return &api.ModelDetailItem{
		ID:          d.ID,
		Object:      d.Object,
		Created:     d.Created,
		OwnedBy:     d.OwnedBy,
		ContextLen:  d.ContextLen,
		MaxPosEmb:   d.MaxPosEmb,
		MaxModelLen: d.MaxModelLen,
	}
}

// healKeyfreeAdapterFormats 为“保留配置升级”而来的免 Key 实例补全 format。
// 旧版在创建免 Key 提供者时尚无协议适配器概念，实例 UCI 中 format 为空（默认 openai），
// 导致 duckduckgo/felo/theoldllm 等适配器在重装后不被触发。
// 这里从随包目录 providers_catalog.json 按提供者 id（即实例 Name）取回非 openai 的 format
// 并写入内存中的 provider，使“保留配置”也能自动启用协议适配器，无需用户手动重加。
// 函数幂等，每次配置加载（含热重载）都调用，无副作用；未命中目录或目录缺失则静默跳过。
func healKeyfreeAdapterFormats(cfg *config.Config, appDir string) {
	if cfg == nil {
		return
	}
	candidates := []string{
		filepath.Join(appDir, "providers_catalog.json"),
		filepath.Join(appDir, "..", "share", "model-gateway", "providers_catalog.json"),
	}
	formatByID := map[string]string{}
	for _, p := range candidates {
		data, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		var doc struct {
			Providers []struct {
				ID     string `json:"id"`
				Format string `json:"format"`
			} `json:"providers"`
		}
		if err := json.Unmarshal(data, &doc); err != nil {
			continue
		}
		for _, pr := range doc.Providers {
			f := strings.ToLower(strings.TrimSpace(pr.Format))
			if f != "" && f != "openai" && pr.ID != "" {
				formatByID[pr.ID] = f
			}
		}
		break // 命中第一个存在的文件即可
	}
	if len(formatByID) == 0 {
		return
	}
	n := 0
	for _, p := range cfg.Providers {
		if !p.NoAuth {
			continue
		}
		if strings.TrimSpace(p.Format) != "" {
			continue
		}
		if f, ok := formatByID[p.Name]; ok {
			p.Format = f
			n++
		}
	}
	if n > 0 {
		log.Printf("[selfheal] 为 %d 个免 Key 实例从目录补全 format（协议适配器已启用），可到面板「提供者」页保存一次以固化到配置", n)
	}
}

// doReloadConfig 热重载配置（UCI 变更后调用）
// dec 用于解密 api_key：UCI 中落盘的是 "enc:" 前缀密文，重载到内存时必须解密回明文，
// 否则后台巡检 / 手动检测 / 代理转发都会带着密文密钥去请求上游 → 全部 401 → 模型全红。
// 这是 r20260727d 之前“保存配置或一键配置后所有模型检测变红”的根因（修复）。
func doReloadConfig(cfgPath string, dec func(string) string) (*config.Config, error) {
	if env := os.Getenv("MODEL_GATEWAY_CONFIG"); env != "" {
		cfgPath = env
	}

	newCfg, err := config.Load(cfgPath)
	if err != nil {
		return nil, err
	}

	// 关键：重载后内存中的密钥必须是明文，与启动时一致
	if dec != nil {
		newCfg.DecryptAPIKeys(dec)
	}

	// 重载同样执行协议适配器自愈，确保“保留配置”升级后热重载不会丢失 format。
	healKeyfreeAdapterFormats(newCfg, appDirGlobal)

	cfgMu.Lock()
	globalCfg = newCfg
	cfgMu.Unlock()

	log.Printf("config reloaded: port=%d providers=%d routers=%d", newCfg.Port(), len(newCfg.Providers), len(newCfg.Routers))
	return newCfg, nil
}

// recoverPresetBackup 启动时检查一键配置的残留备份并自动还原（断电保护，修复 #4）。
// 仅当“进行中标记 + 备份”同时存在时才还原——意味着上次一键配置被中断（进程被杀/断电），
// 当前配置是“部分生效”的脏状态，需要用备份回滚到干净快照。
// 若只有备份而无进行中标记（上次已正常完成，或旧版中断但当前配置完好），则丢弃残留备份，
// 避免把已生效的好配置静默回滚到旧快照（这正是“已配置平台突然消失”的根因之一）。
// 仅当“无标记 + 备份 + 当前配置损坏”时才还原（兼容旧版无 sentinel 的中断场景）。
// 备份路径与配置同目录（/etc/config），属持久化存储，重启后仍在，符合 iStoreOS storage-path 规范。
func recoverPresetBackup(cfgPath string) {
	backupPath := cfgPath + ".preset_bak"
	inProgressPath := cfgPath + ".preset_in_progress"
	hasSentinel := func() bool {
		_, err := os.Stat(inProgressPath)
		return err == nil
	}
	hasBackup := func() bool {
		_, err := os.Stat(backupPath)
		return err == nil
	}

	if !hasSentinel() {
		// 无“进行中”标记：上次一键配置已正常完成（或本就没跑过）。
		if !hasBackup() {
			return
		}
		// 残留孤立备份：若当前配置正常则丢弃；若当前配置损坏（兼容旧版无 sentinel 的中断场景）则还原。
		if curData, err := os.ReadFile(cfgPath); err == nil && len(curData) > 0 {
			if _, perr := config.Load(cfgPath); perr == nil {
				_ = os.Remove(backupPath)
				log.Printf("断电恢复：检测到孤立残留备份且当前配置正常，已丢弃过时备份 %s", backupPath)
				return
			}
		}
		// 当前配置缺失/损坏 → 用备份还原（兼容旧版）
		log.Printf("断电恢复：检测到孤立残留备份且当前配置损坏，尝试从备份还原 %s", backupPath)
	} else {
		// 存在“进行中”标记 → 上次被中断（新版本），必须用备份还原
		if !hasBackup() {
			log.Printf("断电恢复：存在进行中标记但备份缺失，无法还原 %s", backupPath)
			_ = os.Remove(inProgressPath)
			return
		}
	}

	// 执行还原（两种情况都到此：新版本中断，或旧版本中断且当前配置损坏）
	data, err := os.ReadFile(backupPath)
	if err != nil {
		log.Printf("preset recovery: read backup failed: %v", err)
		_ = os.Remove(inProgressPath)
		return
	}
	if err := os.WriteFile(cfgPath, data, 0644); err != nil {
		log.Printf("preset recovery: restore failed: %v", err)
		_ = os.Remove(inProgressPath)
		return
	}
	_ = os.Remove(backupPath)
	_ = os.Remove(inProgressPath)
	log.Printf("断电恢复：已从备份还原配置 %s", cfgPath)
}
