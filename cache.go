package main

import (
	"sync"
	"time"
)

// 簡易インメモリキャッシュ
// 用途: /api/stocks, /api/oneil-ranking など重い読み取り API の高速化
// TTL 経過後は自動失効、明示的な invalidate API も提供 (将来書き込みフックで使用)

type cacheEntry struct {
	data      []byte
	expiresAt time.Time
}

var (
	cacheMu    sync.RWMutex
	cacheStore = make(map[string]cacheEntry)
)

// cacheGet はキャッシュから取得。期限切れ・未存在は ok=false
func cacheGet(key string) ([]byte, bool) {
	cacheMu.RLock()
	e, ok := cacheStore[key]
	cacheMu.RUnlock()
	if !ok {
		return nil, false
	}
	if time.Now().After(e.expiresAt) {
		cacheMu.Lock()
		delete(cacheStore, key)
		cacheMu.Unlock()
		return nil, false
	}
	return e.data, true
}

// cacheSet はキャッシュに保存
func cacheSet(key string, data []byte, ttl time.Duration) {
	cacheMu.Lock()
	cacheStore[key] = cacheEntry{
		data:      data,
		expiresAt: time.Now().Add(ttl),
	}
	cacheMu.Unlock()
}

// cacheClear は全キャッシュをクリア (DB更新後の手動 invalidate 用)
func cacheClear() {
	cacheMu.Lock()
	cacheStore = make(map[string]cacheEntry)
	cacheMu.Unlock()
}

// cacheStats は監視用
func cacheStats() (entries int) {
	cacheMu.RLock()
	defer cacheMu.RUnlock()
	return len(cacheStore)
}
