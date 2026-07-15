package app

import (
	"errors"
	"fmt"
	"maps"

	remoteinput "github.com/tarik02/webdesktop/input"
	"github.com/tarik02/webdesktop/media"
	rtc "github.com/tarik02/webdesktop/webrtc"
)

type mediaSourceAdapter struct {
	source  *media.Service
	samples chan rtc.VideoSample
}

func newMediaSourceAdapter(source *media.Service) *mediaSourceAdapter {
	adapter := &mediaSourceAdapter{source: source, samples: make(chan rtc.VideoSample)}
	go func() {
		defer close(adapter.samples)
		for sample := range source.Samples() {
			adapter.samples <- rtc.VideoSample{
				Data:       sample.Data,
				Codec:      sample.Codec,
				ProducedAt: sample.ProducedAt,
				PTS:        sample.PTS,
				PTSValid:   sample.PTSValid,
				Duration:   sample.Duration,
				KeyFrame:   sample.KeyFrame,
			}
		}
	}()
	return adapter
}

func (adapter *mediaSourceAdapter) Samples() <-chan rtc.VideoSample {
	return adapter.samples
}

func (adapter *mediaSourceAdapter) Quality() rtc.Quality {
	quality := adapter.source.Quality()
	return rtc.Quality{
		Profile:     quality.Profile,
		Option:      quality.Option,
		Width:       quality.Width,
		Height:      quality.Height,
		Framerate:   quality.Framerate,
		BitrateKbps: quality.BitrateKbps,
	}
}

func (adapter *mediaSourceAdapter) Profile(name string) (rtc.EncoderProfile, bool) {
	profile, exists := adapter.source.Profile(name)
	if !exists {
		return rtc.EncoderProfile{}, false
	}
	options := make(map[string]rtc.QualityOption, len(profile.Options))
	for optionName, option := range profile.Options {
		options[optionName] = rtc.QualityOption{
			Label:       option.Label,
			Width:       option.Width,
			Height:      option.Height,
			Framerate:   option.Framerate,
			BitrateKbps: option.BitrateKbps,
		}
	}
	feedback := make([]rtc.RTCPFeedback, len(profile.Codec.RTCPFeedback))
	for index, item := range profile.Codec.RTCPFeedback {
		feedback[index] = rtc.RTCPFeedback{Type: item.Type, Parameter: item.Parameter}
	}
	return rtc.EncoderProfile{
		DefaultOption:     profile.DefaultOption,
		Options:           options,
		FrontendTransform: profile.FrontendTransform,
		Codec: rtc.RTPCodec{
			ID:           profile.Codec.ID,
			MimeType:     profile.Codec.MimeType,
			ClockRate:    profile.Codec.ClockRate,
			Channels:     profile.Codec.Channels,
			PayloadType:  profile.Codec.PayloadType,
			Payloader:    profile.Codec.Payloader,
			SDPFmtpLine:  profile.Codec.SDPFmtpLine,
			RTCPFeedback: feedback,
			SDP: rtc.SDPRequirements{
				OfferFmtp:  maps.Clone(profile.Codec.SDP.OfferFmtp),
				AnswerFmtp: maps.Clone(profile.Codec.SDP.AnswerFmtp),
			},
		},
	}, true
}

func (adapter *mediaSourceAdapter) UpdateQuality(quality rtc.Quality) error {
	return adapter.source.UpdateQuality(media.Quality{
		Profile:     quality.Profile,
		Option:      quality.Option,
		Width:       quality.Width,
		Height:      quality.Height,
		Framerate:   quality.Framerate,
		BitrateKbps: quality.BitrateKbps,
	})
}

func (adapter *mediaSourceAdapter) RequestKeyframe() error {
	return adapter.source.RequestKeyframe()
}

func (adapter *mediaSourceAdapter) SetActive(active bool) {
	adapter.source.SetActive(active)
}

type audioSourceAdapter struct {
	samples chan rtc.AudioSample
}

func newAudioSourceAdapter(source *media.AudioService) *audioSourceAdapter {
	adapter := &audioSourceAdapter{samples: make(chan rtc.AudioSample, 32)}
	go func() {
		defer close(adapter.samples)
		for sample := range source.Samples() {
			adapter.samples <- rtc.AudioSample{Data: sample.Data, PTS: sample.PTS, Duration: sample.Duration}
		}
	}()
	return adapter
}

func (adapter *audioSourceAdapter) Samples() <-chan rtc.AudioSample {
	return adapter.samples
}

type inputControllerAdapter struct {
	controller *remoteinput.Controller
}

func (adapter inputControllerAdapter) Acquire(owner uint64, revoke func(uint64, error)) (rtc.InputCapabilities, error) {
	capabilities, err := adapter.controller.Acquire(owner, func(sequence uint64, cause error) {
		revoke(sequence, adaptInputError(cause))
	})
	return rtc.InputCapabilities{Pointer: capabilities.Pointer, Keyboard: capabilities.Keyboard}, adaptInputError(err)
}

func (adapter inputControllerAdapter) Release(owner uint64) error {
	return adaptInputError(adapter.controller.Release(owner))
}

func (adapter inputControllerAdapter) Owns(owner uint64) bool {
	return adapter.controller.Owns(owner)
}

func (adapter inputControllerAdapter) Submit(owner uint64, event rtc.InputEvent) error {
	return adaptInputError(adapter.controller.Submit(owner, remoteinput.Event{
		Sequence:       event.Sequence,
		Type:           remoteinput.EventType(event.Type),
		X:              event.X,
		Y:              event.Y,
		DX:             event.DX,
		DY:             event.DY,
		ButtonCode:     event.ButtonCode,
		Keycode:        event.Keycode,
		Pressed:        event.Pressed,
		Horizontal:     event.Horizontal,
		Vertical:       event.Vertical,
		StopHorizontal: event.StopHorizontal,
		StopVertical:   event.StopVertical,
	}))
}

func adaptInputError(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, remoteinput.ErrBusy):
		return rtc.ErrInputBusy
	case errors.Is(err, remoteinput.ErrDisabled):
		return rtc.ErrInputDisabled
	case errors.Is(err, remoteinput.ErrPointerUnauthorized):
		return rtc.ErrInputPointerUnauthorized
	case errors.Is(err, remoteinput.ErrKeyboardUnauthorized):
		return rtc.ErrInputKeyboardUnauthorized
	case errors.Is(err, remoteinput.ErrNotReady):
		return fmt.Errorf("%w: %s", rtc.ErrInputNotReady, err)
	case errors.Is(err, remoteinput.ErrNotOwner):
		return rtc.ErrInputNotOwner
	case errors.Is(err, remoteinput.ErrOverloaded):
		return rtc.ErrInputOverloaded
	case errors.Is(err, remoteinput.ErrClosed):
		return rtc.ErrInputClosed
	default:
		return err
	}
}
