package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
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
	"github.com/wanvfx/luci-app-model-gateway/config"
	"github.com/wanvfx/luci-app-model-gateway/engine"
	"github.com/wanvfx/luci-app-model-gateway/proxy"
	"github.com/wanvfx/luci-app-model-gateway/storage"
)

var (
	cfgMu     sync.RWMutex
	globalCfg *config.Config
)

// generateAdminKey 生成随机 admin_key（格式：sk-local- + 32 位十六进制）
func generateAdminKey() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
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

// uciSetOption 调用 uci 工具写入配置
func uciSetOption(sectionType, sectionName, key, value string) {
	// 构造 uci set 命令
	args := []string{"set"}
	if sectionName != "" {
		args = append(args, fmt.Sprintf("%s.%s.%s=%s", sectionType, sectionName, key, value))
	} else {
		args = append(args, fmt.Sprintf("%s.@%s[0].%s=%s", sectionType, sectionType, key, value))
	}
	cmd := exec.Command("uci", args...)
	if out, err := cmd.CombinedOutput(); err != nil {
		log.Printf("uci set failed: %v, output: %s", err, string(out))
		return
	}
	// commit
	cmd = exec.Command("uci", "commit", sectionType)
	if out, err := cmd.CombinedOutput(); err != nil {
		log.Printf("uci commit failed: %v, output: %s", err, string(out))
	}
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

	// 一键配置断电恢复：若上次预设应用被强杀/断电，残留 .preset_bak，
	// 启动时自动还原并删除备份（与 Python 原版 1.6.1 _recover_preset_backup 一致）。
	recoverPresetBackup(*cfgPath)

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		log.Fatalf("load config failed: %v", err)
	}
	globalCfg = cfg

	// 密钥保险库：UCI 中 api_key 以 enc: 前缀密文存储，启动时解密到内存
	// （vault 主密钥落 MODEL_GATEWAY_DATA/vault.key，符合 iStoreOS 可写数据铁律）
	vault := storage.NewVault(dataDir)
	cfg.DecryptAPIKeys(vault.Decrypt)

	// 自动生成 admin_key（首次启动、placeholder，或旧的无前缀格式时）
	if cfg.AdminKey() == "" || cfg.AdminKey() == "AUTO_GENERATED_ON_FIRST_BOOT" || !strings.HasPrefix(cfg.AdminKey(), "sk-local-") {
		newKey := generateAdminKey()
		log.Printf("auto-generating admin_key: %s", maskKey(newKey))
		// 写入 UCI（section type=model-gateway, name=settings）
		uciSetOption("model-gateway", "settings", "admin_key", newKey)
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

	// 初始化熔断器池（3 次失败 / 60 秒恢复，与 Python 原版一致）
	scorer := engine.NewScorer()
	circuits := engine.NewCircuitPool(3, 60*time.Second)

	srv := proxy.New(cfg, dataDir, usage, circuits, scorer)

	// 注册管理面 API（admin_key 鉴权）
	appDir, _ := os.Getwd()
	if envApp := os.Getenv("MODEL_GATEWAY_APP"); envApp != "" {
		appDir = envApp
	}
	if ex, err := os.Executable(); err == nil {
		appDir = filepath.Dir(ex)
	}

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
	go priceSync.AutoSyncLoop(ctx.Done())

	go func() {
		addr := cfg.BindAddr()
		log.Printf("model-gatewayd starting on %s", addr)
		if err := srv.ListenAndServe(); err != nil {
			log.Fatalf("server exited: %v", err)
		}
	}()

	// 启动后台真实巡检（每轮读取最新配置，覆盖一键配置热更新新增的 provider；
	// getStrategy 每轮读取最新策略，支持运行时热切换省电/持续模式）
	go poller.Start(ctx, func() []*config.Provider {
		cfgMu.RLock()
		defer cfgMu.RUnlock()
		if globalCfg != nil {
			return globalCfg.Providers
		}
		return cfg.Providers
	}, func() string {
		cfgMu.RLock()
		defer cfgMu.RUnlock()
		if globalCfg != nil {
			return globalCfg.PollStrategy()
		}
		return cfg.PollStrategy()
	})

	// 预拉取模型详情缓存（启动时 + 每 5 分钟）
	go func() {
		ticker := time.NewTicker(5 * time.Minute)
		defer ticker.Stop()
		srv.ModelCacheFetch()
		for range ticker.C {
			srv.ModelCacheFetch()
		}
	}()

	// 定期清理过期用量记录（保留 30 天）
	go func() {
		ticker := time.NewTicker(5 * time.Minute)
		defer ticker.Stop()
		for range ticker.C {
			_, _ = usage.Cleanup(30 * 24 * time.Hour)
		}
	}()

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
