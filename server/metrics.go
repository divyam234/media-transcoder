package server

import (
	"context"
	"net/http"
	"sync/atomic"
)

type Metrics struct {
	hlsSessions       atomic.Int64
	dashSessions      atomic.Int64
	segmentsGenerated atomic.Int64
	segmentCacheHits  atomic.Int64
	segmentErrors     atomic.Int64
}

type MetricsSnapshot struct {
	HLSSessions       int64 `json:"hls_sessions"`
	DASHSessions      int64 `json:"dash_sessions"`
	SegmentsGenerated int64 `json:"segments_generated"`
	SegmentCacheHits  int64 `json:"segment_cache_hits"`
	SegmentErrors     int64 `json:"segment_errors"`
}

func (m *Metrics) Snapshot() MetricsSnapshot {
	if m == nil {
		return MetricsSnapshot{}
	}
	return MetricsSnapshot{
		HLSSessions:       m.hlsSessions.Load(),
		DASHSessions:      m.dashSessions.Load(),
		SegmentsGenerated: m.segmentsGenerated.Load(),
		SegmentCacheHits:  m.segmentCacheHits.Load(),
		SegmentErrors:     m.segmentErrors.Load(),
	}
}

func (s *Server) metricsHandler(_ context.Context, w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, s.metrics.Snapshot())
}
