package metrics

import (
	"fmt"
	"math"
	"runtime"
	"sync"
	"sync/atomic"
	"time"
)

type PlatformChecker interface {
	Name() string
	Configured() bool
}

type PlatformStatus struct {
	Name               string   `json:"name"`
	Active             bool     `json:"active"`
	Status             string   `json:"status"`
	TotalCalls         int64    `json:"totalCalls"`
	SuccessCalls       int64    `json:"successCalls"`
	FailedCalls        int64    `json:"failedCalls"`
	RtoCalls           int64    `json:"rtoCalls"`
	OverloadCalls      int64    `json:"overloadCalls"`
	SuccessRatePercent *float64 `json:"successRatePercent,omitempty"`
	LastStatus         string   `json:"lastStatus,omitempty"`
	LastSeenAt         *string  `json:"lastSeenAt,omitempty"`
	Details            string   `json:"details,omitempty"`
}

type RequestMetrics struct {
	TotalRequests      int64            `json:"total"`
	SuccessRequests    int64            `json:"success"`
	FailedRequests     int64            `json:"failed"`
	SuccessRatePercent float64          `json:"successRatePercent"`
	FailureRatePercent float64          `json:"failureRatePercent"`
	StatusCodes        map[string]int64 `json:"statusCodes"`
}

type LyricsMetrics struct {
	TotalLookups       int64            `json:"totalLookups"`
	Found              int64            `json:"found"`
	NotFound           int64            `json:"notFound"`
	SuccessRatePercent float64          `json:"successRatePercent"`
	CacheHits          map[string]int64 `json:"cacheHits"`
	WinnerDistribution map[string]int64 `json:"winnerDistribution"`
}

type SystemMetrics struct {
	Goroutines    int     `json:"goroutines"`
	MemoryAllocMB float64 `json:"memoryAllocMB"`
	MemorySysMB   float64 `json:"memorySysMB"`
	NumGC         uint32  `json:"numGC"`
}

type HealthDebugReport struct {
	Requests  RequestMetrics            `json:"requests"`
	Lyrics    LyricsMetrics             `json:"lyrics"`
	Platforms map[string]PlatformStatus `json:"platforms"`
	System    SystemMetrics             `json:"system"`
}

type providerCounters struct {
	mu            sync.Mutex
	name          string
	configuredFn  func() bool
	details       string
	totalCalls    int64
	successCalls  int64
	failedCalls   int64
	rtoCalls      int64
	overloadCalls int64
	lastStatus    string
	lastSeen      time.Time
}

type Collector struct {
	started time.Time

	totalReqs   atomic.Int64
	successReqs atomic.Int64
	failedReqs  atomic.Int64
	statusCodes sync.Map

	totalLookups atomic.Int64
	foundLookups atomic.Int64
	missLookups  atomic.Int64
	cacheHits    sync.Map
	winners      sync.Map

	platformMu sync.RWMutex
	platforms  map[string]*providerCounters
}

var Default = NewCollector()

func NewCollector() *Collector {
	return &Collector{
		started:   time.Now(),
		platforms: make(map[string]*providerCounters),
	}
}

func (c *Collector) RegisterPlatform(id, name string, configuredFn func() bool, details string) {
	c.platformMu.Lock()
	defer c.platformMu.Unlock()
	c.platforms[id] = &providerCounters{
		name:         name,
		configuredFn: configuredFn,
		details:      details,
		lastStatus:   "IDLE",
	}
}

func (c *Collector) RecordHTTPRequest(statusCode int) {
	c.totalReqs.Add(1)
	if statusCode >= 200 && statusCode < 400 {
		c.successReqs.Add(1)
	} else {
		c.failedReqs.Add(1)
	}

	codeStr := fmt.Sprintf("%d", statusCode)
	val, _ := c.statusCodes.LoadOrStore(codeStr, &atomic.Int64{})
	val.(*atomic.Int64).Add(1)
}

func (c *Collector) RecordLyricsLookup(winner string, cacheLevel string, found bool) {
	c.totalLookups.Add(1)
	if found {
		c.foundLookups.Add(1)
	} else {
		c.missLookups.Add(1)
	}

	if cacheLevel != "" {
		val, _ := c.cacheHits.LoadOrStore(cacheLevel, &atomic.Int64{})
		val.(*atomic.Int64).Add(1)
	}

	if winner != "" {
		val, _ := c.winners.LoadOrStore(winner, &atomic.Int64{})
		val.(*atomic.Int64).Add(1)
	}
}

func (c *Collector) RecordProviderCall(providerID string, status string, elapsedMs int64) {
	c.platformMu.Lock()
	p, ok := c.platforms[providerID]
	if !ok {
		p = &providerCounters{
			name:       providerID,
			lastStatus: status,
		}
		c.platforms[providerID] = p
	}
	c.platformMu.Unlock()

	p.mu.Lock()
	defer p.mu.Unlock()
	p.totalCalls++
	p.lastStatus = status
	p.lastSeen = time.Now()

	switch status {
	case "OK":
		p.successCalls++
	case "RTO":
		p.rtoCalls++
		p.failedCalls++
	case "BAD":
		p.failedCalls++
	}
}

func (c *Collector) RecordProviderOverload(providerID string) {
	c.platformMu.Lock()
	p, ok := c.platforms[providerID]
	if !ok {
		p = &providerCounters{name: providerID, lastStatus: "OVERLOAD"}
		c.platforms[providerID] = p
	}
	c.platformMu.Unlock()

	p.mu.Lock()
	defer p.mu.Unlock()
	p.totalCalls++
	p.overloadCalls++
	p.lastStatus = "OVERLOAD"
	p.lastSeen = time.Now()
}

func (c *Collector) Snapshot() HealthDebugReport {
	totReq := c.totalReqs.Load()
	succReq := c.successReqs.Load()
	failReq := c.failedReqs.Load()

	var succRate, failRate float64
	if totReq > 0 {
		succRate = roundTo2((float64(succReq) / float64(totReq)) * 100.0)
		failRate = roundTo2((float64(failReq) / float64(totReq)) * 100.0)
	} else {
		succRate = 100.0
		failRate = 0.0
	}

	codes := make(map[string]int64)
	c.statusCodes.Range(func(key, value any) bool {
		codes[key.(string)] = value.(*atomic.Int64).Load()
		return true
	})

	totLyr := c.totalLookups.Load()
	fndLyr := c.foundLookups.Load()
	mssLyr := c.missLookups.Load()
	var lyrSuccRate float64
	if totLyr > 0 {
		lyrSuccRate = roundTo2((float64(fndLyr) / float64(totLyr)) * 100.0)
	} else {
		lyrSuccRate = 100.0
	}

	hits := make(map[string]int64)
	c.cacheHits.Range(func(key, value any) bool {
		hits[key.(string)] = value.(*atomic.Int64).Load()
		return true
	})

	winDist := make(map[string]int64)
	c.winners.Range(func(key, value any) bool {
		winDist[key.(string)] = value.(*atomic.Int64).Load()
		return true
	})

	c.platformMu.RLock()
	plats := make(map[string]PlatformStatus, len(c.platforms))
	for id, p := range c.platforms {
		p.mu.Lock()
		active := true
		if p.configuredFn != nil {
			active = p.configuredFn()
		}

		status := "ONLINE"
		if !active {
			status = "NOT_CONFIGURED"
		} else if p.failedCalls > 0 && p.successCalls == 0 && p.totalCalls > 3 {
			status = "OFFLINE"
		} else if p.failedCalls > p.successCalls && p.totalCalls > 5 {
			status = "DEGRADED"
		}

		var rate *float64
		if p.totalCalls > 0 {
			r := roundTo2((float64(p.successCalls) / float64(p.totalCalls)) * 100.0)
			rate = &r
		}

		var lastSeenStr *string
		if !p.lastSeen.IsZero() {
			s := p.lastSeen.Format(time.RFC3339)
			lastSeenStr = &s
		}

		plats[id] = PlatformStatus{
			Name:               p.name,
			Active:             active,
			Status:             status,
			TotalCalls:         p.totalCalls,
			SuccessCalls:       p.successCalls,
			FailedCalls:        p.failedCalls,
			RtoCalls:           p.rtoCalls,
			OverloadCalls:      p.overloadCalls,
			SuccessRatePercent: rate,
			LastStatus:         p.lastStatus,
			LastSeenAt:         lastSeenStr,
			Details:            p.details,
		}
		p.mu.Unlock()
	}
	c.platformMu.RUnlock()

	var m runtime.MemStats
	runtime.ReadMemStats(&m)

	sys := SystemMetrics{
		Goroutines:    runtime.NumGoroutine(),
		MemoryAllocMB: roundTo2(float64(m.Alloc) / 1024.0 / 1024.0),
		MemorySysMB:   roundTo2(float64(m.Sys) / 1024.0 / 1024.0),
		NumGC:         m.NumGC,
	}

	return HealthDebugReport{
		Requests: RequestMetrics{
			TotalRequests:      totReq,
			SuccessRequests:    succReq,
			FailedRequests:     failReq,
			SuccessRatePercent: succRate,
			FailureRatePercent: failRate,
			StatusCodes:        codes,
		},
		Lyrics: LyricsMetrics{
			TotalLookups:       totLyr,
			Found:              fndLyr,
			NotFound:           mssLyr,
			SuccessRatePercent: lyrSuccRate,
			CacheHits:          hits,
			WinnerDistribution: winDist,
		},
		Platforms: plats,
		System:    sys,
	}
}

func roundTo2(val float64) float64 {
	return math.Round(val*100.0) / 100.0
}
