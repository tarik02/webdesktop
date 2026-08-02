package webrtc

import (
	"context"
	"time"

	"github.com/tarik02/webdesktop/clipboard"
	remoteinput "github.com/tarik02/webdesktop/input"
)

const (
	PayloaderVP8  = "vp8"
	PayloaderH264 = "h264"
)

var (
	ErrInputBusy                 = remoteinput.ErrBusy
	ErrInputDisabled             = remoteinput.ErrDisabled
	ErrInputPointerUnauthorized  = remoteinput.ErrPointerUnauthorized
	ErrInputKeyboardUnauthorized = remoteinput.ErrKeyboardUnauthorized
	ErrInputNotReady             = remoteinput.ErrNotReady
	ErrInputNotOwner             = remoteinput.ErrNotOwner
	ErrInputOverloaded           = remoteinput.ErrOverloaded
	ErrInputClosed               = remoteinput.ErrClosed
)

// Quality contains runtime-adjustable video settings.
type Quality struct {
	Profile     string `json:"profile"`
	Option      string `json:"option"`
	Width       int    `json:"width"`
	Height      int    `json:"height"`
	Framerate   int    `json:"framerate"`
	BitrateKbps int    `json:"bitrate_kbps"`
}

// QualityOption is one complete video quality tuple exposed to clients.
type QualityOption struct {
	Label       string `json:"label"`
	Width       int    `json:"width"`
	Height      int    `json:"height"`
	Framerate   int    `json:"framerate"`
	BitrateKbps int    `json:"bitrate_kbps"`
}

// Quality resolves this option into runtime encoder settings.
func (option QualityOption) Quality(profileName, optionName string) Quality {
	return Quality{
		Profile:     profileName,
		Option:      optionName,
		Width:       option.Width,
		Height:      option.Height,
		Framerate:   option.Framerate,
		BitrateKbps: option.BitrateKbps,
	}
}

// EncoderProfile contains the profile metadata needed by the WebRTC transport.
type EncoderProfile struct {
	DefaultOption     string
	Options           map[string]QualityOption
	FrontendTransform string
	Codec             RTPCodec
}

// RTPCodec contains codec, RTP, packetizer, and SDP settings.
type RTPCodec struct {
	ID           string
	MimeType     string
	ClockRate    uint32
	Channels     uint16
	PayloadType  uint8
	Payloader    string
	SDPFmtpLine  string
	RTCPFeedback []RTCPFeedback
	SDP          SDPRequirements
}

// RTCPFeedback describes one negotiated RTCP feedback mechanism.
type RTCPFeedback struct {
	Type      string
	Parameter string
}

// SDPRequirements controls offer validation and answer parameter rewriting.
type SDPRequirements struct {
	OfferFmtp  map[string]string
	AnswerFmtp map[string]string
}

// Compatible reports whether two codec definitions can share an active peer connection.
func (codec RTPCodec) Compatible(other RTPCodec) bool {
	if codec.ID != other.ID ||
		codec.MimeType != other.MimeType ||
		codec.ClockRate != other.ClockRate ||
		codec.Channels != other.Channels ||
		codec.PayloadType != other.PayloadType ||
		codec.Payloader != other.Payloader ||
		codec.SDPFmtpLine != other.SDPFmtpLine ||
		len(codec.RTCPFeedback) != len(other.RTCPFeedback) ||
		len(codec.SDP.OfferFmtp) != len(other.SDP.OfferFmtp) ||
		len(codec.SDP.AnswerFmtp) != len(other.SDP.AnswerFmtp) {
		return false
	}
	for index := range codec.RTCPFeedback {
		if codec.RTCPFeedback[index] != other.RTCPFeedback[index] {
			return false
		}
	}
	for key, value := range codec.SDP.OfferFmtp {
		if other.SDP.OfferFmtp[key] != value {
			return false
		}
	}
	for key, value := range codec.SDP.AnswerFmtp {
		if other.SDP.AnswerFmtp[key] != value {
			return false
		}
	}
	return true
}

// VideoSample is one encoded video frame ready for transport.
type VideoSample struct {
	Data       []byte
	Codec      string
	TargetID   string
	ProducedAt time.Time
	PTS        time.Duration
	PTSValid   bool
	Duration   time.Duration
	KeyFrame   bool
}

// TargetSelection describes the source selected for one peer.
type TargetSelection struct {
	TargetID          string  `json:"target_id"`
	Generation        uint64  `json:"generation"`
	Width             int     `json:"width"`
	Height            int     `json:"height"`
	PhysicalWidth     int     `json:"physical_width"`
	PhysicalHeight    int     `json:"physical_height"`
	DeviceScaleFactor float64 `json:"device_scale_factor"`
}

// TargetEventKind identifies a selected target lifecycle change.
type TargetEventKind string

const (
	TargetEventUpdated     TargetEventKind = "updated"
	TargetEventUnavailable TargetEventKind = "unavailable"
)

// TargetEvent reports a change for one peer's selected target.
type TargetEvent struct {
	Kind     TargetEventKind
	PeerID   uint64
	Target   TargetSelection
	TargetID string
	Reason   string
}

// AudioSample is one encoded Opus frame ready for transport.
type AudioSample struct {
	Data     []byte
	PTS      time.Duration
	Duration time.Duration
}

// MediaSource supplies one shared encoded video stream.
type MediaSource interface {
	Samples() <-chan VideoSample
	Quality() Quality
	Profile(string) (EncoderProfile, bool)
	UpdateQuality(Quality) error
	SetBitrate(int) error
	RequestKeyframe() error
	SetActive(bool)
}

// TargetMediaSource routes tagged samples and keyframe requests per peer.
// SelectTarget returns after the source has accepted a frame for the new target.
type TargetMediaSource interface {
	SelectTarget(context.Context, uint64, uint64, string) (TargetSelection, error)
	ReleaseTarget(uint64)
	AcceptsTarget(uint64, string) bool
	ConfirmTargetFrame(uint64, string)
	RequestTargetKeyframe(uint64) error
}

// TargetEventSource reports selected target replacement and loss events.
type TargetEventSource interface {
	TargetEvents() <-chan TargetEvent
}

// AudioSource supplies optional encoded Opus audio.
type AudioSource interface {
	Samples() <-chan AudioSample
}

// InputCapabilities reports the input classes available to a peer.
type InputCapabilities = remoteinput.Capabilities

// InputEventType identifies one input event.
type InputEventType = remoteinput.EventType

const (
	InputEventPointerAbsolute = remoteinput.EventPointerAbsolute
	InputEventPointerRelative = remoteinput.EventPointerRelative
	InputEventPointerButton   = remoteinput.EventPointerButton
	InputEventPointerScroll   = remoteinput.EventPointerScroll
	InputEventKeyboardKey     = remoteinput.EventKeyboardKey
	InputEventKeyboardText    = remoteinput.EventKeyboardText
)

// InputEvent is one validated remote input transition or motion.
type InputEvent = remoteinput.Event

// InputController owns peer input leases and dispatches validated events.
type InputController interface {
	Acquire(uint64, func(uint64, error)) (InputCapabilities, error)
	Release(uint64) error
	Owns(uint64) bool
	Submit(uint64, InputEvent) error
}

// ClipboardController synchronizes clipboard content with peers.
type ClipboardController interface {
	Enabled() bool
	Set(context.Context, clipboard.Content) error
	Latest() (clipboard.Content, bool)
	Subscribe() (<-chan clipboard.Content, func())
}

// PeerInfo identifies one transport peer.
type PeerInfo struct {
	ID          uint64
	ActivePeers int
}

// Observer receives peer lifecycle notifications.
type Observer interface {
	PeerOpened(PeerInfo)
	PeerStateChanged(PeerInfo, string)
	PeerClosed(PeerInfo)
}

type disabledInputController struct{}

func (disabledInputController) Acquire(uint64, func(uint64, error)) (InputCapabilities, error) {
	return InputCapabilities{}, ErrInputDisabled
}

func (disabledInputController) Release(uint64) error {
	return ErrInputNotOwner
}

func (disabledInputController) Owns(uint64) bool {
	return false
}

func (disabledInputController) Submit(uint64, InputEvent) error {
	return ErrInputNotOwner
}

type disabledClipboardController struct{}

func (disabledClipboardController) Enabled() bool {
	return false
}

func (disabledClipboardController) Set(context.Context, clipboard.Content) error {
	return clipboard.ErrDisabled
}

func (disabledClipboardController) Latest() (clipboard.Content, bool) {
	return clipboard.Content{}, false
}

func (disabledClipboardController) Subscribe() (<-chan clipboard.Content, func()) {
	return nil, func() {}
}
