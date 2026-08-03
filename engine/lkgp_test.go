package engine

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLKGPRecordGet(t *testing.T) {
	lg := NewLastGoodTracker("")
	lg.Record("gpt-group", "openai")
	if lg.Get("gpt-group") != "openai" {
		t.Fatalf("expected openai, got %s", lg.Get("gpt-group"))
	}
	if lg.Boost("gpt-group", "openai") != LKGPExtraBoost {
		t.Fatal("expected boost for last-good provider")
	}
	if lg.Boost("gpt-group", "other") != 0 {
		t.Fatal("expected no boost for other provider")
	}
}

func TestLKGPPersist(t *testing.T) {
	dir := t.TempDir()
	lg := NewLastGoodTracker(dir)
	lg.Record("gpt-group", "openai")
	// 重新加载（模拟重启）
	lg2 := NewLastGoodTracker(dir)
	if lg2.Get("gpt-group") != "openai" {
		t.Fatalf("expected persisted mapping, got %s", lg2.Get("gpt-group"))
	}
	_ = os.Remove(filepath.Join(dir, "lkgp.json"))
}
