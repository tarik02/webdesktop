package app

import (
	"maps"

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
