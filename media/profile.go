package media

import (
	"bytes"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"text/template"
)

const (
	PayloaderVP8  = "vp8"
	PayloaderH264 = "h264"

	PropertyTypeInt  = "int"
	PropertyTypeUint = "uint"
)

var profileIdentifierPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]*$`)

// EncoderProfile describes an encoder pipeline and its WebRTC codec metadata.
type EncoderProfile struct {
	Label          string            `mapstructure:"label" yaml:"label"`
	Pipeline       string            `mapstructure:"pipeline" yaml:"pipeline"`
	EncoderElement string            `mapstructure:"encoder_element" yaml:"encoder_element"`
	Bitrate        []EncoderProperty `mapstructure:"bitrate" yaml:"bitrate"`
	Codec          RTPCodec          `mapstructure:"codec" yaml:"codec"`
	Limits         QualityLimits     `mapstructure:"limits" yaml:"limits"`
}

// EncoderProperty describes one live encoder property update.
type EncoderProperty struct {
	Element  string `mapstructure:"element" yaml:"element"`
	Property string `mapstructure:"property" yaml:"property"`
	Type     string `mapstructure:"type" yaml:"type"`
	Value    string `mapstructure:"value" yaml:"value"`
}

// RTPCodec contains the codec, RTP, packetizer, and SDP settings for a profile.
type RTPCodec struct {
	ID           string          `mapstructure:"id" yaml:"id"`
	MimeType     string          `mapstructure:"mime_type" yaml:"mime_type"`
	ClockRate    uint32          `mapstructure:"clock_rate" yaml:"clock_rate"`
	Channels     uint16          `mapstructure:"channels" yaml:"channels"`
	PayloadType  uint8           `mapstructure:"payload_type" yaml:"payload_type"`
	Payloader    string          `mapstructure:"payloader" yaml:"payloader"`
	SDPFmtpLine  string          `mapstructure:"sdp_fmtp_line" yaml:"sdp_fmtp_line"`
	RTCPFeedback []RTCPFeedback  `mapstructure:"rtcp_feedback" yaml:"rtcp_feedback"`
	SDP          SDPRequirements `mapstructure:"sdp" yaml:"sdp"`
}

// RTCPFeedback describes one negotiated RTCP feedback mechanism.
type RTCPFeedback struct {
	Type      string `mapstructure:"type" yaml:"type"`
	Parameter string `mapstructure:"parameter" yaml:"parameter"`
}

// SDPRequirements controls generic offer validation and answer parameter rewriting.
type SDPRequirements struct {
	OfferFmtp  map[string]string `mapstructure:"offer_fmtp" yaml:"offer_fmtp"`
	AnswerFmtp map[string]string `mapstructure:"answer_fmtp" yaml:"answer_fmtp"`
}

// QualityLimits contains profile-specific encoded-video limits. Zero disables a limit.
type QualityLimits struct {
	MaxBitrateKbps             int `mapstructure:"max_bitrate_kbps" yaml:"max_bitrate_kbps" json:"max_bitrate_kbps"`
	MaxMacroblocksPerDimension int `mapstructure:"max_macroblocks_per_dimension" yaml:"max_macroblocks_per_dimension" json:"max_macroblocks_per_dimension"`
	MaxMacroblocksPerFrame     int `mapstructure:"max_macroblocks_per_frame" yaml:"max_macroblocks_per_frame" json:"max_macroblocks_per_frame"`
	MaxMacroblocksPerSecond    int `mapstructure:"max_macroblocks_per_second" yaml:"max_macroblocks_per_second" json:"max_macroblocks_per_second"`
}

type profileTemplateData struct {
	Width            int
	Height           int
	Framerate        int
	BitrateKbps      int
	Threads          int
	KeyframeInterval int
	VP8CPUUsed       int
	prefix           string
}

// Validate checks one configured encoder profile and its templates.
func (profile EncoderProfile) Validate(name string, quality Quality, tuning Tuning) error {
	var errs []error
	if !profileIdentifierPattern.MatchString(name) {
		errs = append(errs, fmt.Errorf("video profile name %q must contain lowercase letters, numbers, underscores, or hyphens", name))
	}
	if profile.Label == "" {
		errs = append(errs, fmt.Errorf("video profile %q label is required", name))
	}
	if profile.Pipeline == "" {
		errs = append(errs, fmt.Errorf("video profile %q pipeline is required", name))
	}
	if !profileIdentifierPattern.MatchString(profile.EncoderElement) {
		errs = append(errs, fmt.Errorf("video profile %q encoder_element is invalid", name))
	}
	if len(profile.Bitrate) == 0 {
		errs = append(errs, fmt.Errorf("video profile %q requires at least one live bitrate property", name))
	}

	data := profileTemplateData{
		Width:            quality.Width,
		Height:           quality.Height,
		Framerate:        quality.Framerate,
		BitrateKbps:      quality.BitrateKbps,
		Threads:          tuning.Threads,
		KeyframeInterval: tuning.KeyframeInterval,
		VP8CPUUsed:       tuning.VP8CPUUsed,
		prefix:           "profile-validation",
	}
	if _, err := renderProfileTemplate(name+" pipeline", profile.Pipeline, data); err != nil {
		errs = append(errs, err)
	}
	for index, property := range profile.Bitrate {
		if !profileIdentifierPattern.MatchString(property.Element) {
			errs = append(errs, fmt.Errorf("video profile %q bitrate[%d] element is invalid", name, index))
		}
		if property.Property == "" {
			errs = append(errs, fmt.Errorf("video profile %q bitrate[%d] property is required", name, index))
		}
		if _, err := property.Render(name, data); err != nil {
			errs = append(errs, fmt.Errorf("video profile %q bitrate[%d]: %w", name, index, err))
		}
	}
	if err := profile.Codec.Validate(name); err != nil {
		errs = append(errs, err)
	}
	for _, limit := range []struct {
		name  string
		value int
	}{
		{name: "max_bitrate_kbps", value: profile.Limits.MaxBitrateKbps},
		{name: "max_macroblocks_per_dimension", value: profile.Limits.MaxMacroblocksPerDimension},
		{name: "max_macroblocks_per_frame", value: profile.Limits.MaxMacroblocksPerFrame},
		{name: "max_macroblocks_per_second", value: profile.Limits.MaxMacroblocksPerSecond},
	} {
		if limit.value < 0 {
			errs = append(errs, fmt.Errorf("video profile %q limits.%s must not be negative", name, limit.name))
		}
	}
	return errors.Join(errs...)
}

// RenderPipeline resolves the profile pipeline for one quality setting.
func (profile EncoderProfile) RenderPipeline(name, prefix string, quality Quality, tuning Tuning) (string, error) {
	return renderProfileTemplate(name+" pipeline", profile.Pipeline, profileTemplateData{
		Width:            quality.Width,
		Height:           quality.Height,
		Framerate:        quality.Framerate,
		BitrateKbps:      quality.BitrateKbps,
		Threads:          tuning.Threads,
		KeyframeInterval: tuning.KeyframeInterval,
		VP8CPUUsed:       tuning.VP8CPUUsed,
		prefix:           prefix,
	})
}

// RenderBitrate resolves the configured live property values.
func (profile EncoderProfile) RenderBitrate(name, prefix string, quality Quality, tuning Tuning) ([]any, error) {
	data := profileTemplateData{
		Width:            quality.Width,
		Height:           quality.Height,
		Framerate:        quality.Framerate,
		BitrateKbps:      quality.BitrateKbps,
		Threads:          tuning.Threads,
		KeyframeInterval: tuning.KeyframeInterval,
		VP8CPUUsed:       tuning.VP8CPUUsed,
		prefix:           prefix,
	}
	values := make([]any, len(profile.Bitrate))
	for index, property := range profile.Bitrate {
		value, err := property.Render(name, data)
		if err != nil {
			return nil, fmt.Errorf("render video profile %q bitrate[%d]: %w", name, index, err)
		}
		values[index] = value
	}
	return values, nil
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
		otherValue, exists := other.SDP.OfferFmtp[key]
		if !exists || otherValue != value {
			return false
		}
	}
	for key, value := range codec.SDP.AnswerFmtp {
		otherValue, exists := other.SDP.AnswerFmtp[key]
		if !exists || otherValue != value {
			return false
		}
	}
	return true
}

// Validate checks codec and SDP metadata used by WebRTC.
func (codec RTPCodec) Validate(profileName string) error {
	var errs []error
	if !profileIdentifierPattern.MatchString(codec.ID) {
		errs = append(errs, fmt.Errorf("video profile %q codec.id is invalid", profileName))
	}
	if !strings.HasPrefix(codec.MimeType, "video/") || len(codec.MimeType) == len("video/") {
		errs = append(errs, fmt.Errorf("video profile %q codec.mime_type must start with video/", profileName))
	}
	if codec.ClockRate == 0 {
		errs = append(errs, fmt.Errorf("video profile %q codec.clock_rate must be positive", profileName))
	}
	if codec.PayloadType < 96 || codec.PayloadType > 127 {
		errs = append(errs, fmt.Errorf("video profile %q codec.payload_type must be between 96 and 127", profileName))
	}
	switch codec.Payloader {
	case PayloaderVP8, PayloaderH264:
	default:
		errs = append(errs, fmt.Errorf("video profile %q codec.payloader must be vp8 or h264", profileName))
	}
	for index, feedback := range codec.RTCPFeedback {
		if feedback.Type == "" {
			errs = append(errs, fmt.Errorf("video profile %q codec.rtcp_feedback[%d].type is required", profileName, index))
		}
	}
	for key, pattern := range codec.SDP.OfferFmtp {
		if key == "" {
			errs = append(errs, fmt.Errorf("video profile %q codec.sdp.offer_fmtp contains an empty parameter", profileName))
			continue
		}
		if _, err := regexp.Compile(pattern); err != nil {
			errs = append(errs, fmt.Errorf("video profile %q codec.sdp.offer_fmtp[%q]: %w", profileName, key, err))
		}
	}
	for key, value := range codec.SDP.AnswerFmtp {
		if key == "" || value == "" {
			errs = append(errs, fmt.Errorf("video profile %q codec.sdp.answer_fmtp keys and values must not be empty", profileName))
		}
	}
	return errors.Join(errs...)
}

func (property EncoderProperty) Render(profileName string, data profileTemplateData) (any, error) {
	value, err := renderProfileTemplate(profileName+" bitrate property", property.Value, data)
	if err != nil {
		return nil, err
	}
	value = strings.TrimSpace(value)
	switch property.Type {
	case PropertyTypeInt:
		parsed, err := strconv.ParseInt(value, 10, 0)
		if err != nil {
			return nil, fmt.Errorf("parse int value %q: %w", value, err)
		}
		return int(parsed), nil
	case PropertyTypeUint:
		parsed, err := strconv.ParseUint(value, 10, 0)
		if err != nil {
			return nil, fmt.Errorf("parse uint value %q: %w", value, err)
		}
		return uint(parsed), nil
	default:
		return nil, fmt.Errorf("unsupported property type %q", property.Type)
	}
}

func renderProfileTemplate(name, source string, data profileTemplateData) (string, error) {
	tmpl, err := template.New(name).Option("missingkey=error").Funcs(template.FuncMap{
		"ceilDiv": func(value, divisor int) (int, error) {
			if divisor == 0 {
				return 0, errors.New("ceilDiv divisor must not be zero")
			}
			return (value + divisor - 1) / divisor, nil
		},
		"element": func(element string) string {
			return data.prefix + "-" + element
		},
		"mul": func(left, right int) int {
			return left * right
		},
	}).Parse(source)
	if err != nil {
		return "", fmt.Errorf("parse %s: %w", name, err)
	}
	var rendered bytes.Buffer
	if err := tmpl.Execute(&rendered, data); err != nil {
		return "", fmt.Errorf("execute %s: %w", name, err)
	}
	return rendered.String(), nil
}
