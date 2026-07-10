package media

import (
	"errors"
	"fmt"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/go-gst/go-gst/gst"
	gstapp "github.com/go-gst/go-gst/gst/app"
	"go.uber.org/zap"
)

type audioPipeline struct {
	pipeline *gst.Pipeline
	elements []*gst.Element
	source   *gst.Element
	cfg      AudioConfig
	emit     func(AudioSample)
	logger   *zap.Logger

	stop        chan struct{}
	monitorDone chan struct{}
	done        chan struct{}
	ready       chan struct{}
	closeOnce   sync.Once
	doneOnce    sync.Once
	readyOnce   sync.Once
	deviceMu    sync.Mutex
	deliveryMu  sync.Mutex
	errMu       sync.RWMutex
	err         error

	deviceValid           atomic.Bool
	deviceGeneration      atomic.Uint64
	validatedGeneration   atomic.Uint64
	started               atomic.Bool
	deliver               atomic.Bool
	closing               atomic.Bool
	disconnectDeviceWatch func()
}

func newAudioPipeline(
	cfg AudioConfig,
	emit func(AudioSample),
	logger *zap.Logger,
) (*audioPipeline, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}

	elements, err := gst.NewElementMany(
		"pulsesrc",
		"queue",
		"audioconvert",
		"audioresample",
		"capsfilter",
	)
	if err != nil {
		return nil, fmt.Errorf("create raw audio elements: %w", err)
	}
	source := elements[0]
	queue := elements[1]
	convert := elements[2]
	resample := elements[3]
	rawFilter := elements[4]

	if err := source.SetProperty("device", cfg.Device); err != nil {
		return nil, fmt.Errorf("set pulsesrc monitor device: %w", err)
	}
	if err := source.SetProperty("client-name", "webdesktop"); err != nil {
		return nil, fmt.Errorf("set pulsesrc client name: %w", err)
	}
	if err := source.SetProperty("do-timestamp", true); err != nil {
		return nil, fmt.Errorf("enable pulsesrc timestamps: %w", err)
	}
	if err := source.SetProperty("buffer-time", int64(100000)); err != nil {
		return nil, fmt.Errorf("set pulsesrc buffer time: %w", err)
	}
	if err := source.SetProperty("latency-time", int64(20000)); err != nil {
		return nil, fmt.Errorf("set pulsesrc latency time: %w", err)
	}

	if err := queue.SetProperty("max-size-buffers", uint(8)); err != nil {
		return nil, fmt.Errorf("set audio queue buffer limit: %w", err)
	}
	if err := queue.SetProperty("max-size-bytes", uint(0)); err != nil {
		return nil, fmt.Errorf("disable audio queue byte limit: %w", err)
	}
	if err := queue.SetProperty("max-size-time", uint64(0)); err != nil {
		return nil, fmt.Errorf("disable audio queue time limit: %w", err)
	}
	queue.SetArg("leaky", "downstream")

	rawCaps := gst.NewCapsFromString("audio/x-raw,format=S16LE,layout=interleaved,rate=48000,channels=2")
	if rawCaps == nil {
		return nil, errors.New("create raw audio caps")
	}
	if err := rawFilter.SetProperty("caps", rawCaps); err != nil {
		return nil, fmt.Errorf("set raw audio caps: %w", err)
	}

	encoder, err := gst.NewElementWithName("opusenc", "audio-encoder")
	if err != nil {
		return nil, fmt.Errorf("create Opus encoder: %w", err)
	}
	if err := encoder.SetProperty("bitrate", cfg.BitrateKbps*1000); err != nil {
		return nil, fmt.Errorf("set Opus bitrate: %w", err)
	}
	if err := encoder.SetProperty("inband-fec", true); err != nil {
		return nil, fmt.Errorf("enable Opus in-band FEC: %w", err)
	}
	if err := encoder.SetProperty("packet-loss-percentage", 5); err != nil {
		return nil, fmt.Errorf("set Opus packet loss estimate: %w", err)
	}
	encoder.SetArg("frame-size", "20")
	encoder.SetArg("audio-type", "generic")

	encodedFilter, err := gst.NewElement("capsfilter")
	if err != nil {
		return nil, fmt.Errorf("create encoded audio caps filter: %w", err)
	}
	encodedCaps := gst.NewCapsFromString("audio/x-opus,rate=48000,channels=2,channel-mapping-family=0")
	if encodedCaps == nil {
		return nil, errors.New("create encoded audio caps")
	}
	if err := encodedFilter.SetProperty("caps", encodedCaps); err != nil {
		return nil, fmt.Errorf("set encoded audio caps: %w", err)
	}

	sink, err := gstapp.NewAppSink()
	if err != nil {
		return nil, fmt.Errorf("create encoded audio app sink: %w", err)
	}
	sink.SetMaxBuffers(16)
	sink.SetDrop(true)
	sink.SetWaitOnEOS(false)
	if err := sink.SetProperty("sync", false); err != nil {
		return nil, fmt.Errorf("disable audio app sink synchronization: %w", err)
	}

	pipeline, err := gst.NewPipeline("webdesktop-audio")
	if err != nil {
		return nil, err
	}
	if err := pipeline.AddMany(
		source,
		queue,
		convert,
		resample,
		rawFilter,
		encoder,
		encodedFilter,
		sink.Element,
	); err != nil {
		return nil, fmt.Errorf("add audio pipeline elements: %w", err)
	}
	if err := gst.ElementLinkMany(
		source,
		queue,
		convert,
		resample,
		rawFilter,
		encoder,
		encodedFilter,
		sink.Element,
	); err != nil {
		return nil, fmt.Errorf("link audio pipeline: %w", err)
	}

	result := &audioPipeline{
		pipeline: pipeline,
		elements: []*gst.Element{
			source,
			queue,
			convert,
			resample,
			rawFilter,
			encoder,
			encodedFilter,
			sink.Element,
		},
		source:      source,
		cfg:         cfg,
		emit:        emit,
		logger:      logger,
		stop:        make(chan struct{}),
		monitorDone: make(chan struct{}),
		done:        make(chan struct{}),
		ready:       make(chan struct{}),
	}
	sink.SetCallbacks(&gstapp.SinkCallbacks{
		NewSampleFunc: result.handleSample,
	})

	go result.monitor()
	deviceNotify, err := source.Connect("notify::current-device", func() {
		result.deliveryMu.Lock()
		result.deviceGeneration.Add(1)
		result.deliver.Store(false)
		result.deliveryMu.Unlock()
		result.checkDevice(true)
	})
	if err != nil {
		_ = result.Close()
		return nil, fmt.Errorf("watch pulsesrc device: %w", err)
	}
	result.disconnectDeviceWatch = func() {
		source.HandlerDisconnect(deviceNotify)
	}

	if err := pipeline.SetState(gst.StatePlaying); err != nil {
		_ = result.Close()
		return nil, err
	}

	select {
	case <-result.ready:
	case <-result.done:
		err := result.Err()
		_ = result.Close()
		if err != nil {
			return nil, err
		}
		return nil, errors.New("audio pipeline stopped during startup")
	case <-time.After(5 * time.Second):
		_ = result.Close()
		return nil, errors.New("audio monitor did not produce data within 5 seconds")
	}

	result.deviceMu.Lock()
	validatedGeneration := result.deviceGeneration.Load()
	if err := result.validateDevice(source, cfg); err != nil {
		result.deviceMu.Unlock()
		_ = result.Close()
		return nil, err
	}
	result.deviceValid.Store(true)
	result.validatedGeneration.Store(validatedGeneration)
	result.started.Store(true)
	result.deliveryMu.Lock()
	result.deliver.Store(validatedGeneration == result.deviceGeneration.Load())
	result.deliveryMu.Unlock()
	result.deviceMu.Unlock()

	return result, nil
}

func (p *audioPipeline) checkDevice(pause bool) bool {
	p.deviceMu.Lock()
	defer p.deviceMu.Unlock()
	if p.closing.Load() {
		return false
	}

	if err := p.validateDevice(p.source, p.cfg); err != nil {
		p.deviceValid.Store(false)
		p.deliveryMu.Lock()
		p.deliver.Store(false)
		p.deliveryMu.Unlock()
		if !p.closing.Load() {
			p.fail(err)
		}
		return false
	}
	if p.closing.Load() {
		p.deliveryMu.Lock()
		p.deliver.Store(false)
		p.deviceValid.Store(false)
		p.deliveryMu.Unlock()
		return false
	}
	p.deviceValid.Store(true)
	if pause {
		p.validatedGeneration.Store(p.deviceGeneration.Load())
	}
	p.deliveryMu.Lock()
	if p.closing.Load() {
		p.deliver.Store(false)
		p.deviceValid.Store(false)
		p.deliveryMu.Unlock()
		return false
	}
	if p.started.Load() &&
		p.validatedGeneration.Load() == p.deviceGeneration.Load() {
		p.deliver.Store(true)
	}
	p.deliveryMu.Unlock()
	return true
}

func (p *audioPipeline) validateDevice(source *gst.Element, cfg AudioConfig) error {
	currentDeviceValue, err := source.GetProperty("current-device")
	if err != nil {
		return fmt.Errorf("read active pulsesrc device: %w", err)
	}
	currentDevice, ok := currentDeviceValue.(string)
	if !ok || !strings.HasSuffix(currentDevice, ".monitor") {
		return fmt.Errorf("pulsesrc resolved desktop audio to non-monitor source %q", currentDevice)
	}
	if cfg.Device != DefaultAudioMonitor && currentDevice != cfg.Device {
		return fmt.Errorf("pulsesrc resolved configured monitor %q to %q", cfg.Device, currentDevice)
	}
	return nil
}

func (p *audioPipeline) handleSample(sink *gstapp.Sink) gst.FlowReturn {
	sample := sink.PullSample()
	if sample == nil {
		return gst.FlowEOS
	}
	runtime.SetFinalizer(sample, nil)
	defer sample.Unref()

	buffer := sample.GetBuffer()
	runtime.SetFinalizer(buffer, nil)
	defer buffer.Unref()

	var pts time.Duration
	if value := buffer.PresentationTimestamp().AsDuration(); value != nil {
		pts = *value
	}
	var duration time.Duration
	if value := buffer.Duration().AsDuration(); value != nil {
		duration = *value
	}

	p.readyOnce.Do(func() {
		close(p.ready)
	})
	p.deviceMu.Lock()
	p.deliveryMu.Lock()
	if p.closing.Load() {
		p.deliver.Store(false)
		p.deviceValid.Store(false)
		p.deliveryMu.Unlock()
		p.deviceMu.Unlock()
		return gst.FlowOK
	}
	if !p.deliver.Load() ||
		!p.deviceValid.Load() ||
		p.validatedGeneration.Load() != p.deviceGeneration.Load() {
		p.deliveryMu.Unlock()
		p.deviceMu.Unlock()
		return gst.FlowOK
	}
	if err := p.validateDevice(p.source, p.cfg); err != nil {
		p.deliver.Store(false)
		p.deviceValid.Store(false)
		p.deliveryMu.Unlock()
		p.deviceMu.Unlock()
		p.fail(err)
		return gst.FlowOK
	}
	if p.closing.Load() {
		p.deliver.Store(false)
		p.deviceValid.Store(false)
		p.deliveryMu.Unlock()
		p.deviceMu.Unlock()
		return gst.FlowOK
	}
	p.emit(AudioSample{
		Data:     buffer.Bytes(),
		PTS:      pts,
		Duration: duration,
	})
	p.deliveryMu.Unlock()
	p.deviceMu.Unlock()
	return gst.FlowOK
}

func (p *audioPipeline) monitor() {
	defer close(p.monitorDone)

	bus := p.pipeline.GetPipelineBus()
	for {
		select {
		case <-p.stop:
			return
		default:
		}

		message := bus.TimedPop(gst.ClockTime(pipelineBusPollInterval))
		if message == nil {
			if p.started.Load() && !p.checkDevice(false) {
				return
			}
			continue
		}

		switch message.Type() {
		case gst.MessageError:
			p.fail(fmt.Errorf("gstreamer %s: %w", message.Source(), message.ParseError()))
			return
		case gst.MessageEOS:
			p.fail(errors.New("gstreamer audio pipeline reached end of stream"))
			return
		case gst.MessageWarning:
			p.logger.Warn("gstreamer warning",
				zap.String("source", message.Source()),
				zap.Error(message.ParseWarning()),
			)
		}
	}
}

func (p *audioPipeline) Done() <-chan struct{} {
	return p.done
}

func (p *audioPipeline) Err() error {
	p.errMu.RLock()
	defer p.errMu.RUnlock()
	return p.err
}

func (p *audioPipeline) Close() error {
	p.closeOnce.Do(func() {
		p.closing.Store(true)
		p.deviceMu.Lock()
		p.deliveryMu.Lock()
		p.deliver.Store(false)
		p.deviceValid.Store(false)
		p.deliveryMu.Unlock()
		if p.disconnectDeviceWatch != nil {
			p.disconnectDeviceWatch()
			p.disconnectDeviceWatch = nil
		}
		p.deviceMu.Unlock()

		close(p.stop)
		if err := p.pipeline.SetState(gst.StateNull); err != nil {
			p.setErr(fmt.Errorf("stop gstreamer audio pipeline: %w", err))
		}
		<-p.monitorDone

		pipelineObject := p.pipeline.GObject()
		runtime.SetFinalizer(pipelineObject, nil)
		p.pipeline.Unref()
		for _, element := range p.elements {
			elementObject := element.GObject()
			runtime.SetFinalizer(elementObject, nil)
			element.Unref()
		}

		p.doneOnce.Do(func() {
			close(p.done)
		})
	})
	return p.Err()
}

func (p *audioPipeline) fail(err error) {
	p.setErr(err)
	p.doneOnce.Do(func() {
		close(p.done)
	})
}

func (p *audioPipeline) setErr(err error) {
	p.errMu.Lock()
	defer p.errMu.Unlock()
	p.err = errors.Join(p.err, err)
}
