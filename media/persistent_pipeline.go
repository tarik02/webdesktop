package media

import (
	"errors"
	"fmt"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/go-gst/go-gst/gst"
	gstapp "github.com/go-gst/go-gst/gst/app"
	"github.com/tarik02/webdesktop/capture"
	"go.uber.org/zap"
)

type persistentVideoPipelineTrace struct {
	source           videoFlowTrace
	handoffOutput    videoFlowTrace
	handoffSink      videoFlowTrace
	routedBuffers    atomic.Uint64
	routeDuration    atomic.Int64
	maxRouteDuration atomic.Int64
}

type videoEncoderBranchTrace struct {
	rateOutput        videoFlowTrace
	encoderInput      videoFlowTrace
	encoderOutput     videoFlowTrace
	appSink           videoFlowTrace
	appSrcPushFailure atomic.Uint64
	emitDuration      atomic.Int64
	maxEmit           atomic.Int64
	bitrateKbps       atomic.Int64
}

type videoSampleSlot struct {
	mu       sync.Mutex
	sample   *gst.Sample
	sequence uint64
	changed  chan struct{}
	closed   bool
}

func newVideoSampleSlot() *videoSampleSlot {
	return &videoSampleSlot{
		changed: make(chan struct{}),
	}
}

func (s *videoSampleSlot) Store(sample *gst.Sample) bool {
	retained := sample.Ref()

	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		retained.Unref()
		return false
	}
	previous := s.sample
	s.sample = retained
	s.sequence++
	close(s.changed)
	s.changed = make(chan struct{})
	s.mu.Unlock()

	if previous != nil {
		previous.Unref()
	}
	return true
}

func (s *videoSampleSlot) Next(
	after uint64,
) (sample *gst.Sample, sequence uint64, changed <-chan struct{}, open bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.closed {
		return nil, after, nil, false
	}
	if s.sample != nil && s.sequence != after {
		return s.sample.Ref(), s.sequence, nil, true
	}
	return nil, after, s.changed, true
}

func (s *videoSampleSlot) Close() {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.closed = true
	sample := s.sample
	s.sample = nil
	close(s.changed)
	s.mu.Unlock()

	if sample != nil {
		sample.Unref()
	}
}

type videoEncoderBranch struct {
	generation uint64
	namePrefix string
	quality    Quality
	profile    EncoderProfile
	tuning     Tuning
	pipeline   *gst.Pipeline
	source     *gstapp.Source
	rate       *gst.Element
	encoder    *gst.Element
	sink       *gstapp.Sink
	elements   []*gst.Element
	probePads  []*gst.Pad
	emit       func(<-chan struct{}, Sample) bool
	logger     *zap.Logger
	trace      *videoEncoderBranchTrace

	active       chan struct{}
	ready        chan struct{}
	failed       chan error
	stop         chan struct{}
	inputDone    chan struct{}
	monitorDone  chan struct{}
	done         chan struct{}
	activateOnce sync.Once
	readyOnce    sync.Once
	stopOnce     sync.Once
	closeOnce    sync.Once
	doneOnce     sync.Once
	lifecycleMu  sync.Mutex
	closed       bool
	inputStarted bool
	emitMu       sync.Mutex
	errMu        sync.RWMutex
	err          error
}

func newVideoEncoderBranch(
	generation uint64,
	quality Quality,
	profile EncoderProfile,
	tuning Tuning,
	emit func(<-chan struct{}, Sample) bool,
	logger *zap.Logger,
) (_ *videoEncoderBranch, err error) {
	namePrefix := fmt.Sprintf("video-branch-%d", generation)
	ratePath := []string{
		"videorate",
		"name=" + namePrefix + "-rate",
		"drop-only=true",
		fmt.Sprintf("max-rate=%d", quality.Framerate),
		"skip-to-first=true",
	}
	profilePath, err := profile.RenderPipeline(quality.Profile, namePrefix, quality, tuning)
	if err != nil {
		return nil, err
	}

	description := strings.Join([]string{
		"appsrc",
		"name=" + namePrefix + "-appsrc",
		"is-live=true",
		"format=time",
		"do-timestamp=false",
		"block=false",
		"max-buffers=1",
		"max-bytes=0",
		"max-time=0",
		"leaky-type=downstream",
		"handle-segment-change=true",
		"emit-signals=false",
		"!",
		strings.Join(ratePath, " "),
		"!",
		profilePath,
		"!",
		"appsink",
		"name=" + namePrefix + "-appsink",
		"max-buffers=1",
		"wait-on-eos=false",
		"enable-last-sample=false",
		"sync=false",
		"async=false",
	}, " ")
	pipeline, err := gst.NewPipelineFromString(description)
	if err != nil {
		return nil, fmt.Errorf("create %s encoder pipeline: %w", quality.Profile, err)
	}

	var sourceElement *gst.Element
	var rate *gst.Element
	var encoder *gst.Element
	var sinkElement *gst.Element
	defer func() {
		if err == nil {
			return
		}
		_ = pipeline.SetState(gst.StateNull)
		for _, element := range []*gst.Element{sourceElement, rate, encoder, sinkElement} {
			if element == nil {
				continue
			}
			runtime.SetFinalizer(element.GObject(), nil)
			element.Unref()
		}
		runtime.SetFinalizer(pipeline.GObject(), nil)
		pipeline.Unref()
	}()

	sourceElement, err = pipeline.GetElementByName(namePrefix + "-appsrc")
	if err != nil {
		return nil, fmt.Errorf("get encoder branch appsrc: %w", err)
	}
	source := gstapp.SrcFromElement(sourceElement)
	if source == nil {
		return nil, errors.New("encoder branch source is not a GStreamer appsrc")
	}
	rate, err = pipeline.GetElementByName(namePrefix + "-rate")
	if err != nil {
		return nil, fmt.Errorf("get encoder branch videorate: %w", err)
	}
	encoder, err = pipeline.GetElementByName(namePrefix + "-" + profile.EncoderElement)
	if err != nil {
		return nil, fmt.Errorf("get encoder branch encoder: %w", err)
	}
	sinkElement, err = pipeline.GetElementByName(namePrefix + "-appsink")
	if err != nil {
		return nil, fmt.Errorf("get encoder branch appsink: %w", err)
	}
	sink := gstapp.SinkFromElement(sinkElement)
	if sink == nil {
		return nil, errors.New("encoder branch sink is not a GStreamer appsink")
	}

	trace := &videoEncoderBranchTrace{}
	trace.bitrateKbps.Store(int64(quality.BitrateKbps))
	probePads := make([]*gst.Pad, 0, 3)
	for _, probe := range []struct {
		element *gst.Element
		pad     string
		trace   *videoFlowTrace
	}{
		{element: rate, pad: "src", trace: &trace.rateOutput},
		{element: encoder, pad: "sink", trace: &trace.encoderInput},
		{element: encoder, pad: "src", trace: &trace.encoderOutput},
	} {
		pad, probeErr := addVideoTraceProbe(probe.element, probe.pad, probe.trace, nil)
		if probeErr != nil {
			for _, addedPad := range probePads {
				addedPad.Unref()
			}
			return nil, probeErr
		}
		probePads = append(probePads, pad)
	}

	result := &videoEncoderBranch{
		generation:  generation,
		namePrefix:  namePrefix,
		quality:     quality,
		profile:     profile,
		tuning:      tuning,
		pipeline:    pipeline,
		source:      source,
		rate:        rate,
		encoder:     encoder,
		sink:        sink,
		elements:    []*gst.Element{sourceElement, rate, encoder, sinkElement},
		probePads:   probePads,
		emit:        emit,
		logger:      logger,
		trace:       trace,
		active:      make(chan struct{}),
		ready:       make(chan struct{}),
		failed:      make(chan error, 1),
		stop:        make(chan struct{}),
		inputDone:   make(chan struct{}),
		monitorDone: make(chan struct{}),
		done:        make(chan struct{}),
	}
	sink.SetCallbacks(&gstapp.SinkCallbacks{
		NewSampleFunc: result.handleSample,
	})
	go result.monitor()
	return result, nil
}

func (b *videoEncoderBranch) Start(samples *videoSampleSlot, timeout time.Duration) error {
	b.lifecycleMu.Lock()
	if b.closed {
		b.lifecycleMu.Unlock()
		return errors.New("video encoder branch is already closed")
	}
	if err := b.pipeline.SetState(gst.StatePlaying); err != nil {
		b.lifecycleMu.Unlock()
		return errors.Join(err, b.Close())
	}
	stateChange, state := b.pipeline.GetState(gst.VoidPending, gst.ClockTime(timeout))
	if stateChange == gst.StateChangeFailure || state != gst.StatePlaying {
		b.lifecycleMu.Unlock()
		return errors.Join(
			fmt.Errorf(
				"encoder pipeline did not reach PLAYING within %s: state=%s transition=%s",
				timeout,
				state,
				stateChange,
			),
			b.Close(),
		)
	}
	select {
	case <-b.stop:
		err := b.Err()
		b.lifecycleMu.Unlock()
		return errors.Join(
			errors.New("video encoder branch stopped while starting"),
			err,
			b.Close(),
		)
	default:
	}
	b.inputStarted = true
	go b.pumpInput(samples)
	b.lifecycleMu.Unlock()
	return nil
}

func (b *videoEncoderBranch) pumpInput(samples *videoSampleSlot) {
	defer close(b.inputDone)

	var sequence uint64
	for {
		select {
		case <-b.stop:
			return
		default:
		}

		sample, nextSequence, changed, open := samples.Next(sequence)
		if !open {
			return
		}
		if sample == nil {
			select {
			case <-b.stop:
				return
			case <-changed:
				continue
			}
		}

		flow := b.source.PushSample(sample)
		sample.Unref()
		sequence = nextSequence
		if flow == gst.FlowOK {
			continue
		}
		select {
		case <-b.stop:
			return
		default:
		}
		b.trace.appSrcPushFailure.Add(1)
		b.Fail(fmt.Errorf("video encoder appsrc rejected capture sample: %s", flow))
		return
	}
}

func (b *videoEncoderBranch) handleSample(sink *gstapp.Sink) gst.FlowReturn {
	sample := sink.PullSample()
	if sample == nil {
		return gst.FlowEOS
	}
	runtime.SetFinalizer(sample, nil)
	defer sample.Unref()

	buffer := sample.GetBuffer()
	if buffer == nil {
		return gst.FlowError
	}
	runtime.SetFinalizer(buffer, nil)
	defer buffer.Unref()

	var pts time.Duration
	ptsValue := buffer.PresentationTimestamp().AsDuration()
	ptsValid := ptsValue != nil
	if ptsValid {
		pts = *ptsValue
	}
	var duration time.Duration
	if value := buffer.Duration().AsDuration(); value != nil {
		duration = *value
	}
	keyFrame := !buffer.HasFlags(gst.BufferFlagDeltaUnit)
	b.trace.appSink.observe(buffer)

	select {
	case <-b.stop:
		return gst.FlowFlushing
	default:
	}
	select {
	case <-b.active:
	default:
		if !keyFrame {
			return gst.FlowOK
		}
		b.readyOnce.Do(func() {
			close(b.ready)
		})
		select {
		case <-b.active:
		case <-b.stop:
			return gst.FlowFlushing
		}
	}

	mapInfo := buffer.Map(gst.MapRead)
	if mapInfo == nil {
		return gst.FlowError
	}
	data := mapInfo.Bytes()
	buffer.Unmap()

	b.emitMu.Lock()
	defer b.emitMu.Unlock()
	select {
	case <-b.stop:
		return gst.FlowFlushing
	default:
	}

	producedAt := time.Now()
	if !b.emit(b.stop, Sample{
		Data:       data,
		Codec:      b.profile.Codec.ID,
		ProducedAt: producedAt,
		PTS:        pts,
		PTSValid:   ptsValid,
		Duration:   duration,
		KeyFrame:   keyFrame,
	}) {
		return gst.FlowFlushing
	}
	emitDuration := time.Since(producedAt)
	b.trace.emitDuration.Store(int64(emitDuration))
	for previousMax := b.trace.maxEmit.Load(); int64(emitDuration) > previousMax; previousMax = b.trace.maxEmit.Load() {
		if b.trace.maxEmit.CompareAndSwap(previousMax, int64(emitDuration)) {
			break
		}
	}
	return gst.FlowOK
}

func (b *videoEncoderBranch) Activate() error {
	return b.activateIfHealthy(nil)
}

func (b *videoEncoderBranch) activateIfHealthy(beforeActivate func()) error {
	b.errMu.Lock()
	defer b.errMu.Unlock()

	if b.err != nil {
		return b.err
	}
	select {
	case <-b.stop:
		return errors.New("video encoder branch stopped before activation")
	default:
	}
	b.activateOnce.Do(func() {
		if beforeActivate != nil {
			beforeActivate()
		}
		close(b.active)
	})
	return nil
}

func (b *videoEncoderBranch) Fail(err error) {
	b.setErr(err)
	b.Stop()
	select {
	case b.failed <- err:
	default:
	}
	b.doneOnce.Do(func() {
		close(b.done)
	})
}

func (b *videoEncoderBranch) Stop() {
	b.stopOnce.Do(func() {
		close(b.stop)
		// Wait for an in-flight emit to return before Stop completes.
		b.emitMu.Lock()
		defer b.emitMu.Unlock()
	})
}

func (b *videoEncoderBranch) SetBitrate(bitrateKbps int) error {
	next := b.quality
	next.BitrateKbps = bitrateKbps
	values, err := b.profile.RenderBitrate(next.Profile, b.namePrefix, next, b.tuning)
	if err != nil {
		return err
	}
	for index, property := range b.profile.Bitrate {
		element, err := b.pipeline.GetElementByName(b.namePrefix + "-" + property.Element)
		if err != nil {
			return fmt.Errorf("get live bitrate element %q: %w", property.Element, err)
		}
		setErr := element.SetProperty(property.Property, values[index])
		runtime.SetFinalizer(element.GObject(), nil)
		element.Unref()
		if setErr != nil {
			return fmt.Errorf("set %s.%s live bitrate property: %w", property.Element, property.Property, setErr)
		}
	}
	b.quality = next
	b.trace.bitrateKbps.Store(int64(bitrateKbps))
	return nil
}

func (b *videoEncoderBranch) RequestKeyframe() error {
	if !requestGStreamerKeyframe(b.sink.Element) {
		return errors.New("GStreamer rejected the force-key-unit event")
	}
	return nil
}

func (b *videoEncoderBranch) appendTraceFields(
	fields []zap.Field,
	prefix string,
	now int64,
) []zap.Field {
	fieldName := func(name string) string {
		if prefix == "" {
			return name
		}
		return prefix + "_" + name
	}
	fields = append(fields,
		zap.Uint64(fieldName("branch_generation"), b.generation),
		zap.String(fieldName("profile"), b.quality.Profile),
		zap.String(fieldName("codec"), b.profile.Codec.ID),
		zap.Int64(fieldName("bitrate_kbps"), b.trace.bitrateKbps.Load()),
		zap.Uint64(fieldName("appsrc_push_failures"), b.trace.appSrcPushFailure.Load()),
		zap.Duration(fieldName("appsink_emit_duration"), time.Duration(b.trace.emitDuration.Load())),
		zap.Duration(fieldName("appsink_max_emit_duration"), time.Duration(b.trace.maxEmit.Load())),
	)
	for _, property := range []string{"in", "out", "dropped", "current-level-buffers"} {
		if value, err := b.source.GetProperty(property); err == nil {
			fields = append(fields, zap.Any(fieldName("appsrc_"+strings.ReplaceAll(property, "-", "_")), value))
		}
	}
	for _, property := range []string{"in", "out", "drop", "duplicate"} {
		if value, err := b.rate.GetProperty(property); err == nil {
			fields = append(fields, zap.Any(fieldName("videorate_"+property), value))
		}
	}
	for _, property := range b.profile.Bitrate {
		element, err := b.pipeline.GetElementByName(b.namePrefix + "-" + property.Element)
		if err != nil {
			continue
		}
		if value, err := element.GetProperty(property.Property); err == nil {
			fields = append(fields, zap.Any(
				fieldName("encoder_"+strings.ReplaceAll(property.Property, "-", "_")),
				value,
			))
		}
		runtime.SetFinalizer(element.GObject(), nil)
		element.Unref()
	}
	fields = appendVideoFlowTraceFields(fields, fieldName("rate_output"), &b.trace.rateOutput, now)
	fields = appendVideoFlowTraceFields(fields, fieldName("encoder_input"), &b.trace.encoderInput, now)
	fields = appendVideoFlowTraceFields(fields, fieldName("encoder_output"), &b.trace.encoderOutput, now)
	fields = appendVideoFlowTraceFields(fields, fieldName("appsink"), &b.trace.appSink, now)
	return fields
}

func (b *videoEncoderBranch) monitor() {
	defer close(b.monitorDone)

	bus := b.pipeline.GetPipelineBus()
	runtime.SetFinalizer(bus.GObject(), nil)
	defer bus.Unref()
	for {
		select {
		case <-b.stop:
			return
		default:
		}

		message := bus.TimedPop(gst.ClockTime(pipelineBusPollInterval))
		if message == nil {
			continue
		}
		runtime.SetFinalizer(message, nil)
		select {
		case <-b.stop:
			message.Unref()
			return
		default:
		}

		source := message.Source()
		switch message.Type() {
		case gst.MessageError:
			gstreamerErr := message.ParseError()
			debug := gstreamerErr.DebugString()
			message.Unref()
			err := fmt.Errorf("gstreamer %s: %w", source, gstreamerErr)
			b.logger.Error("gstreamer encoder pipeline error",
				zap.Uint64("generation", b.generation),
				zap.String("profile", b.quality.Profile),
				zap.String("source", source),
				zap.Error(gstreamerErr),
				zap.String("debug", debug),
			)
			b.Fail(err)
			return
		case gst.MessageEOS:
			message.Unref()
			b.Fail(fmt.Errorf("gstreamer encoder pipeline %s reached end of stream", source))
			return
		case gst.MessageWarning:
			warning := message.ParseWarning()
			message.Unref()
			b.logger.Warn("gstreamer encoder warning",
				zap.Uint64("generation", b.generation),
				zap.String("profile", b.quality.Profile),
				zap.String("source", source),
				zap.Error(warning),
			)
		default:
			message.Unref()
		}
	}
}

func (b *videoEncoderBranch) Done() <-chan struct{} {
	return b.done
}

func (b *videoEncoderBranch) Err() error {
	b.errMu.RLock()
	defer b.errMu.RUnlock()
	return b.err
}

func (b *videoEncoderBranch) Close() error {
	b.closeOnce.Do(func() {
		b.lifecycleMu.Lock()
		b.closed = true
		b.Stop()
		if err := b.pipeline.SetState(gst.StateNull); err != nil {
			b.setErr(fmt.Errorf("stop GStreamer encoder pipeline: %w", err))
		}
		if b.inputStarted {
			<-b.inputDone
		}
		<-b.monitorDone

		for _, pad := range b.probePads {
			pad.Unref()
		}
		runtime.SetFinalizer(b.pipeline.GObject(), nil)
		b.pipeline.Unref()
		for _, element := range b.elements {
			runtime.SetFinalizer(element.GObject(), nil)
			element.Unref()
		}
		b.doneOnce.Do(func() {
			close(b.done)
		})
		b.lifecycleMu.Unlock()
	})
	return b.Err()
}

func (b *videoEncoderBranch) setErr(err error) {
	b.errMu.Lock()
	defer b.errMu.Unlock()
	b.err = errors.Join(b.err, err)
}

type persistentVideoPipeline struct {
	pipeline  *gst.Pipeline
	stream    *capture.Stream
	source    *gst.Element
	handoff   *gst.Element
	sink      *gstapp.Sink
	elements  []*gst.Element
	probePads []*gst.Pad
	emit      func(<-chan struct{}, Sample) bool
	active    *atomic.Bool
	logger    *zap.Logger
	trace     *persistentVideoPipelineTrace
	samples   *videoSampleSlot

	branchMu       sync.RWMutex
	branch         *videoEncoderBranch
	candidate      *videoEncoderBranch
	nextGeneration uint64
	replaceMu      sync.Mutex
	retirements    sync.WaitGroup

	stop        chan struct{}
	monitorDone chan struct{}
	done        chan struct{}
	closeOnce   sync.Once
	doneOnce    sync.Once
	errMu       sync.RWMutex
	err         error
}

// newPersistentVideoPipeline takes ownership of stream on both success and failure.
func newPersistentVideoPipeline(
	stream *capture.Stream,
	quality Quality,
	profile EncoderProfile,
	tuning Tuning,
	emit func(<-chan struct{}, Sample) bool,
	active *atomic.Bool,
	logger *zap.Logger,
) (_ *persistentVideoPipeline, err error) {
	defer func() {
		if err != nil {
			err = errors.Join(err, stream.Close())
		}
	}()
	if err := profile.Validate(quality.Profile, quality, tuning); err != nil {
		return nil, err
	}

	elements, err := gst.NewElementMany(
		"pipewiresrc",
		"identity",
	)
	if err != nil {
		return nil, fmt.Errorf("create persistent video capture elements: %w", err)
	}
	source := elements[0]
	handoff := elements[1]
	sink, err := gstapp.NewAppSink()
	if err != nil {
		for _, element := range elements {
			runtime.SetFinalizer(element.GObject(), nil)
			element.Unref()
		}
		return nil, fmt.Errorf("create persistent video capture appsink: %w", err)
	}
	captureElements := []*gst.Element{source, handoff, sink.Element}
	defer func() {
		if err == nil {
			return
		}
		for _, element := range captureElements {
			runtime.SetFinalizer(element.GObject(), nil)
			element.Unref()
		}
	}()

	if err := source.SetProperty("fd", stream.PipeWireFD); err != nil {
		return nil, fmt.Errorf("set pipewiresrc fd: %w", err)
	}
	if stream.HasPipeWireSerial {
		if err := source.SetProperty("target-object", strconv.FormatUint(stream.PipeWireSerial, 10)); err != nil {
			return nil, fmt.Errorf("set pipewiresrc target object: %w", err)
		}
	} else {
		if err := source.SetProperty("path", strconv.FormatUint(uint64(stream.NodeID), 10)); err != nil {
			return nil, fmt.Errorf("set pipewiresrc node path: %w", err)
		}
	}
	if err := source.SetProperty("do-timestamp", true); err != nil {
		return nil, fmt.Errorf("enable pipewiresrc timestamps: %w", err)
	}
	if err := source.SetProperty("provide-clock", false); err != nil {
		return nil, fmt.Errorf("disable pipewiresrc pipeline clock: %w", err)
	}
	if err := source.SetProperty("always-copy", true); err != nil {
		return nil, fmt.Errorf("copy pipewiresrc buffers: %w", err)
	}
	if err := source.SetProperty("min-buffers", 2); err != nil {
		return nil, fmt.Errorf("set minimum PipeWire capture buffers: %w", err)
	}
	if err := source.SetProperty("max-buffers", 4); err != nil {
		return nil, fmt.Errorf("set maximum PipeWire capture buffers: %w", err)
	}
	if err := source.SetProperty("keepalive-time", 1000/quality.Framerate); err != nil {
		return nil, fmt.Errorf("set pipewiresrc frame keepalive: %w", err)
	}
	captureCaps := gst.NewCapsFromString("video/x-raw")
	if captureCaps == nil {
		return nil, errors.New("create PipeWire capture caps")
	}
	sink.SetCaps(captureCaps)
	runtime.SetFinalizer(captureCaps, nil)
	captureCaps.Unref()

	sink.SetMaxBuffers(1)
	sink.SetWaitOnEOS(false)
	sink.SetArg("leaky-type", "downstream")
	if err := sink.SetProperty("enable-last-sample", false); err != nil {
		return nil, fmt.Errorf("disable capture appsink last sample: %w", err)
	}
	if err := sink.SetProperty("sync", false); err != nil {
		return nil, fmt.Errorf("disable capture appsink synchronization: %w", err)
	}
	if err := sink.SetProperty("async", false); err != nil {
		return nil, fmt.Errorf("disable capture appsink asynchronous preroll: %w", err)
	}

	initialBranch, err := newVideoEncoderBranch(1, quality, profile, tuning, emit, logger)
	if err != nil {
		return nil, err
	}
	defer func() {
		if err != nil {
			err = errors.Join(err, initialBranch.Close())
		}
	}()
	if err := initialBranch.Activate(); err != nil {
		return nil, fmt.Errorf("activate initial video encoder branch: %w", err)
	}

	pipeline, err := gst.NewPipeline("webdesktop-video-capture")
	if err != nil {
		return nil, err
	}
	defer func() {
		if err == nil {
			return
		}
		_ = pipeline.SetState(gst.StateNull)
		runtime.SetFinalizer(pipeline.GObject(), nil)
		pipeline.Unref()
	}()
	if err := pipeline.AddMany(captureElements...); err != nil {
		return nil, fmt.Errorf("add persistent video capture elements: %w", err)
	}
	if err := gst.ElementLinkMany(captureElements...); err != nil {
		return nil, fmt.Errorf("link persistent video capture pipeline: %w", err)
	}

	trace := &persistentVideoPipelineTrace{}
	probePads := make([]*gst.Pad, 0, 3)
	for _, probe := range []struct {
		element      *gst.Element
		pad          string
		trace        *videoFlowTrace
		afterObserve func(*gst.Buffer)
	}{
		{element: source, pad: "src", trace: &trace.source},
		{element: handoff, pad: "src", trace: &trace.handoffOutput},
		{element: sink.Element, pad: "sink", trace: &trace.handoffSink},
	} {
		pad, probeErr := addVideoTraceProbe(probe.element, probe.pad, probe.trace, probe.afterObserve)
		if probeErr != nil {
			for _, addedPad := range probePads {
				addedPad.Unref()
			}
			return nil, probeErr
		}
		probePads = append(probePads, pad)
	}

	result := &persistentVideoPipeline{
		pipeline:       pipeline,
		stream:         stream,
		source:         source,
		handoff:        handoff,
		sink:           sink,
		elements:       captureElements,
		probePads:      probePads,
		emit:           emit,
		active:         active,
		logger:         logger,
		trace:          trace,
		samples:        newVideoSampleSlot(),
		branch:         initialBranch,
		nextGeneration: 2,
		stop:           make(chan struct{}),
		monitorDone:    make(chan struct{}),
		done:           make(chan struct{}),
	}
	sink.SetCallbacks(&gstapp.SinkCallbacks{
		NewSampleFunc: result.handleCaptureSample,
	})
	go result.monitor()
	go result.watchBranch(initialBranch)
	return result, nil
}

func (p *persistentVideoPipeline) Start() error {
	p.branchMu.RLock()
	branch := p.branch
	p.branchMu.RUnlock()
	if branch == nil {
		return errors.Join(errors.New("video encoder branch is unavailable"), p.Close())
	}
	if err := branch.Start(p.samples, videoPipelineReadyTimeout); err != nil {
		return errors.Join(fmt.Errorf("start video encoder branch: %w", err), p.Close())
	}
	if err := p.pipeline.SetState(gst.StatePlaying); err != nil {
		return errors.Join(fmt.Errorf("start video capture pipeline: %w", err), p.Close())
	}
	return nil
}

func (p *persistentVideoPipeline) handleCaptureSample(sink *gstapp.Sink) gst.FlowReturn {
	sample := sink.PullSample()
	if sample == nil {
		return gst.FlowEOS
	}
	runtime.SetFinalizer(sample, nil)
	defer sample.Unref()

	select {
	case <-p.stop:
		return gst.FlowFlushing
	default:
	}
	if !p.active.Load() {
		return gst.FlowOK
	}

	started := time.Now()
	if !p.samples.Store(sample) {
		return gst.FlowFlushing
	}

	routeDuration := time.Since(started)
	p.trace.routeDuration.Store(int64(routeDuration))
	for previousMax := p.trace.maxRouteDuration.Load(); int64(routeDuration) > previousMax; previousMax = p.trace.maxRouteDuration.Load() {
		if p.trace.maxRouteDuration.CompareAndSwap(previousMax, int64(routeDuration)) {
			break
		}
	}
	p.trace.routedBuffers.Add(1)
	return gst.FlowOK
}

func (p *persistentVideoPipeline) RequestKeyframe() error {
	p.branchMu.RLock()
	defer p.branchMu.RUnlock()
	if p.branch == nil {
		return errors.New("video encoder branch is unavailable")
	}
	return p.branch.RequestKeyframe()
}

func (p *persistentVideoPipeline) SetBitrate(bitrateKbps int) error {
	p.replaceMu.Lock()
	defer p.replaceMu.Unlock()

	p.branchMu.RLock()
	defer p.branchMu.RUnlock()
	if p.branch == nil {
		return errors.New("video encoder branch is unavailable")
	}
	return p.branch.SetBitrate(bitrateKbps)
}

func (p *persistentVideoPipeline) ReplaceQuality(
	quality Quality,
	profile EncoderProfile,
	tuning Tuning,
	timeout time.Duration,
) error {
	p.replaceMu.Lock()
	defer p.replaceMu.Unlock()
	started := time.Now()

	p.branchMu.RLock()
	old := p.branch
	p.branchMu.RUnlock()
	if old == nil {
		return errors.New("video encoder branch is unavailable")
	}

	candidate, err := newVideoEncoderBranch(
		p.nextGeneration,
		quality,
		profile,
		tuning,
		p.emit,
		p.logger,
	)
	if err != nil {
		return err
	}
	p.nextGeneration++
	if err := candidate.Start(p.samples, timeout); err != nil {
		return errors.Join(err, candidate.Close())
	}
	go p.watchBranch(candidate)

	p.branchMu.Lock()
	candidateErr := candidate.Err()
	if p.branch != old || candidateErr != nil {
		p.branchMu.Unlock()
		if candidateErr != nil {
			return errors.Join(
				fmt.Errorf("candidate video encoder failed during startup: %w", candidateErr),
				candidate.Close(),
			)
		}
		return errors.Join(
			errors.New("active video encoder changed during replacement"),
			candidate.Close(),
		)
	}
	p.candidate = candidate
	p.branchMu.Unlock()
	overlapFramerate := old.quality.Framerate
	if quality.Framerate > overlapFramerate {
		overlapFramerate = quality.Framerate
	}
	if err := p.source.SetProperty("keepalive-time", 1000/overlapFramerate); err != nil {
		p.branchMu.Lock()
		if p.candidate == candidate {
			p.candidate = nil
		}
		p.branchMu.Unlock()
		return errors.Join(
			fmt.Errorf("update pipewiresrc frame keepalive: %w", err),
			candidate.Close(),
		)
	}
	p.logger.Debug("video encoder candidate started",
		zap.Uint64("generation", candidate.generation),
		zap.String("profile", quality.Profile),
		zap.Duration("elapsed", time.Since(started)),
	)

	rollback := func(cause error) error {
		p.branchMu.Lock()
		if p.candidate == candidate {
			p.candidate = nil
		}
		p.branchMu.Unlock()
		rollbackErr := p.source.SetProperty("keepalive-time", 1000/old.quality.Framerate)
		rollbackErr = errors.Join(rollbackErr, candidate.Close())
		return errors.Join(cause, rollbackErr)
	}

	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-candidate.ready:
		p.logger.Debug("video encoder candidate ready",
			zap.Uint64("generation", candidate.generation),
			zap.String("profile", quality.Profile),
			zap.Duration("elapsed", time.Since(started)),
		)
	case candidateErr := <-candidate.failed:
		return rollback(candidateErr)
	case <-p.done:
		return rollback(errors.Join(
			errors.New("video pipeline stopped during encoder replacement"),
			p.Err(),
		))
	case <-timer.C:
		return rollback(fmt.Errorf(
			"video encoder replacement did not produce an initial keyframe within %s",
			timeout,
		))
	}

	if err := p.source.SetProperty("keepalive-time", 1000/quality.Framerate); err != nil {
		return rollback(fmt.Errorf("set replacement pipewiresrc frame keepalive: %w", err))
	}

	p.branchMu.Lock()
	candidateErr = candidate.Err()
	if p.branch != old || p.candidate != candidate || candidateErr != nil {
		p.branchMu.Unlock()
		if candidateErr != nil {
			return rollback(fmt.Errorf(
				"candidate video encoder failed before promotion: %w",
				candidateErr,
			))
		}
		return rollback(errors.New("video encoder replacement state changed unexpectedly"))
	}
	activationErr := candidate.activateIfHealthy(func() {
		p.branch = candidate
		p.candidate = nil
		old.Stop()
	})
	p.branchMu.Unlock()
	if activationErr != nil {
		return rollback(fmt.Errorf("activate replacement video encoder branch: %w", activationErr))
	}
	p.retireBranch(old)
	p.logger.Info("video encoder branch replaced",
		zap.Uint64("generation", candidate.generation),
		zap.String("profile", quality.Profile),
		zap.String("codec", profile.Codec.ID),
		zap.Int("width", quality.Width),
		zap.Int("height", quality.Height),
		zap.Int("framerate", quality.Framerate),
		zap.Int("bitrate_kbps", quality.BitrateKbps),
		zap.Duration("duration", time.Since(started)),
	)
	return nil
}

func (p *persistentVideoPipeline) retireBranch(branch *videoEncoderBranch) {
	p.retirements.Add(1)
	go func() {
		defer p.retirements.Done()
		if err := branch.Close(); err != nil {
			p.logger.Warn("previous video encoder branch cleanup failed",
				zap.Uint64("generation", branch.generation),
				zap.Error(err),
			)
		}
	}()
}

func (p *persistentVideoPipeline) watchBranch(branch *videoEncoderBranch) {
	<-branch.Done()
	if err := branch.Err(); err != nil {
		p.branchMu.RLock()
		active := p.branch == branch
		p.branchMu.RUnlock()
		if active {
			p.fail(fmt.Errorf(
				"active %s video encoder branch failed: %w",
				branch.quality.Profile,
				err,
			))
		}
	}
}

func (p *persistentVideoPipeline) monitor() {
	defer close(p.monitorDone)

	bus := p.pipeline.GetPipelineBus()
	runtime.SetFinalizer(bus.GObject(), nil)
	defer bus.Unref()
	traceTicker := time.NewTicker(pipelineTraceInterval)
	defer traceTicker.Stop()
	for {
		select {
		case <-p.stop:
			return
		case <-traceTicker.C:
			p.logTraceSnapshot()
		default:
		}

		message := bus.TimedPop(gst.ClockTime(pipelineBusPollInterval))
		if message == nil {
			continue
		}
		runtime.SetFinalizer(message, nil)
		select {
		case <-p.stop:
			message.Unref()
			return
		default:
		}

		source := message.Source()
		switch message.Type() {
		case gst.MessageError:
			gstreamerErr := message.ParseError()
			debug := gstreamerErr.DebugString()
			message.Unref()
			p.logger.Error("gstreamer capture pipeline error",
				zap.String("source", source),
				zap.Error(gstreamerErr),
				zap.String("debug", debug),
			)
			p.fail(fmt.Errorf("gstreamer %s: %w", source, gstreamerErr))
			return
		case gst.MessageEOS:
			message.Unref()
			p.fail(fmt.Errorf("gstreamer capture pipeline %s reached end of stream", source))
			return
		case gst.MessageWarning:
			warning := message.ParseWarning()
			message.Unref()
			p.logger.Warn("gstreamer capture warning",
				zap.String("source", source),
				zap.Error(warning),
			)
		default:
			message.Unref()
		}
	}
}

func (p *persistentVideoPipeline) logTraceSnapshot() {
	now := time.Now().UnixNano()
	fields := []zap.Field{
		zap.Uint64("capture_routed_buffers", p.trace.routedBuffers.Load()),
		zap.Duration("capture_route_duration", time.Duration(p.trace.routeDuration.Load())),
		zap.Duration("capture_max_route_duration", time.Duration(p.trace.maxRouteDuration.Load())),
	}
	for _, property := range []string{"in", "out", "dropped", "current-level-buffers"} {
		if value, err := p.sink.GetProperty(property); err == nil {
			fields = append(fields, zap.Any("capture_appsink_"+strings.ReplaceAll(property, "-", "_"), value))
		}
	}
	if clock := p.pipeline.GetClock(); clock != nil {
		fields = append(fields, zap.String("capture_pipeline_clock", clock.GetName()))
		runtime.SetFinalizer(clock.GObject(), nil)
		clock.Unref()
	}
	if pad := p.handoff.GetStaticPad("src"); pad != nil {
		if caps := pad.GetCurrentCaps(); caps != nil {
			fields = append(fields, zap.String("capture_output_caps", caps.String()))
			runtime.SetFinalizer(caps, nil)
			caps.Unref()
		}
		runtime.SetFinalizer(pad.GObject(), nil)
		pad.Unref()
	}
	fields = appendVideoFlowTraceFields(fields, "source", &p.trace.source, now)
	fields = appendVideoFlowTraceFields(fields, "capture_output", &p.trace.handoffOutput, now)
	fields = appendVideoFlowTraceFields(fields, "capture_appsink", &p.trace.handoffSink, now)

	p.branchMu.RLock()
	if p.branch != nil {
		fields = p.branch.appendTraceFields(fields, "", now)
	}
	if p.candidate != nil {
		fields = p.candidate.appendTraceFields(fields, "candidate", now)
	}
	p.branchMu.RUnlock()
	p.logger.Debug("video pipeline trace snapshot", fields...)
}

func (p *persistentVideoPipeline) Done() <-chan struct{} {
	return p.done
}

func (p *persistentVideoPipeline) Err() error {
	p.errMu.RLock()
	defer p.errMu.RUnlock()
	return p.err
}

func (p *persistentVideoPipeline) Close() error {
	p.closeOnce.Do(func() {
		p.replaceMu.Lock()
		defer p.replaceMu.Unlock()

		close(p.stop)
		p.samples.Close()
		if err := p.pipeline.SetState(gst.StateNull); err != nil {
			p.setErr(fmt.Errorf("stop GStreamer video capture pipeline: %w", err))
		}
		<-p.monitorDone

		p.branchMu.Lock()
		branch := p.branch
		candidate := p.candidate
		p.branch = nil
		p.candidate = nil
		p.branchMu.Unlock()
		for _, encoderBranch := range []*videoEncoderBranch{branch, candidate} {
			if encoderBranch == nil {
				continue
			}
			if err := encoderBranch.Close(); err != nil {
				p.setErr(fmt.Errorf(
					"close %s video encoder branch: %w",
					encoderBranch.quality.Profile,
					err,
				))
			}
		}
		p.retirements.Wait()

		for _, pad := range p.probePads {
			pad.Unref()
		}
		runtime.SetFinalizer(p.pipeline.GObject(), nil)
		p.pipeline.Unref()
		for _, element := range p.elements {
			runtime.SetFinalizer(element.GObject(), nil)
			element.Unref()
		}
		if err := p.stream.Close(); err != nil {
			p.setErr(fmt.Errorf("close persistent pipeline PipeWire remote: %w", err))
		}
		p.doneOnce.Do(func() {
			close(p.done)
		})
	})
	return p.Err()
}

func (p *persistentVideoPipeline) fail(err error) {
	p.setErr(err)
	p.doneOnce.Do(func() {
		close(p.done)
	})
}

func (p *persistentVideoPipeline) setErr(err error) {
	p.errMu.Lock()
	defer p.errMu.Unlock()
	p.err = errors.Join(p.err, err)
}
