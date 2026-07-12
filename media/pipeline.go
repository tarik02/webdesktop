package media

import (
	"fmt"
	"runtime"
	"sync/atomic"
	"time"

	"github.com/go-gst/go-gst/gst"
	"go.uber.org/zap"
)

const (
	pipelineBusPollInterval = 100 * time.Millisecond
	pipelineTraceInterval   = 5 * time.Second
)

type videoFlowTrace struct {
	buffers        atomic.Uint64
	invalidPTS     atomic.Uint64
	duplicatePTS   atomic.Uint64
	ptsRegressions atomic.Uint64
	lastArrival    atomic.Int64
	lastWallGap    atomic.Int64
	maxWallGap     atomic.Int64
	lastPTS        atomic.Int64
	lastPTSGap     atomic.Int64
	maxPTSGap      atomic.Int64
	maxPTSRegress  atomic.Int64
	havePTS        atomic.Bool
	lastOffset     atomic.Int64
	haveOffset     atomic.Bool
	repeatedOffset atomic.Uint64
	offsetChanges  atomic.Uint64
	bytes          atomic.Uint64
	keyframes      atomic.Uint64
}

func (t *videoFlowTrace) observe(buffer *gst.Buffer) {
	t.buffers.Add(1)
	t.bytes.Add(uint64(buffer.GetSize()))
	if !buffer.HasFlags(gst.BufferFlagDeltaUnit) {
		t.keyframes.Add(1)
	}

	now := time.Now().UnixNano()
	if previous := t.lastArrival.Swap(now); previous > 0 {
		gap := now - previous
		t.lastWallGap.Store(gap)
		for previousMax := t.maxWallGap.Load(); gap > previousMax; previousMax = t.maxWallGap.Load() {
			if t.maxWallGap.CompareAndSwap(previousMax, gap) {
				break
			}
		}
	}

	if offset := buffer.Offset(); offset >= 0 {
		if t.haveOffset.Swap(true) {
			if offset == t.lastOffset.Swap(offset) {
				t.repeatedOffset.Add(1)
			} else {
				t.offsetChanges.Add(1)
			}
		} else {
			t.lastOffset.Store(offset)
		}
	}

	pts := buffer.PresentationTimestamp().AsDuration()
	if pts == nil {
		t.invalidPTS.Add(1)
		return
	}
	currentPTS := int64(*pts)
	if t.havePTS.Swap(true) {
		gap := currentPTS - t.lastPTS.Swap(currentPTS)
		t.lastPTSGap.Store(gap)
		if gap == 0 {
			t.duplicatePTS.Add(1)
		} else if gap < 0 {
			t.ptsRegressions.Add(1)
			regression := -gap
			for previousMax := t.maxPTSRegress.Load(); regression > previousMax; previousMax = t.maxPTSRegress.Load() {
				if t.maxPTSRegress.CompareAndSwap(previousMax, regression) {
					break
				}
			}
		} else {
			for previousMax := t.maxPTSGap.Load(); gap > previousMax; previousMax = t.maxPTSGap.Load() {
				if t.maxPTSGap.CompareAndSwap(previousMax, gap) {
					break
				}
			}
		}
		return
	}
	t.lastPTS.Store(currentPTS)
}

func addVideoTraceProbe(
	element *gst.Element,
	padName string,
	trace *videoFlowTrace,
	afterObserve func(*gst.Buffer),
) (*gst.Pad, error) {
	pad := element.GetStaticPad(padName)
	if pad == nil {
		return nil, fmt.Errorf("get %s %s pad for tracing", element.GetName(), padName)
	}
	runtime.SetFinalizer(pad.GObject(), nil)

	if pad.AddProbe(gst.PadProbeTypeBuffer, func(_ *gst.Pad, info *gst.PadProbeInfo) gst.PadProbeReturn {
		if buffer := info.GetBuffer(); buffer != nil {
			if afterObserve != nil {
				afterObserve(buffer)
			}
			trace.observe(buffer)
		}
		return gst.PadProbeOK
	}) == 0 {
		pad.Unref()
		return nil, fmt.Errorf("add %s %s buffer probe", element.GetName(), padName)
	}
	return pad, nil
}

func appendVideoFlowTraceFields(
	fields []zap.Field,
	prefix string,
	trace *videoFlowTrace,
	now int64,
) []zap.Field {
	lastArrival := trace.lastArrival.Load()
	var lastArrivalAge time.Duration
	if lastArrival > 0 {
		lastArrivalAge = time.Duration(now - lastArrival)
	}
	return append(fields,
		zap.Uint64(prefix+"_buffers", trace.buffers.Load()),
		zap.Duration(prefix+"_last_arrival_age", lastArrivalAge),
		zap.Duration(prefix+"_last_wall_gap", time.Duration(trace.lastWallGap.Load())),
		zap.Duration(prefix+"_max_wall_gap", time.Duration(trace.maxWallGap.Load())),
		zap.Duration(prefix+"_last_pts", time.Duration(trace.lastPTS.Load())),
		zap.Duration(prefix+"_last_pts_gap", time.Duration(trace.lastPTSGap.Load())),
		zap.Duration(prefix+"_max_pts_gap", time.Duration(trace.maxPTSGap.Load())),
		zap.Duration(prefix+"_max_pts_regression", time.Duration(trace.maxPTSRegress.Load())),
		zap.Uint64(prefix+"_invalid_pts", trace.invalidPTS.Load()),
		zap.Uint64(prefix+"_duplicate_pts", trace.duplicatePTS.Load()),
		zap.Uint64(prefix+"_pts_regressions", trace.ptsRegressions.Load()),
		zap.Int64(prefix+"_last_offset", trace.lastOffset.Load()),
		zap.Uint64(prefix+"_repeated_offset", trace.repeatedOffset.Load()),
		zap.Uint64(prefix+"_offset_changes", trace.offsetChanges.Load()),
		zap.Uint64(prefix+"_bytes", trace.bytes.Load()),
		zap.Uint64(prefix+"_keyframes", trace.keyframes.Load()),
	)
}
