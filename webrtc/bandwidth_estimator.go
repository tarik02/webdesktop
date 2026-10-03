package webrtc

import (
	"math"
	"strings"
	"sync"
	"time"

	"github.com/pion/interceptor"
	"github.com/pion/interceptor/pkg/cc"
	"github.com/pion/interceptor/pkg/gcc"
	"github.com/pion/rtcp"
	"github.com/pion/rtp"
)

const (
	bandwidthMinBitrate = 100_000
	bandwidthMaxBitrate = 50_000_000

	sentRateWindow        = 500 * time.Millisecond
	sentRateBucketCount   = 10
	sentRateBucketPeriod  = sentRateWindow / sentRateBucketCount
	applicationLimitEnter = 0.65
	applicationLimitExit  = 0.80

	delayIncreaseFactor   = 1.08
	delayIncreaseCap      = 1.5
	lossDecreaseThreshold = 0.1
	lossDecreaseInterval  = 200 * time.Millisecond
)

// bandwidthEstimator keeps Pion's GCC as the overuse and loss detector but
// owns the target bitrate. Pion's rate controller derives every decrease, and
// even its additive increases, from the received rate. While the desktop is
// static the encoder sends far below its target, so the received rate says
// nothing about link capacity and the GCC target decays to its floor. This
// estimator holds the target while the sender is application-limited and only
// applies delay-based changes measured while video used most of its budget.
//
// Loss-based decreases apply in every state because loss is direct evidence.
type bandwidthEstimator struct {
	detector       *gcc.SendSideBWE
	encoderBitrate func() int

	sentMu      sync.Mutex
	sentBuckets [sentRateBucketCount]int
	sentBucket  int64

	mu                 sync.Mutex
	target             int
	applicationLimited bool
	lastDelayStats     delayStats
	lastIncrease       time.Time
	lastLossDecrease   time.Time
	onChange           func(int)
}

type delayStats struct {
	state         string
	measurement   float64
	estimate      float64
	threshold     float64
	targetBitrate int
}

var _ cc.BandwidthEstimator = (*bandwidthEstimator)(nil)

// newBandwidthEstimator creates an estimator for one peer connection.
// encoderBitrate reports the bitrate in bits per second currently configured
// on the shared video encoder.
func newBandwidthEstimator(initialBitrate int, encoderBitrate func() int) (*bandwidthEstimator, error) {
	detector, err := gcc.NewSendSideBWE(
		gcc.SendSideBWEInitialBitrate(initialBitrate),
		gcc.SendSideBWEMinBitrate(bandwidthMinBitrate),
		gcc.SendSideBWEMaxBitrate(bandwidthMaxBitrate),
		// RTP is not paced so Opus never waits behind a large video access unit.
		gcc.SendSideBWEPacer(gcc.NewNoOpPacer()),
	)
	if err != nil {
		return nil, err
	}
	return &bandwidthEstimator{
		detector:       detector,
		encoderBitrate: encoderBitrate,
		target:         clampBitrate(initialBitrate),
	}, nil
}

func (e *bandwidthEstimator) AddStream(info *interceptor.StreamInfo, writer interceptor.RTPWriter) interceptor.RTPWriter {
	if !strings.HasPrefix(strings.ToLower(info.MimeType), "video/") {
		return e.detector.AddStream(info, writer)
	}
	return e.detector.AddStream(info, interceptor.RTPWriterFunc(
		func(header *rtp.Header, payload []byte, attributes interceptor.Attributes) (int, error) {
			written, err := writer.Write(header, payload, attributes)
			if err == nil {
				e.recordSent(time.Now(), header.MarshalSize()+len(payload))
			}
			return written, err
		},
	))
}

func (e *bandwidthEstimator) WriteRTCP(packets []rtcp.Packet, attributes interceptor.Attributes) error {
	if err := e.detector.WriteRTCP(packets, attributes); err != nil {
		return err
	}
	for _, packet := range packets {
		switch packet.(type) {
		case *rtcp.TransportLayerCC, *rtcp.CCFeedbackReport:
			e.update(time.Now())
			return nil
		}
	}
	return nil
}

func (e *bandwidthEstimator) GetTargetBitrate() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.target
}

func (e *bandwidthEstimator) OnTargetBitrateChange(f func(bitrate int)) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.onChange = f
}

func (e *bandwidthEstimator) GetStats() map[string]any {
	stats := e.detector.GetStats()
	e.mu.Lock()
	defer e.mu.Unlock()
	stats["targetBitrate"] = e.target
	stats["applicationLimited"] = e.applicationLimited
	stats["sentBitrate"] = e.sentBitrate(time.Now())
	return stats
}

func (e *bandwidthEstimator) Close() error {
	return e.detector.Close()
}

func (e *bandwidthEstimator) update(now time.Time) {
	detectorStats := e.detector.GetStats()
	delay := delayStats{
		state:         statString(detectorStats, "state"),
		measurement:   statFloat(detectorStats, "delayMeasurement"),
		estimate:      statFloat(detectorStats, "delayEstimate"),
		threshold:     statFloat(detectorStats, "delayThreshold"),
		targetBitrate: statInt(detectorStats, "delayTargetBitrate"),
	}
	averageLoss := statFloat(detectorStats, "averageLoss")
	sentBitrate := e.sentBitrate(now)
	encoderBitrate := e.encoderBitrate()

	e.mu.Lock()
	previous := e.target
	budget := min(e.target, encoderBitrate)
	switch {
	case float64(sentBitrate) < applicationLimitEnter*float64(budget):
		e.applicationLimited = true
	case float64(sentBitrate) > applicationLimitExit*float64(budget):
		e.applicationLimited = false
	}

	// GCC reports nothing while it holds, so its last stats stay visible
	// until the next update. Only act on a fresh delay-based decision.
	freshDelay := delay != e.lastDelayStats
	e.lastDelayStats = delay
	if freshDelay && !e.applicationLimited {
		switch delay.state {
		case "decrease":
			// GCC's decrease target is 0.85 of the acknowledged rate.
			e.target = min(e.target, clampBitrate(delay.targetBitrate))
			e.lastIncrease = now
		case "increase":
			elapsed := 1.0
			if !e.lastIncrease.IsZero() {
				elapsed = min(now.Sub(e.lastIncrease).Seconds(), 1)
			}
			increased := int(float64(e.target) * math.Pow(delayIncreaseFactor, elapsed))
			ceiling := max(e.target, int(delayIncreaseCap*float64(sentBitrate)))
			e.target = clampBitrate(min(increased, ceiling))
			e.lastIncrease = now
		}
	}
	if averageLoss > lossDecreaseThreshold && now.Sub(e.lastLossDecrease) > lossDecreaseInterval {
		e.target = clampBitrate(int(float64(e.target) * (1 - 0.5*averageLoss)))
		e.lastLossDecrease = now
	}

	changed := e.target != previous
	onChange := e.onChange
	e.mu.Unlock()

	if changed && onChange != nil {
		// Callbacks run outside the RTCP reader and read the newest target so
		// a delayed goroutine cannot apply an older estimate.
		go onChange(e.GetTargetBitrate())
	}
}

func (e *bandwidthEstimator) recordSent(now time.Time, size int) {
	bucket := now.UnixNano() / int64(sentRateBucketPeriod)
	e.sentMu.Lock()
	defer e.sentMu.Unlock()
	e.advanceSentBucketsLocked(bucket)
	e.sentBuckets[bucket%sentRateBucketCount] += size
}

// sentBitrate returns the video bitrate written over the last sentRateWindow.
func (e *bandwidthEstimator) sentBitrate(now time.Time) int {
	bucket := now.UnixNano() / int64(sentRateBucketPeriod)
	e.sentMu.Lock()
	defer e.sentMu.Unlock()
	e.advanceSentBucketsLocked(bucket)
	total := 0
	for _, size := range e.sentBuckets {
		total += size
	}
	return int(float64(total*8) / sentRateWindow.Seconds())
}

func (e *bandwidthEstimator) advanceSentBucketsLocked(bucket int64) {
	if bucket <= e.sentBucket {
		return
	}
	stale := min(bucket-e.sentBucket, sentRateBucketCount)
	for offset := int64(1); offset <= stale; offset++ {
		e.sentBuckets[(e.sentBucket+offset)%sentRateBucketCount] = 0
	}
	e.sentBucket = bucket
}

func clampBitrate(bitrate int) int {
	return min(max(bitrate, bandwidthMinBitrate), bandwidthMaxBitrate)
}

func statString(stats map[string]any, key string) string {
	value, _ := stats[key].(string)
	return value
}

func statFloat(stats map[string]any, key string) float64 {
	value, _ := stats[key].(float64)
	return value
}

func statInt(stats map[string]any, key string) int {
	value, _ := stats[key].(int)
	return value
}
