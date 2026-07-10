package media

import (
	"errors"
	"fmt"
	"runtime"
	"strconv"
	"sync"
	"time"

	"github.com/go-gst/go-gst/gst"
	gstapp "github.com/go-gst/go-gst/gst/app"
	"github.com/tarik02/webdesktop/capture"
	"go.uber.org/zap"
)

const pipelineBusPollInterval = 100 * time.Millisecond

type videoPipeline struct {
	pipeline       *gst.Pipeline
	elements       []*gst.Element
	stream         *capture.Stream
	encoder        *gst.Element
	keyframeTarget *gst.Element
	codec          string
	emit           func(Sample)
	logger         *zap.Logger

	stop        chan struct{}
	monitorDone chan struct{}
	done        chan struct{}
	closeOnce   sync.Once
	doneOnce    sync.Once
	errMu       sync.RWMutex
	err         error
}

// newVideoPipeline takes ownership of stream on both success and failure.
func newVideoPipeline(
	stream *capture.Stream,
	quality Quality,
	tuning Tuning,
	emit func(Sample),
	logger *zap.Logger,
) (_ *videoPipeline, err error) {
	defer func() {
		if err != nil {
			err = errors.Join(err, stream.Close())
		}
	}()

	if err := quality.Validate(); err != nil {
		return nil, err
	}

	elements, err := gst.NewElementMany(
		"pipewiresrc",
		"queue",
		"videoconvert",
		"videorate",
		"videoscale",
		"capsfilter",
	)
	if err != nil {
		return nil, fmt.Errorf("create raw video elements: %w", err)
	}
	source := elements[0]
	queue := elements[1]
	convert := elements[2]
	rate := elements[3]
	scale := elements[4]
	rawFilter := elements[5]

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

	if err := queue.SetProperty("max-size-buffers", uint(2)); err != nil {
		return nil, fmt.Errorf("set queue buffer limit: %w", err)
	}
	if err := queue.SetProperty("max-size-bytes", uint(0)); err != nil {
		return nil, fmt.Errorf("disable queue byte limit: %w", err)
	}
	if err := queue.SetProperty("max-size-time", uint64(0)); err != nil {
		return nil, fmt.Errorf("disable queue time limit: %w", err)
	}
	queue.SetArg("leaky", "downstream")

	rawCaps := gst.NewCapsFromString(fmt.Sprintf(
		"video/x-raw,format=I420,width=%d,height=%d,framerate=%d/1",
		quality.Width,
		quality.Height,
		quality.Framerate,
	))
	if rawCaps == nil {
		return nil, errors.New("create raw video caps")
	}
	if err := rawFilter.SetProperty("caps", rawCaps); err != nil {
		return nil, fmt.Errorf("set raw video caps: %w", err)
	}

	encoderFactory := "vp8enc"
	encodedCapsString := "video/x-vp8"
	if quality.Codec == CodecH264 {
		encoderFactory = "x264enc"
		encodedCapsString = "video/x-h264,stream-format=byte-stream,alignment=au,profile=constrained-baseline,level=(string)" + H264Level
	}

	encoder, err := gst.NewElementWithName(encoderFactory, "video-encoder")
	if err != nil {
		return nil, fmt.Errorf("create %s: %w", encoderFactory, err)
	}

	switch quality.Codec {
	case CodecVP8:
		if err := encoder.SetProperty("target-bitrate", quality.BitrateKbps*1000); err != nil {
			return nil, fmt.Errorf("set VP8 target bitrate: %w", err)
		}
		if err := encoder.SetProperty("cpu-used", tuning.VP8CPUUsed); err != nil {
			return nil, fmt.Errorf("set VP8 CPU tuning: %w", err)
		}
		if err := encoder.SetProperty("threads", tuning.Threads); err != nil {
			return nil, fmt.Errorf("set VP8 encoder threads: %w", err)
		}
		if err := encoder.SetProperty("deadline", int64(1)); err != nil {
			return nil, fmt.Errorf("set VP8 realtime deadline: %w", err)
		}
		if err := encoder.SetProperty("lag-in-frames", 0); err != nil {
			return nil, fmt.Errorf("disable VP8 frame lag: %w", err)
		}
		if err := encoder.SetProperty("keyframe-max-dist", tuning.KeyframeInterval); err != nil {
			return nil, fmt.Errorf("set VP8 keyframe interval: %w", err)
		}
		encoder.SetArg("end-usage", "cbr")
		encoder.SetArg("error-resilient", "partitions")
	case CodecH264:
		if err := encoder.SetProperty("bitrate", uint(quality.BitrateKbps)); err != nil {
			return nil, fmt.Errorf("set H.264 bitrate: %w", err)
		}
		if err := encoder.SetProperty("threads", uint(tuning.Threads)); err != nil {
			return nil, fmt.Errorf("set H.264 encoder threads: %w", err)
		}
		if err := encoder.SetProperty("key-int-max", uint(tuning.KeyframeInterval)); err != nil {
			return nil, fmt.Errorf("set H.264 keyframe interval: %w", err)
		}
		if err := encoder.SetProperty("bframes", uint(0)); err != nil {
			return nil, fmt.Errorf("disable H.264 B-frames: %w", err)
		}
		if err := encoder.SetProperty("byte-stream", true); err != nil {
			return nil, fmt.Errorf("enable H.264 byte stream output: %w", err)
		}
		encoder.SetArg("tune", "zerolatency")
		encoder.SetArg("speed-preset", tuning.H264SpeedPreset)
	}

	encodedFilter, err := gst.NewElement("capsfilter")
	if err != nil {
		return nil, fmt.Errorf("create encoded video caps filter: %w", err)
	}
	encodedCaps := gst.NewCapsFromString(encodedCapsString)
	if encodedCaps == nil {
		return nil, errors.New("create encoded video caps")
	}
	if err := encodedFilter.SetProperty("caps", encodedCaps); err != nil {
		return nil, fmt.Errorf("set encoded video caps: %w", err)
	}

	sink, err := gstapp.NewAppSink()
	if err != nil {
		return nil, fmt.Errorf("create encoded video app sink: %w", err)
	}
	sink.SetMaxBuffers(4)
	sink.SetDrop(true)
	sink.SetWaitOnEOS(false)
	if err := sink.SetProperty("sync", false); err != nil {
		return nil, fmt.Errorf("disable app sink synchronization: %w", err)
	}

	pipeline, err := gst.NewPipeline("webdesktop-video")
	if err != nil {
		return nil, err
	}
	if err := pipeline.AddMany(
		source,
		queue,
		convert,
		rate,
		scale,
		rawFilter,
		encoder,
		encodedFilter,
		sink.Element,
	); err != nil {
		return nil, fmt.Errorf("add video pipeline elements: %w", err)
	}
	if err := gst.ElementLinkMany(
		source,
		queue,
		convert,
		rate,
		scale,
		rawFilter,
		encoder,
		encodedFilter,
		sink.Element,
	); err != nil {
		return nil, fmt.Errorf("link video pipeline: %w", err)
	}

	result := &videoPipeline{
		pipeline: pipeline,
		elements: []*gst.Element{
			source,
			queue,
			convert,
			rate,
			scale,
			rawFilter,
			encoder,
			encodedFilter,
			sink.Element,
		},
		stream:         stream,
		encoder:        encoder,
		keyframeTarget: sink.Element,
		codec:          quality.Codec,
		emit:           emit,
		logger:         logger,
		stop:           make(chan struct{}),
		monitorDone:    make(chan struct{}),
		done:           make(chan struct{}),
	}
	sink.SetCallbacks(&gstapp.SinkCallbacks{
		NewSampleFunc: result.handleSample,
	})

	go result.monitor()
	if err := pipeline.SetState(gst.StatePlaying); err != nil {
		_ = result.Close()
		return nil, err
	}

	return result, nil
}

func (p *videoPipeline) handleSample(sink *gstapp.Sink) gst.FlowReturn {
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

	p.emit(Sample{
		Data:     buffer.Bytes(),
		Codec:    p.codec,
		PTS:      pts,
		Duration: duration,
		KeyFrame: !buffer.HasFlags(gst.BufferFlagDeltaUnit),
	})
	return gst.FlowOK
}

func (p *videoPipeline) monitor() {
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
			continue
		}

		switch message.Type() {
		case gst.MessageError:
			p.fail(fmt.Errorf("gstreamer %s: %w", message.Source(), message.ParseError()))
			return
		case gst.MessageEOS:
			p.fail(errors.New("gstreamer video pipeline reached end of stream"))
			return
		case gst.MessageWarning:
			p.logger.Warn("gstreamer warning",
				zap.String("source", message.Source()),
				zap.Error(message.ParseWarning()),
			)
		}
	}
}

func (p *videoPipeline) SetBitrate(bitrateKbps int) error {
	switch p.codec {
	case CodecVP8:
		return p.encoder.SetProperty("target-bitrate", bitrateKbps*1000)
	case CodecH264:
		return p.encoder.SetProperty("bitrate", uint(bitrateKbps))
	default:
		return fmt.Errorf("unsupported live bitrate update for codec %q", p.codec)
	}
}

func (p *videoPipeline) RequestKeyframe() error {
	if !requestGStreamerKeyframe(p.keyframeTarget) {
		return errors.New("GStreamer rejected the force-key-unit event")
	}
	return nil
}

func (p *videoPipeline) Done() <-chan struct{} {
	return p.done
}

func (p *videoPipeline) Err() error {
	p.errMu.RLock()
	defer p.errMu.RUnlock()
	return p.err
}

func (p *videoPipeline) Close() error {
	p.closeOnce.Do(func() {
		close(p.stop)
		if err := p.pipeline.SetState(gst.StateNull); err != nil {
			p.setErr(fmt.Errorf("stop gstreamer video pipeline: %w", err))
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
		if err := p.stream.Close(); err != nil {
			p.setErr(fmt.Errorf("close pipeline PipeWire remote: %w", err))
		}

		p.doneOnce.Do(func() {
			close(p.done)
		})
	})
	return p.Err()
}

func (p *videoPipeline) fail(err error) {
	p.setErr(err)
	p.doneOnce.Do(func() {
		close(p.done)
	})
}

func (p *videoPipeline) setErr(err error) {
	p.errMu.Lock()
	defer p.errMu.Unlock()
	p.err = errors.Join(p.err, err)
}
