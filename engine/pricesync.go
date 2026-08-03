package engine

// pricesync.go 模型价格自动同步：从 models.dev 公开数据库拉取最新单价/上下文/能力，
// 生成 catalog 覆盖层落 MODEL_GATEWAY_DATA/models_catalog_sync.json（可写数据铁律），
// 合并优先级：内置库 < share 随包库 < 同步覆盖层 < 用户手工覆盖（dataDir/models_catalog.json）。
// 网络失败静默回退既有数据，不影响主流程。

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// PriceSyncURL 数据源（models.dev 开源模型数据库，MIT 许可）
const PriceSyncURL = "https://models.dev/api.json"

// syncFileName 覆盖层文件名
const syncFileName = "models_catalog_sync.json"

// PriceSyncStatus 同步状态（供 /api/price-sync GET）
type PriceSyncStatus struct {
	LastSync   string `json:"last_sync"`   // RFC3339，空=从未同步
	ModelCount int    `json:"model_count"` // 覆盖层条目数
	LastError  string `json:"last_error"`
}

// PriceSync 价格同步器
type PriceSync struct {
	mu       sync.Mutex
	appDir   string
	dataDir  string
	catalog  *Catalog
	client   *http.Client
	status   PriceSyncStatus
	enabled  bool
	interval time.Duration
}

// NewPriceSync 创建同步器
func NewPriceSync(appDir, dataDir string, catalog *Catalog, client *http.Client) *PriceSync {
	ps := &PriceSync{appDir: appDir, dataDir: dataDir, catalog: catalog, client: client}
	// 启动时读取既有覆盖层的元信息
	if fi, err := os.Stat(ps.syncPath()); err == nil {
		ps.status.LastSync = fi.ModTime().Format(time.RFC3339)
		if data, err := os.ReadFile(ps.syncPath()); err == nil {
			var f catalogFile
			if json.Unmarshal(data, &f) == nil {
				ps.status.ModelCount = len(f.Models)
			}
		}
	}
	return ps
}

func (ps *PriceSync) syncPath() string {
	return filepath.Join(ps.dataDir, syncFileName)
}

// Status 当前同步状态快照
func (ps *PriceSync) Status() PriceSyncStatus {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	return ps.status
}

// mdModel models.dev 单模型条目（防御性解析，只取需要的字段）
type mdModel struct {
	Cost struct {
		Input  float64 `json:"input"`
		Output float64 `json:"output"`
	} `json:"cost"`
	Limit struct {
		Context int `json:"context"`
	} `json:"limit"`
	Modalities struct {
		Input []string `json:"input"`
	} `json:"modalities"`
	Reasoning bool `json:"reasoning"`
}

// mdProvider models.dev 单提供商条目
type mdProvider struct {
	Models map[string]mdModel `json:"models"`
}

// Sync 立即执行一次同步：拉取 → 转换 → 落覆盖层 → 重载 catalog。
// 返回（写入条目数, error）。
func (ps *PriceSync) Sync() (int, error) {
	req, err := http.NewRequest(http.MethodGet, PriceSyncURL, nil)
	if err != nil {
		return 0, err
	}
	req.Header.Set("User-Agent", "model-gateway-pricesync")
	resp, err := ps.client.Do(req)
	if err != nil {
		ps.setError(err)
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		err := fmt.Errorf("models.dev returned HTTP %d", resp.StatusCode)
		ps.setError(err)
		return 0, err
	}
	// 上限 32MB（api.json 当前约 2MB，留余量防撑爆内存）
	data, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		ps.setError(err)
		return 0, err
	}

	var providers map[string]mdProvider
	if err := json.Unmarshal(data, &providers); err != nil {
		ps.setError(err)
		return 0, err
	}

	out := catalogFile{Version: 2, Models: map[string]*CatalogEntry{}}
	for _, prov := range providers {
		for id, m := range prov.Models {
			key := normalizeCatalogKey(id)
			if key == "" {
				continue
			}
			e := &CatalogEntry{
				PriceIn:       m.Cost.Input,
				PriceOut:      m.Cost.Output,
				ContextLength: m.Limit.Context,
			}
			// 能力映射：input 含 image → vision；reasoning 标志 → reasoning；默认 text
			caps := []string{"text"}
			for _, mod := range m.Modalities.Input {
				if mod == "image" {
					caps = append(caps, "vision")
					break
				}
			}
			if m.Reasoning {
				caps = append(caps, "reasoning")
			}
			e.Capabilities = caps
			// 同名模型多提供商时保留价格更低的一份（用户走聚合网关通常选便宜渠道）
			if old, ok := out.Models[key]; ok && old.PriceIn+old.PriceOut <= e.PriceIn+e.PriceOut {
				continue
			}
			out.Models[key] = e
		}
	}
	if len(out.Models) == 0 {
		err := fmt.Errorf("models.dev data parsed to 0 entries, skip overwrite")
		ps.setError(err)
		return 0, err
	}

	buf, err := json.Marshal(out)
	if err != nil {
		ps.setError(err)
		return 0, err
	}
	if err := os.MkdirAll(ps.dataDir, 0755); err != nil {
		ps.setError(err)
		return 0, err
	}
	tmp := ps.syncPath() + ".tmp"
	if err := os.WriteFile(tmp, buf, 0644); err != nil {
		ps.setError(err)
		return 0, err
	}
	if err := os.Rename(tmp, ps.syncPath()); err != nil {
		ps.setError(err)
		return 0, err
	}

	// 重载 catalog（分层合并，用户手工覆盖层仍最高优先）
	if ps.catalog != nil {
		ps.catalog.Reload(ps.appDir, ps.dataDir)
	}

	ps.mu.Lock()
	ps.status.LastSync = time.Now().Format(time.RFC3339)
	ps.status.ModelCount = len(out.Models)
	ps.status.LastError = ""
	ps.mu.Unlock()
	return len(out.Models), nil
}

func (ps *PriceSync) setError(err error) {
	ps.mu.Lock()
	ps.status.LastError = err.Error()
	ps.mu.Unlock()
}

// SetEnabled 设置是否启用自动同步（A15）
func (ps *PriceSync) SetEnabled(on bool) {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	ps.enabled = on
}

// SetInterval 设置自动同步间隔（A15）
func (ps *PriceSync) SetInterval(d time.Duration) {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	if d < time.Hour {
		d = time.Hour
	}
	ps.interval = d
}

// AutoSyncLoop 后台自动同步：按 enabled/interval 动态检查，覆盖层超过 7 天未更新才真正拉取。
// 首次启动且从未同步过时延迟 5 分钟再尝试（避开开机网络未就绪窗口）。
func (ps *PriceSync) AutoSyncLoop(stop <-chan struct{}) {
	tryOnce := func() {
		ps.mu.Lock()
		if !ps.enabled {
			ps.mu.Unlock()
			return
		}
		interval := ps.interval
		ps.mu.Unlock()
		if interval <= 0 {
			interval = 24 * time.Hour
		}

		if fi, err := os.Stat(ps.syncPath()); err == nil {
			if time.Since(fi.ModTime()) < 7*24*time.Hour {
				return // 数据仍新鲜
			}
		}
		_, _ = ps.Sync()
	}
	// P2-1：改用 time.NewTimer + 显式 Stop，避免 goroutine 取消后 time.After 定时器泄漏
	timer := time.NewTimer(5 * time.Minute)
	defer timer.Stop()
	select {
	case <-timer.C:
		tryOnce()
	case <-stop:
		return
	}
	// 用最小 1h  ticker + 内部间隔判断，支持运行时改 interval
	ticker := time.NewTicker(time.Hour)
	defer ticker.Stop()
	var next time.Time
	for {
		select {
		case <-ticker.C:
			ps.mu.Lock()
			interval := ps.interval
			enabled := ps.enabled
			ps.mu.Unlock()
			if !enabled {
				next = time.Time{}
				continue
			}
			if interval <= 0 {
				interval = 24 * time.Hour
			}
			if next.IsZero() || time.Now().After(next) {
				tryOnce()
				next = time.Now().Add(interval)
			}
		case <-stop:
			return
		}
	}
}
